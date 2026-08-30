package email

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/files"

	"github.com/emersion/go-imap/v2"
)

// Source implements contract.Source for one email account.
//
// Multi-account is N Source instances, each with its own SourceID, IMAP
// loop, and SMTP send queue.
//
// Access control (allow/deny/admin) is the gate's job — the inbound flow
// screens senders above this layer. This source captures inbound mail
// unconditionally and lets the gate reject; it sets ev.IsAdmin = false at
// ingress and not Trusted, so the central inbound gate screens it
// and stamps admin (email has no native sender-screen phase of its own).
type Source struct {
	// cfg is the immutable connection config the source was built with — set once at
	// New and never mutated (a host/port change needs a restart, same as telegram's
	// token). Read via &s.cfg where a *EmailConfig is wanted. (Was an atomic.Pointer
	// mirroring a v1 reload hook that never landed; plain field now that there's no
	// reload path to write it.)
	cfg EmailConfig

	// password is read from the configured env at New time and is never
	// written to logs (every log call uses redact()).
	password string

	// files captures attachments. nil → don't capture.
	files *files.Store

	// out is the inbound event channel — written by the IMAP poll loop,
	// set by Run.
	out chan<- contract.MessageEvent

	// Send queue — bounded; the SMTP send worker drains it. See
	// 25-sendqueue.go.
	sendCh chan sendReq

	// FIFO ring buffer bounding the recent-Message-ID set so a server that
	// misbehaves on \Seen flagging (or our own mark_seen=false choice)
	// doesn't re-emit the same message forever. Re-seen IDs return early
	// without bumping their queue position, so eviction is purely
	// insertion-order. 10K entries × ~70 bytes = ~700KB cap. pollOnce clamps
	// each poll's unseen set to dedupCap so the ring always covers the whole
	// live set — without the clamp, unseen > cap evicts still-live ids and
	// re-emits the entire backlog every poll.
	dedupMu   sync.Mutex
	dedupRing *list.List
	dedupSet  map[string]struct{}

	// fetchFail counts CONSECUTIVE transient (transport/server) fetch failures
	// per uid. A transient failure leaves the message UNSEEN so a reconnect
	// re-fetches it; this bounds that retry so a server that persistently NOs a
	// single uid (a poison message) eventually gets \Seen'd to break the
	// per-poll re-fetch loop, instead of being retried forever. Content errors
	// (malformed MIME, no From) mark \Seen immediately and never land here.
	// Cleared on the uid's first success. Bounded by the live unseen set
	// (clampUIDs caps it at dedupCap).
	failMu    sync.Mutex
	fetchFail map[imap.UID]int

	// inboxCtx caches the latest inbound message per (canonical sender,
	// alias) so the agent's reply can build a proper In-Reply-To /
	// References chain + minimal quote without re-parsing the MIME.
	// Bounded at 1024 entries — eviction is best-effort.
	inboxMu  sync.Mutex
	inboxCtx map[string]inboundContext

	// priorBody stashes the last-outbound body keyed by (canonical sender,
	// alias). Read by the chunked-substring quote stripper to match the
	// next inbound against bob's previous reply, which tolerates line-wrap
	// / punctuation tweaks the regex path can't. Bounded by random
	// eviction at cap priorBodyCap (no insertion-order tracking — an
	// arbitrary entry is dropped when full); lost on restart — first
	// inbound per sender post-restart falls back to regex strip. (D-B5)
	priorMu   sync.Mutex
	priorBody map[string]string

	// coalescer holds + combines a session's per-turn replies into one mail
	// (batch_window_seconds). nil when reply-coalescing is off. See
	// 75-coalesce.go.
	coalescer *batchCoalescer

	// outbound is the file-backed recovery log for pending outbound mail
	// (durability between enqueue and SMTP-accept). nil when there is no file
	// store (degrades to at-most-once). See 26-outbound.go.
	outbound *outboundLog

	// adminLine lazily resolves the optional admin-summon funnel so the source
	// can alert on permanently-undeliverable outbound mail (SMTP down past the
	// retry budget, or the send queue full). Wired by stoma at Start; nil until
	// then, and a nil line is a no-op. Mirrors stoma's own lazy adminLine.
	adminLine func() contract.AdminLine

	closed atomic.Bool
}

const (
	// dedupCap bounds the FIFO dedup set of recently-seen Message-IDs.
	dedupCap = 10000

	// sendQueueSize bounds the outbound SMTP queue per account. Cap 50 —
	// anything beyond is back-pressure to the agent (queue full → Send
	// returns an error, agent sees it).
	sendQueueSize = 50

	// imapForcedReconnect is the periodic "drop the connection and LOGIN
	// fresh" interval. Mitigates long-conn buffer accumulation and Gmail's
	// IMAP idle 29-min timeout.
	imapForcedReconnect = 1 * time.Hour

	// priorBodyCap bounds the per-sender prior-outbound stash used by the
	// chunked-substring quote stripper. (D-B5)
	priorBodyCap = 1000
)

// New builds an email Source for one account. The password is read from
// cfg.PassEnv (which must resolve to a non-empty env var). fileStore may
// be nil to disable attachment capture.
//
// The signature is (cfg EmailConfig, fileStore *files.Store) — the
// integrator wires cfg; there is no access config (the gate owns that).
func New(cfg EmailConfig, fileStore *files.Store) (*Source, error) {
	cfg.applyDefaults()
	if cfg.SourceID == "" {
		return nil, fmt.Errorf("email: source_id is required")
	}
	if cfg.Address == "" {
		return nil, fmt.Errorf("email %q: address is required", cfg.SourceID)
	}
	if cfg.IMAP.Host == "" {
		return nil, fmt.Errorf("email %q: imap.host is required", cfg.SourceID)
	}
	if cfg.SMTP.Host == "" {
		return nil, fmt.Errorf("email %q: smtp.host is required", cfg.SourceID)
	}
	if cfg.PassEnv == "" {
		return nil, fmt.Errorf("email %q: PassEnv is required (name of env var holding the IMAP/SMTP password)", cfg.SourceID)
	}
	if msg := validateSecurity("imap", cfg.IMAP.Security); msg != "" {
		return nil, fmt.Errorf("email %q: %s", cfg.SourceID, msg)
	}
	if msg := validateSecurity("smtp", cfg.SMTP.Security); msg != "" {
		return nil, fmt.Errorf("email %q: %s", cfg.SourceID, msg)
	}
	password := os.Getenv(cfg.PassEnv)
	if password == "" {
		return nil, fmt.Errorf("email %q: env %s is empty", cfg.SourceID, cfg.PassEnv)
	}
	s := &Source{
		password:  password,
		files:     fileStore,
		sendCh:    make(chan sendReq, sendQueueSize),
		dedupRing: list.New(),
		dedupSet:  make(map[string]struct{}, dedupCap),
		fetchFail: make(map[imap.UID]int),
	}
	s.cfg = cfg
	// Reply-coalescing: hold a session's per-turn replies + combine into one
	// mail. The buffer is persisted to files under a sibling of the attachment
	// sandbox ($BOB_HOME/email-batch/<source>/) so a restart mid-window doesn't
	// lose a computed-but-unsent reply. Needs a writable home: without fileStore
	// we have no base path, so coalescing stays off (each turn replies
	// immediately) rather than buffering only in memory.
	if cfg.BatchWindowSeconds > 0 {
		if fileStore != nil {
			dir := filepath.Join(filepath.Dir(fileStore.Base()), "email-batch", cfg.SourceID)
			s.coalescer = newBatchCoalescer(s, dir,
				time.Duration(cfg.BatchWindowSeconds)*time.Second, cfg.BatchMaxMessages)
		} else {
			slog.Warn("email: batch_window_seconds set but no file store — coalescing disabled",
				"source", cfg.SourceID)
		}
	}
	// Outbound recovery log: durability for pending replies between enqueue and
	// SMTP-accept (F24). Sibling of the batch dir under the file-store home; no
	// file store → no persistence (at-most-once, as before).
	if fileStore != nil {
		s.outbound = newOutboundLog(s, filepath.Join(filepath.Dir(fileStore.Base()), "email-outbound", cfg.SourceID))
	}
	return s, nil
}

// SetAdminLine wires the lazy admin-line accessor so the source can summon the
// operator on undeliverable outbound mail. stoma calls this at Start; the
// adminline module resolves AFTER the source bus (it Needs the Gateway the bus
// provides), hence a lazy func rather than a resolved value.
func (s *Source) SetAdminLine(get func() contract.AdminLine) { s.adminLine = get }

// Name returns the per-account source id (e.g. "email-support") — the source's
// identity and the scope prefix routing keys off (no "email" prefix required).
func (s *Source) Name() string { return s.cfg.SourceID }

// Caps: email supports replying (RFC In-Reply-To / References chain) but
// not editing, reactions, or buttons. NOT Trusted (screened by default) — email
// screens nothing in its own protocol phase, so it relies ENTIRELY on the central
// inbound gate for access control + admin stamping. Marking it Trusted would skip
// screening and let any sender reach a turn unauthenticated (and permanently reject
// admin slash commands) — which is exactly why the default is fail-closed.
func (s *Source) Caps() contract.Caps {
	return contract.Caps{ReplyTo: true, RequiredModelTags: s.cfg.RequiredModelTags} // screened by default (not Trusted)
}

// HealthCheck: best-effort liveness probe. Verifies the IMAP loop is
// running (closed flag unset) and the SMTP password is loaded — a real
// round-trip would cost a TLS handshake per probe, heavier than the value
// (the poll loop is itself a liveness signal).
func (s *Source) HealthCheck(ctx context.Context) error {
	if s.closed.Load() {
		return fmt.Errorf("email %s: source closed", s.Name())
	}
	// Defensive: New guarantees a non-empty password and the field is
	// set-once, so this is unreachable in production — kept as a cheap
	// struct-invariant probe (same posture as redact's guard).
	if s.password == "" {
		return fmt.Errorf("email %s: password not loaded", s.Name())
	}
	return nil
}

// SendButtons is unsupported on email — no inline buttons on the wire.
// Degrades to a plain text Send (mirrors telegram's deferred-callback
// path), so the gateway's /approve text flow still reaches the user.
func (s *Source) SendButtons(ctx context.Context, t contract.Target, text string, _ []contract.Button) (string, error) {
	return "", s.Send(ctx, t, text)
}

// Run is the per-account lifecycle. Spawns the SMTP send-worker goroutine,
// then runs the IMAP poll loop until ctx is cancelled. Both exit when ctx
// fires; a WaitGroup keeps teardown deterministic.
//
// Serially re-invocable — the bus retries a boot-window exit (leaf/stoma
// runSource) on this SAME instance, so s.out is rebound each run. The
// unlocked write is safe because retries are serial and both worker
// goroutines are joined (wg.Wait) before Run returns.
func (s *Source) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	// Reset the closed latch like telegram/discord/feishu: a prior run's exit
	// latched it, and "Run entry ⇒ serving" should hold locally rather than
	// lean on the poll loop having no pre-cancel exit path.
	s.closed.Store(false)
	s.out = out
	cfg := &s.cfg
	slog.Info("email source starting",
		"source", cfg.SourceID, "address", cfg.Address,
		"imap", fmt.Sprintf("%s:%d", cfg.IMAP.Host, cfg.IMAP.Port),
		"smtp", fmt.Sprintf("%s:%d", cfg.SMTP.Host, cfg.SMTP.Port),
		"poll_s", cfg.PollIntervalSeconds, "folder", cfg.Folder)

	// Re-load any reply-batch buffers persisted before the last shutdown and
	// re-arm their flush timers, so a restart mid-window doesn't lose them.
	if s.coalescer != nil {
		s.coalescer.rehydrate(ctx)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.runSendWorker(ctx) }()

	// Replay any outbound mail that a prior process enqueued but never got an
	// SMTP-accept for (crash / restart / permanent failure within the TTL). The
	// send worker is up (above), so re-enqueued rows drain immediately.
	if s.outbound != nil {
		s.outbound.replay(ctx)
	}

	// IMAP poll loop runs in-line (this goroutine) — its return is the
	// source's return.
	s.runPollLoop(ctx)

	s.closed.Store(true)
	wg.Wait()
	return ctx.Err()
}

// markSeenAndCheck reports whether msgID has already been processed (true)
// and otherwise records it as seen. Bounded FIFO ring — when full, evicts
// the oldest insertion. Pure in-memory; the persistent fallback is the IMAP
// \Seen flag itself.
func (s *Source) markSeenAndCheck(msgID string) bool {
	if msgID == "" {
		return false // can't dedup without an id — let it through
	}
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	if _, ok := s.dedupSet[msgID]; ok {
		return true
	}
	s.dedupSet[msgID] = struct{}{}
	s.dedupRing.PushBack(msgID)
	if s.dedupRing.Len() > dedupCap {
		front := s.dedupRing.Front()
		if front != nil {
			if old, ok := front.Value.(string); ok {
				delete(s.dedupSet, old)
			}
			s.dedupRing.Remove(front)
		}
	}
	return false
}

// dedupContains is the read-only sibling of markSeenAndCheck — used by the
// two-stage fetch's screen pass so the stage-1 cheap check doesn't commit
// the id (the full-parse path does the commit via markSeenAndCheck on the
// canonical id, which may differ from the header-only one).
func (s *Source) dedupContains(msgID string) bool {
	if msgID == "" {
		return false
	}
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	_, ok := s.dedupSet[msgID]
	return ok
}

// redact returns a string safe to log — replaces the password if present
// anywhere in the input. Cheap defence: the IMAP / SMTP stdlib wrappers
// sometimes echo URLs / auth strings in error messages.
func (s *Source) redact(in string) string {
	if s.password == "" || in == "" {
		return in
	}
	return strings.ReplaceAll(in, s.password, "<redacted>")
}

var _ contract.Source = (*Source)(nil)
