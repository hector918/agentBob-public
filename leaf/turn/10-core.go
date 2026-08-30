package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
	"agentbob/i18n"
)

// errNilSink is returned when Run is handed a spec with no sink to render to.
var errNilSink = errors.New("turn: nil sink")

// loopCap is the absolute per-turn round limit (spec §10). A var only so tests
// can lower it; production never reassigns.
var loopCap = 40

// historyReplayCap bounds the pre-compaction replay (spec §10) — once a compaction
// has run, GetReplay anchors on the summary marker instead and this is moot.
const historyReplayCap = 500

// The turn core's user-facing notices are i18n keys (rendered via i18n.T with
// turnLang(spec)) — spec.Lang carries the resolved recipient language (Detect fallback),
// hardcode one.
const (
	// modelUnavailableKey is delivered when the pool can't serve a round.
	modelUnavailableKey = "system.turn.model_unavailable"
	// contextTooLongKey is delivered when the history is too large for the model's
	// context window AND compaction can't shrink it further (the recent tail alone
	// overflows) — an honest "start a new session" line instead of a misleading
	// model-unavailable notice.
	contextTooLongKey = "system.turn.context_too_long"
	// salvageStockKey is the §6.7 tier-3 stock floor — the last-resort fixed line when
	// even the no-LLM action brief has no tools to name. Keeps I8: the user gets
	// exactly one finish even when a turn ends without a model reply.
	salvageStockKey = "system.turn.salvage_stock"
	// persistFailKey ends a turn whose assistant-calls row failed to persist
	// (I2): we can't safely dispatch, so the user gets one readable line.
	persistFailKey = "system.turn.persist_fail"
	// internalErrorKey is the turn-level panic backstop (I16 / §4.1 step 1) — the
	// user never gets silence on an uncaught panic.
	internalErrorKey = "system.turn.internal_error"
	// cancelledMarkerKey is appended (§4.3) when a turn is cancelled mid-stream AFTER it
	// already streamed visible content: the partial was shown live, so it is persisted
	// + finalized with this honest cut-here marker (NOT an apology — I8). A cancel with
	// nothing shown yet stays fully silent (the loop-top cancel door).
	cancelledMarkerKey = "system.turn.cancelled_marker"
	// streamInterruptedMarkerKey is appended (§4.2) when the stream BREAKS after visible
	// content was already shown: the partial is preserved + marked rather than clobbered
	// by the model-unavailable notice (Finish replaces the message window, so the notice
	// would erase the half-answer the user saw). A break with nothing shown still gets
	// the notice.
	streamInterruptedMarkerKey = "system.turn.stream_interrupted_marker"
)

// turnLang is the turn's reply language for a core-rendered notice: the flow's
// resolved spec.Lang (the ingress cascade — admin override → detect → per-sender
// memory) when set, else a per-reply Detect of the user input. The core must not grow
// a new contract field for it; the text the user just sent is the honest signal
// we already carry. "" (no script signal — a sub-turn / internal wake) falls
// through to the catalog default (English).
func turnLang(spec contract.TurnSpec) string {
	if spec.Lang != "" {
		return spec.Lang
	}
	return i18n.Detect(spec.UserText)
}

// core is the contract.Turn: the turn driver over a model pool + history store.
// The store is session's contract.MessageStore — turn reads the tail and appends
// its produced rows; it does not own the table.
type core struct {
	store  contract.MessageStore
	pool   contract.ModelPool
	subSeq atomic.Int64 // monotonic child-sid counter for delegated sub-turns (§7)

	// weighDownUntil is the 称重制 tokenizer breaker (88-weigh.go): after a
	// failed tokenize the estimator serves until this instant, so a hung
	// endpoint costs one timeout per cooldown window instead of one per text
	// per loop-top. The exact-count CACHE lives pool-side (it alone knows
	// which tokenizer measured a text).
	weighMu        sync.Mutex
	weighDownUntil time.Time

	// promptFloor caches the per-sid context gauge: the last REAL prompt-token
	// count the pool reported (resp.Usage.InputTokens) — the provider-agnostic
	// truth floor for the compaction trigger, naturally including tool specs
	// and chat-template overhead the loop-top weigh can't see, and the ONLY
	// real reading on pools without a tokenize path — plus the last sizing
	// budget (the static winner's window), recorded at loop-top so read-only
	// panels (/session, via ContextGauge) can show "吃了多少 / 锅多大" with
	// zero store reads. tokens is cleared after a compaction rewrites history
	// (the reading describes a window that no longer exists); budget survives
	// (it describes the mouth, not the meal). Process-lifetime ephemeral.
	floorMu     sync.Mutex
	promptFloor map[string]ctxGauge
}

// ctxGauge is one sid's in-memory context reading: 0 = unknown (fresh boot /
// just compacted / never sized).
type ctxGauge struct {
	tokens int // last REAL prompt size the model reported
	budget int // last sizing budget (static winner's declared window)
	rows   int // messages in the last replay window (post-compaction count)
}

// promptFloorCap bounds the per-sid floor map — a HYGIENE bound: on overflow
// the map is cleared and live sessions re-learn from resp.Usage next round.
const promptFloorCap = 8192

// recordPromptFloor stores the real prompt-token count the pool reported for
// sid. n<=0 is ignored (an errored / aborted call reports no usage).
func (c *core) recordPromptFloor(sid string, n int) {
	if n <= 0 {
		return
	}
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	g := c.gaugeLocked(sid)
	g.tokens = n
	c.promptFloor[sid] = g
}

// recordCtxBudget stores the sid's last sizing budget (loop-top, 90-compact).
func (c *core) recordCtxBudget(sid string, budget int) {
	if budget <= 0 {
		return
	}
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	g := c.gaugeLocked(sid)
	g.budget = budget
	c.promptFloor[sid] = g
}

// recordCtxRows stores the sid's last replay message count (buildMessages, the
// post-compaction window the model actually sees). n<0 ignored; 0 is a valid
// fresh session so it is stored.
func (c *core) recordCtxRows(sid string, n int) {
	if n < 0 {
		return
	}
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	g := c.gaugeLocked(sid)
	g.rows = n
	c.promptFloor[sid] = g
}

// gaugeLocked returns the sid's gauge, applying the hygiene cap on first
// insert. Caller holds floorMu.
func (c *core) gaugeLocked(sid string) ctxGauge {
	if c.promptFloor == nil {
		c.promptFloor = make(map[string]ctxGauge)
	}
	if _, exists := c.promptFloor[sid]; !exists && len(c.promptFloor) >= promptFloorCap {
		c.promptFloor = make(map[string]ctxGauge)
	}
	return c.promptFloor[sid]
}

// ContextGauge is the read-only panel view (/session, via a soft seam on
// contract.Turn): the sid's last REAL prompt size, last sizing budget, and last
// replay message count (post-compaction), 0 = unknown. One mutex + map read —
// no store, no pool.
func (c *core) ContextGauge(sid string) (lastTokens, budget, rows int) {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	g := c.promptFloor[sid]
	return g.tokens, g.budget, g.rows
}

// lastPromptFloor returns the sid's last real prompt-token reading, 0 if none.
func (c *core) lastPromptFloor(sid string) int {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	return c.promptFloor[sid].tokens
}

// clearPromptFloor zeroes the sid's tokens reading after an in-place
// compaction — the old reading describes the pre-compaction window and would
// re-trigger compaction every turn until a fresh round re-learns. The budget
// half survives: it describes the mouth, not the meal.
func (c *core) clearPromptFloor(sid string) {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	if g, ok := c.promptFloor[sid]; ok {
		g.tokens = 0
		c.promptFloor[sid] = g
	}
}

// Run is the turn core's shared shell (spec §4.1): the prelude (nil-sink check,
// panic backstop, persist the user rows), a one-shot Mode dispatch to the loop
// driver (docs/turn-driver-split.md — round iteration and exit semantics live in
// the driver, 12-driver-*.go), and the shared teardown every driver falls back
// to when its loop ends without an in-loop door (iter-cap default → cancel door
// → salvage).
func (c *core) Run(ctx context.Context, spec contract.TurnSpec) (res contract.TurnResult) {
	// A turn with no sink can't deliver a reply — surface a contained error
	// rather than nil-panic deep in the round (the flow composes the spec).
	if spec.Sink == nil {
		slog.Error("turn: nil sink — cannot run", "sid", spec.Sid)
		return contract.TurnResult{Err: errNilSink, Outcome: contract.OutcomeError}
	}
	// I16 / §4.1 step 1: an uncaught panic anywhere in the round path must not leave
	// the user in silence — log with stack, finish with a notice, return the error.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("turn: panicked", "sid", spec.Sid, "panic", r, "stack", string(debug.Stack()))
			if ferr := spec.Sink.Finish(i18n.T(internalErrorKey, turnLang(spec))); ferr != nil {
				slog.Warn("turn: sink finish (panic) failed", "sid", spec.Sid, "err", ferr)
			}
			res = contract.TurnResult{Err: fmt.Errorf("turn: panicked: %v", r), Outcome: contract.OutcomeError}
		}
	}()
	// Persist the user row FIRST (I10): it survives a model failure, and every
	// round reads it back from history rather than carrying it out-of-band. So if
	// THIS write fails, the model's message list (built entirely from history) would
	// never include what the user just asked — it would answer against stale history,
	// silently. Abort with an honest notice instead of mis-answering.
	if err := c.persistUserRows(ctx, spec); err != nil {
		// I8/I14: an entry ctx already cancelled (/stop racing ahead, system stop, or
		// a parent turn's cancel propagating to a sub) makes database/sql return
		// context.Canceled before executing — that is NOT a storage error. Exit
		// silently (no notice) like the loop's cancel door below — but still release
		// the sink (Finish("")) so the coalescer's per-turn counter is balanced.
		if ctx.Err() != nil {
			c.releaseSink(spec)
			return contract.TurnResult{Err: ctx.Err(), Outcome: contract.OutcomeCancelled}
		}
		slog.Warn("turn: persist user message failed — aborting round", "sid", spec.Sid, "err", err)
		if ferr := spec.Sink.Finish(i18n.T(persistFailKey, turnLang(spec))); ferr != nil {
			slog.Warn("turn: sink finish (persist-fail) failed", "sid", spec.Sid, "err", ferr)
		}
		return contract.TurnResult{Err: fmt.Errorf("turn: persist user message: %w", err), Outcome: contract.OutcomeError}
	}

	st := newTurnState()
	// Driver dispatch (docs/turn-driver-split.md §1): the ONLY read of spec.Mode in
	// the core. The done=false contract lives on the drivers (their doc comments
	// own it); an unknown mode deliberately drives regular — fail-safe, never a
	// half-built path.
	var (
		out  contract.TurnResult
		done bool
	)
	switch spec.Mode {
	case contract.TurnModeLooping:
		out, done = c.driveLooping(ctx, spec, st)
	default:
		out, done = c.driveRegular(ctx, spec, st)
	}
	if done {
		return out
	}
	if st.exit == nil {
		st.exit = &turnExit{state: exitIterCap}
	}
	// A cancel exits silently (I8/I14): no apology, no content — the caller sees the
	// cancel error and the durable pending row drives any re-send. The sink is still
	// released (Finish("")) so the coalescer's per-turn counter is balanced; an empty
	// finish shows nothing (streamsink skips it) and no-ops the agora return sink.
	if st.exit.state == exitCancelled {
		c.releaseSink(spec)
		// exitCancelled is only ever set at the loop top under ctx.Err() != nil, and a
		// context error is sticky — so ctx.Err() is always non-nil here. (A zero-value
		// turnExit can't land here either: exitUnset holds the 0 slot, see 30-state.go.)
		return contract.TurnResult{Err: ctx.Err(), Outcome: contract.OutcomeCancelled}
	}
	return c.salvage(spec, st) // §6.7 ladder (50-salvage.go)
}

// persistUserRows writes the turn's user input to history BEFORE the model call (I10).
// One row PER inbound speaker (spec.UserMsgs), each tagged with its sender, so a group's
// speakers stay individually attributed. A sub-turn / non-event wake (no UserMsgs) falls
// back to one unattributed row from the joined UserText.
func (c *core) persistUserRows(ctx context.Context, spec contract.TurnSpec) error {
	if len(spec.UserMsgs) > 0 {
		// Atomic batch: all speaker rows commit together, so a mid-batch failure + retry
		// can't leave a duplicated already-committed speaker row.
		return c.store.AppendUserMsgs(ctx, spec.Sid, spec.UserMsgs)
	}
	return c.store.AppendMessage(ctx, spec.Sid, "user", spec.UserText, contract.MsgAuthor{})
}

// buildMessages assembles one round's message list: the system prompt (re-Built
// from the flow's builder, so a later round renders fresh) followed by the
// session history (which already includes the user row, persisted before the
// call, and later the round's tool rows). This is the per-round rebuild seam
// (spec §4.2 step 1); the round loop calls it each iteration.
//
// replay, when non-nil, is a current GetReplay result the caller already holds
// (the loop-top compaction read) — reused so one round issues ONE history read.
// It is consumed destructively (the cleanse/speaker/orphan passes mutate it in
// place); nil → read fresh here.
func (c *core) buildMessages(ctx context.Context, spec contract.TurnSpec, replay []contract.Message) ([]contract.Message, error) {
	history := replay
	if history == nil {
		// §6.9 replay rule: GetReplay anchors on the last summary marker (a compaction's
		// start) so the marker is always present and the recent tail whole.
		var err error
		history, err = c.store.GetReplay(ctx, spec.Sid, historyReplayCap)
		if err != nil {
			// Fail-closed: proceeding with EMPTY history would answer an ongoing conversation
			// as if fresh (silently forgetting everything, possibly redoing done work) — worse
			// than an honest retry. An empty history with NO error is a new session, not a
			// failure, so only a real err aborts.
			return nil, err
		}
	}
	// Record the replay message count for the read-only /session gauge — the
	// window the model actually sees (post-compaction, since GetReplay anchors on
	// the summary marker). Zero extra I/O: history is already loaded. Main turns
	// only; a sub's scratch count must not overwrite the parent's gauge.
	if !spec.IsSub {
		c.recordCtxRows(spec.Sid, len(history))
	}
	sys := buildSystem(spec.Prompt)
	// A leading summary row is rendered as a USER message — identity keyed on
	// row_kind (store-stamped; also normalises legacy role=system markers), role
	// stamped here regardless. The post-compaction window then always opens with
	// a user message, which is what lets splitForCompaction stay structure-only
	// (no "recent must hold a user row" rule) and satisfies strict templates
	// that reject user-less conversations. Author is zeroed so renderSpeakers
	// (Author.Kind=="human") never prefixes it with a group speaker name.
	if len(history) > 0 && history[0].RowKind == contract.RowKindSummary {
		sum := history[0].Content
		if !spec.IsSub && spec.History != nil {
			// Read-time read-back路标 (pull 化 P1): rendered here, never persisted.
			// Gated on the ARMED capability itself (spec.History, set by
			// maybeArmHistoryTool) — not merely on the summary row — so the note
			// can never promise a tool that isn't in the suite (review: when the
			// loop-top GetReplay fails but this fresh read succeeds, arming was
			// skipped; a row-keyed note would dangle). Sub-turns get neither:
			// their History is the DELEGATOR's log, not this scratch history.
			sum += "\n" + compactReadbackNote
		}
		history[0] = contract.Message{Role: "user", Content: sum, RowKind: contract.RowKindSummary}
	}
	msgs := make([]contract.Message, 0, len(history)+1)
	if sys != "" {
		msgs = append(msgs, contract.Message{Role: "system", Content: sys})
	}
	// §6.6 cleanse: a degenerate assistant row poisons every rebuild that replays it
	// — replace its content with a marker in this slice (not the store). Then drop any
	// REVERSE-orphan tool row (a `tool` whose assistant parent was tail-cut) before
	// repairOrphanToolCalls heals the FORWARD orphans — so the window is well-formed
	// from both ends regardless of where the replay cut landed.
	return append(msgs, repairOrphanToolCalls(dropOrphanToolRows(renderSpeakers(cleanseDegenerate(history), spec.Scope)))...), nil
}

// dropOrphanToolRows removes any `tool` row that has no PRECEDING assistant tool_call
// of its id — a reverse/severed orphan, e.g. when the historyReplayCap tail-cut starts
// mid tool-exchange (compaction fires on token budget, not row count, so a long
// small-message session can exceed the cap without a summary). toAnthropicWire turns a
// leading orphan `tool` into a user message carrying a tool_result block with no
// preceding tool_use, and the provider 400s — wedging the turn until the orphan rolls
// out of the window. (repairOrphanToolCalls handles the
// inverse: an assistant tool_call with no following result.)
func dropOrphanToolRows(msgs []contract.Message) []contract.Message {
	seen := make(map[string]bool)
	out := msgs[:0] // in-place filter: msgs is the fresh per-round replay slice
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, call := range m.ToolCalls {
				seen[call.ID] = true
			}
		}
		if m.Role == "tool" && !seen[m.ToolCallID] {
			continue // severed orphan — no preceding assistant call of this id
		}
		out = append(out, m)
	}
	return out
}

// renderSpeakers prefixes a GROUP user row's content with its speaker name ("[名字]: ")
// from the row's Author, so the model can tell who said what in a multi-speaker chat.
// Decoupled: storage keeps content clean (attribution lives in Author); the prefix is
// added only at prompt-build time, and only for groups (a DM is single-speaker → no
// noise). Mutates the passed slice, which is the FRESH per-round GetReplay result (never
// the stored rows), so the in-place edit can't leak into stored content or re-prefix.
func renderSpeakers(history []contract.Message, scope string) []contract.Message {
	if !strings.Contains(scope, ":group:") {
		return history
	}
	// The "[名字]: " prefix exists to disambiguate WHO said what — worth it (and
	// non-distracting) only when MULTIPLE humans have spoken in this window. With a single
	// speaker it is pure noise a weak model may latch onto as content (e.g. treat the lone
	// sender's name as the query), so skip it then — same as a DM. Count distinct speaker
	// names (what the prefix would show); <2 → no prefix.
	seen := map[string]struct{}{}
	for i := range history {
		if m := history[i]; m.Role == "user" && m.Author.Kind == "human" {
			// Count distinct SENDERS by id (fall back to name) — so two people with the
			// same display name, or a named speaker alongside an unnamed one, still count
			// as ≥2 and get attributed.
			key := m.Author.ID
			if key == "" {
				key = prompt.SanitizeSpeaker(m.Author.Name)
			}
			if key != "" {
				seen[key] = struct{}{}
			}
		}
	}
	if len(seen) < 2 {
		return history
	}
	for i := range history {
		m := history[i]
		if m.Role == "user" && m.Author.Kind == "human" && m.Content != "" {
			if name := prompt.SanitizeSpeaker(m.Author.Name); name != "" {
				history[i].Content = "[" + name + "]: " + m.Content
			}
		}
	}
	return history
}

// repairOrphanToolCalls (spec §6.11) ensures every assistant tool_call has a
// matching tool-result message — a crash BETWEEN persisting the assistant-calls
// (I2) and all its results (I3) leaves orphans that make the model API reject the
// replay. For each call lacking a result, it splices a synthetic "interrupted"
// result right after the assistant row, so the conversation is always well-formed.
func repairOrphanToolCalls(msgs []contract.Message) []contract.Message {
	have := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			have[m.ToolCallID] = true
		}
	}
	out := make([]contract.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		for _, call := range m.ToolCalls {
			if have[call.ID] {
				continue
			}
			out = append(out, contract.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				ToolName:   call.Name,
				// Honest, NON-failure-asserting: we genuinely don't know the outcome (the
				// result row never persisted). A flat "failed" would make the model re-fire a
				// SIDE-EFFECT call (duplicate send / email / arrangement), so tell it the call
				// may have completed and must not be blindly retried.
				Content: `{"ok":false,"error":"interrupted — result not recorded; this call MAY have completed. Do not assume it failed; do not blindly retry a side-effecting action."}`,
			})
			have[call.ID] = true
		}
	}
	return out
}

// buildSystem renders the round's system prompt from the flow-filled builder.
// nil-safe (a spec with no builder → no system message). The nudge merge (spec
// §3.6) needs no special handling here: the nudge layers (80-nudge.go) ride the
// builder, which collapses every layer into this single slot-0 system message.
func buildSystem(p contract.Prompt) string {
	if p == nil {
		return ""
	}
	return p.Build()
}

var _ contract.Turn = (*core)(nil)
