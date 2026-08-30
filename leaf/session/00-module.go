package session

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/session/messages"
	"agentbob/leaf/session/store"
	"agentbob/trunk"
)

// Stale-pending housekeeping (B-31): a pending_inbound row is normally drained
// within a turn; one older than a day is crash residue a boot-time clear missed
// (a failed delete, or a boot cut short). Swept periodically as a backstop.
const (
	stalePendingAge         = 24 * time.Hour
	stalePendingSweepPeriod = 6 * time.Hour
	// messageIndexAge bounds reply-routing rows (B-4/S-3). It MUST cover the
	// cold-archive revive window: a reply to a was-alive cold session revives it
	// (resolver ensureLive → Archiver.Restore), and that lookup is keyed on the
	// index row — pruning it before the archive is purged makes revive dead-on-arrival
	// (the reply misses the index, opens a fresh empty session). The index row is keyed
	// on the bot message's SEND ts, but the archive is purged at archived_at +
	// archiveHardTTL with archived_at ≈ activity + archiveColdAge — so the index must
	// outlive the message ts by archiveColdAge + archiveHardTTL to stay aligned with
	// the archive's purge, not just archiveHardTTL (which pruned ~archiveColdAge early).
	// (declared below)
	messageIndexAge         = archiveColdAge + archiveHardTTL
	messageIndexSweepPeriod = 12 * time.Hour

	// Cold-session archival (docs/wip-message-ownership.md §6 / loop-core-spec §11.8).
	// archiveColdAge: a session whose last activity is older than this is serialized
	// to the archive blob table and dropped from the hot tables (dead or alive). A
	// was-alive archive is revivable by a later reply; reply index rows are kept so
	// it can be located. NOTE: the retention window is hardcoded 30d here. The
	// "retention_days=0 → archiving off" config knob is DEFERRED — a Go int zero-value
	// collides with "off", so it needs a sentinel (e.g. *int / -1) to distinguish
	// "unset" from "disabled"; not built in this slice.
	archiveColdAge     = 30 * 24 * time.Hour
	archiveColdPeriod  = 12 * time.Hour
	archiveHardTTL     = 365 * 24 * time.Hour // archive rows older than this are hard-purged
	archivePurgePeriod = 24 * time.Hour

	// Compacted-row prune (A5): in-place compaction appends a summary marker +
	// replay tail and leaves the pre-marker originals in bob_messages — dead for
	// replay (GetReplay anchors on the last marker) but unbounded for a long-lived,
	// repeatedly-compacting session. Prune them once past the same 30d retention
	// horizon as cold archival (archival itself only reaps IDLE sessions; this
	// covers the busy ones).
	compactedPruneAge    = 30 * 24 * time.Hour
	compactedPrunePeriod = 24 * time.Hour
)

// msgIndexer is session's contract.MessageIndexer: the flow records a delivered
// bot message through it; session owns the write (and the resolver's read).
type msgIndexer struct{ st *store.PG }

func (x msgIndexer) RecordSent(ctx context.Context, scope, msgID, sid string) {
	if err := x.st.RecordSent(ctx, scope, msgID, sid); err != nil {
		slog.Warn("session: record message index failed", "scope", scope, "msg", msgID, "err", err)
	}
}

// Module is the session core as a trunk module: it migrates its tables, builds
// the Manager over the shared DB and the (optional) turn handler, and provides
// contract.SessionManager.
type Module struct {
	config Config
	mgr    *Manager
	state  atomic.Int32 // trunk.State
}

// New returns a session module. Config knobs default when zero.
func New(cfg Config) *Module { return &Module{config: cfg} }

func (m *Module) Name() string { return "session" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.SessionManager](),
		trunk.TypeOf[contract.SessionResume](),
		trunk.TypeOf[contract.MessageIndexer](),
		trunk.TypeOf[contract.MessageStore](),
		trunk.TypeOf[contract.ChatHistory](),
	}
}

// Needs the shared DB and the source bus (Gateway) — the latter for the
// manager's pre-turn I/O (reactions, transient notices), resolved by source
// name. The turn handler is OPTIONAL (TryRequire) — a session is a session even
// with no turn module wired; until then a stub logs would-be turns — so it is
// NOT a hard Need.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.DB](),
		trunk.TypeOf[contract.Gateway](),
		trunk.TypeOf[contract.SlashRegistry](),
		trunk.TypeOf[contract.PanelRegistry](),
	}
}

// Optional is false: with no session core, nothing routes inbound to a turn.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Manager exposes the concrete manager as a TEST-ONLY seam (module tests drive
// sm.Manager().Wait() directly). Production code reaches the session core through
// its provided contracts (contract.SessionManager & co.) — the inbound flow calls
// RecoverPendingOnBoot via contract.SessionManager, never via *Module.
func (m *Module) Manager() *Manager { return m.mgr }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("session: migrate: %w", err)
	}
	// Conversation history (bob_messages) belongs to session now (it owns the
	// continuity). Migrate it (namespace "turn" preserved — see messages package
	// doc) and provide it as contract.MessageStore for the turn core to consume.
	if err := messages.Migrate(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("session: migrate messages: %w", err)
	}
	msgStore := messages.NewPG(db)
	trunk.Provide[contract.MessageStore](reg, msgStore)
	// Same *PG also serves the read-only, scope-addressed chat-log reader (the webui
	// agora chat browser via flow/agora). A separate narrow interface so the turn-hot
	// write path (MessageStore) doesn't widen.
	trunk.Provide[contract.ChatHistory](reg, msgStore)
	// Resolve the turn handler LAZILY (on first turn), not at Start: the turn
	// module is an optional collaborator the trunk doesn't order before session,
	// so it may register AFTER us — but by the first inbound (post-boot) it has.
	// Falls back to a logging stub if no turn module ever registers.
	gw := trunk.Require[contract.Gateway](reg)
	st := store.NewPG(db)
	// Ingestion voice-preprocess (F165): transcribe audio to text at submit so the
	// transcript lives in the message. The transcriber is an OPTIONAL collaborator (the
	// leaf/asr module over the model pool's KindASR backend), resolved lazily on first
	// use through the trunk — no direct import, and a no-ASR deployment simply never folds
	// a transcript. Keeps the session core decoupled (a contract interface, not the pool).
	if m.config.Transcribe == nil {
		var once sync.Once
		var tr contract.Transcriber
		m.config.Transcribe = func(ctx context.Context, ev contract.MessageEvent) (string, string) {
			once.Do(func() { tr, _ = trunk.TryRequire[contract.Transcriber](reg) })
			if tr == nil {
				return "", ""
			}
			return tr.Transcribe(ctx, ev)
		}
	}
	m.mgr = NewManager(st, newLazyTurnHandler(reg), gw.SourceByName, m.config)
	m.mgr.turnRef = lazyTurn(reg) // /session context gauge (read-only; turn starts later)
	trunk.Provide[contract.SessionManager](reg, m.mgr)
	trunk.Provide[contract.SessionResume](reg, m.mgr) // wake-a-session for the takeover hand-back
	// Cold-session archival lives entirely in the session module (history + the
	// lifecycle row are both session subpackages over the same DB). Wire the restore
	// hook unconditionally — a reply to a cold session must revive even in a minimal
	// config; only the periodic ARCHIVE/PURGE sweeps need the housekeeper.
	archiver := NewArchiver(db, st, msgStore)
	m.mgr.SetRestore(archiver.Restore)
	archiver.SetInFlight(m.mgr.SidInFlight) // cold sweep skips busy/closing sids (D18)
	// Backstop: reclaim crash-residual pending_inbound rows. A normal pending row is
	// drained within a turn; one surviving a day is a crash orphan a boot-time clear
	// missed (a failed delete, or a boot cut short). RecoverPendingOnBoot clears what it
	// can on boot, this sweeps the rest. Optional — a minimal config runs without a
	// housekeeper. (B-31)
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "session-stale-pending",
			Period: stalePendingSweepPeriod,
			Run: func(ctx context.Context) error {
				n, err := st.ClearStalePending(ctx, stalePendingAge.Seconds())
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("session: cleared stale pending rows", "removed", n)
				}
				return nil
			},
		})
		// Prune the reply-routing index (S-3): one row per delivered bot message
		// once RecordSent is wired (B-4), so it grows without a sweep. Drop rows
		// older than the session retention window — replying to continue a message
		// older than that opens a fresh session anyway (its session is archived).
		hk.Register(trunk.Task{
			Name:   "session-message-index-prune",
			Period: messageIndexSweepPeriod,
			Run: func(ctx context.Context) error {
				n, err := st.PruneMessageIndex(ctx, messageIndexAge.Seconds())
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("session: pruned message-index rows", "removed", n)
				}
				return nil
			},
		})
		// Archive cold sessions (§11.8): a session whose last activity is older than
		// archiveColdAge is serialized to the blob table and dropped from the hot
		// tables (dead or alive). The reply index is kept so a was-alive archive can
		// be revived by a later reply.
		hk.Register(trunk.Task{
			Name:   "session-archive-cold",
			Period: archiveColdPeriod,
			Run: func(ctx context.Context) error {
				cutoff := clock.UnixSeconds() - archiveColdAge.Seconds()
				n, err := archiver.ArchiveCold(ctx, cutoff)
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("session: archived cold sessions", "removed", n)
				}
				return nil
			},
		})
		// A5: prune rows superseded by an in-place compaction (before the session's
		// last summary marker AND older than the retention window). See the
		// compactedPruneAge comment above.
		hk.Register(trunk.Task{
			Name:   "session-compacted-prune",
			Period: compactedPrunePeriod,
			Run: func(ctx context.Context) error {
				n, err := msgStore.PruneCompacted(ctx, compactedPruneAge.Seconds())
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("session: pruned compacted message rows", "removed", n)
				}
				return nil
			},
		})
		// Second-level cleanup (§11.8): hard-purge archive rows past archiveHardTTL +
		// their reply-index rows — past the hard TTL a reply opens a fresh session.
		hk.Register(trunk.Task{
			Name:   "session-archive-purge",
			Period: archivePurgePeriod,
			Run: func(ctx context.Context) error {
				cutoff := clock.UnixSeconds() - archiveHardTTL.Seconds()
				n, err := archiver.PurgeOld(ctx, cutoff)
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("session: purged expired archive rows", "removed", n)
				}
				return nil
			},
		})
	}
	// Reply-routing write side (B-4): a flow records its delivered bot message
	// (scope + sink msg id → sid) here; the resolver's read side reconnects a later
	// reply. session owns the mapping; the flow drives it (it holds the sink/scope).
	trunk.Provide[contract.MessageIndexer](reg, msgIndexer{st: st})
	// Optional: let /whoami show the sender's bound account. Lazy — accounts isn't a
	// hard Need, so it may register after us.
	m.mgr.SetAccounts(lazyAccounts(reg))
	// Optional: agora group-router for virtual-group member routing. Lazy — agora isn't
	// a hard Need (absent on a non-agora deployment → groups route 1:1).
	m.mgr.SetGroupRouter(lazyGroupRouter(reg))
	// Register session's slash commands into the shared registry (the registry
	// holds the table; session holds the /new logic).
	slashReg := trunk.Require[contract.SlashRegistry](reg)
	slashReg.Register(contract.SlashCommand{
		Name:    "new",
		DescKey: "slash.new.desc",
		Handler: m.mgr.slashNew,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "session",
		DescKey: "slash.session.desc",
		Handler: m.mgr.slashSession,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "whoami",
		DescKey: "slash.whoami.desc",
		Handler: m.mgr.slashWhoami,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "close-session",
		DescKey: "slash.close_session.desc",
		Handler: m.mgr.slashCloseSession,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "trace",
		DescKey: "slash.trace.desc",
		Handler: m.mgr.slashTrace,
	})
	slashReg.Register(contract.SlashCommand{
		Name:      "looping",
		DescKey:   "slash.looping.desc",
		AdminOnly: true, // flips the scope's round budget 40 → 500; operator's call
		Handler:   m.mgr.slashLooping,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "uncensored",
		DescKey: "slash.uncensored.desc",
		Handler: m.mgr.slashUncensored,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "thinking",
		DescKey: "slash.thinking.desc",
		Handler: m.mgr.slashThinking,
	})
	slashReg.Register(contract.SlashCommand{
		Name:      "prompt",
		DescKey:   "slash.prompt.desc",
		AdminOnly: true,
		Handler:   m.mgr.slashPrompt,
	})
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// Stop drains in-flight turns.
func (m *Module) Stop(context.Context) error {
	if m.mgr != nil {
		m.mgr.Shutdown(0)
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// lazyTurnHandler resolves the real contract.TurnHandler from the registry on
// first use (caching it), or a stub if none registered. This decouples session
// from the turn module's start order — the arbiter holds a stable handler from
// Start, but the resolution happens at the first turn, after every module has
// started.
type lazyTurnHandler struct {
	reg  *trunk.Registry
	once sync.Once
	h    contract.TurnHandler
}

func newLazyTurnHandler(reg *trunk.Registry) *lazyTurnHandler { return &lazyTurnHandler{reg: reg} }

// lazyTurn resolves the turn core on first use (turn starts AFTER session — it
// Requires the MessageStore session provides). Absent → nil; /session then
// omits the context-gauge line.
func lazyTurn(reg *trunk.Registry) func() contract.Turn {
	var once sync.Once
	var t contract.Turn
	return func() contract.Turn {
		once.Do(func() { t, _ = trunk.TryRequire[contract.Turn](reg) })
		return t
	}
}

// lazyAccounts resolves the optional accounts authority on first use (accounts is
// not a hard Need, so it may Start after session). Absent → nil; /whoami then omits
// the account line.
func lazyAccounts(reg *trunk.Registry) func() contract.Accounts {
	var once sync.Once
	var acc contract.Accounts
	return func() contract.Accounts {
		once.Do(func() { acc, _ = trunk.TryRequire[contract.Accounts](reg) })
		return acc
	}
}

// lazyGroupRouter resolves the optional agora group-router once (agora satisfies
// contract.GroupRouter). Absent (no agora bootstrapped) → nil → groups route 1:1.
func lazyGroupRouter(reg *trunk.Registry) func() contract.GroupRouter {
	var once sync.Once
	var gr contract.GroupRouter
	return func() contract.GroupRouter {
		once.Do(func() {
			if ag, ok := trunk.TryRequire[contract.Agora](reg); ok && ag != nil {
				gr = ag
			}
		})
		return gr
	}
}

func (l *lazyTurnHandler) resolve() contract.TurnHandler {
	l.once.Do(func() {
		if h, ok := trunk.TryRequire[contract.TurnHandler](l.reg); ok {
			l.h = h
		} else {
			l.h = stubTurnHandler{}
		}
	})
	return l.h
}

func (l *lazyTurnHandler) RunTurn(ctx context.Context, work contract.TurnWork) {
	l.resolve().RunTurn(ctx, work)
}
func (l *lazyTurnHandler) Blocked(ctx context.Context, sid string) bool {
	return l.resolve().Blocked(ctx, sid)
}
func (l *lazyTurnHandler) NotePending(ctx context.Context, sid string, ev contract.MessageEvent, d contract.PendingDisposition) {
	l.resolve().NotePending(ctx, sid, ev, d)
}
func (l *lazyTurnHandler) DumpPrompt(ctx context.Context, work contract.TurnWork) string {
	return l.resolve().DumpPrompt(ctx, work)
}
func (l *lazyTurnHandler) FlowInfo(ctx context.Context, work contract.TurnWork) (string, contract.SessionMode, bool) {
	return l.resolve().FlowInfo(ctx, work)
}

// stubTurnHandler stands in when no turn module is wired: it logs a would-be
// turn and never blocks the arbiter, never reports a session blocked, and drops
// busy-arrived surfacing.
type stubTurnHandler struct{}

func (stubTurnHandler) RunTurn(_ context.Context, work contract.TurnWork) {
	slog.Info("session: would run turn (turn module not wired)", "sid", work.Sid, "scope", work.Scope, "events", len(work.Events))
}
func (stubTurnHandler) Blocked(context.Context, string) bool { return false }
func (stubTurnHandler) NotePending(context.Context, string, contract.MessageEvent, contract.PendingDisposition) {
}
func (stubTurnHandler) DumpPrompt(context.Context, contract.TurnWork) string { return "" }
func (stubTurnHandler) FlowInfo(context.Context, contract.TurnWork) (string, contract.SessionMode, bool) {
	return "", "", false
}

var _ trunk.Module = (*Module)(nil)
