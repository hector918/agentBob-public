package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"agentbob/contract"
)

// bufferCap is the depth of the in-memory emit buffer. Emit is non-blocking
// against it (a full buffer is backpressure → ErrBridgeBusy), so a slow
// downstream never blocks an emitting tool.
const bufferCap = 100

// Source is the in-process "internal" source. It satisfies contract.Source (so
// the gateway fans it in + looks it up by name) and contract.MessageEmitter (so
// in-process callers push events into it).
type Source struct {
	buf chan contract.MessageEvent
}

// New builds the internal source with an empty emit buffer.
func New() *Source { return &Source{buf: make(chan contract.MessageEvent, bufferCap)} }

func (s *Source) Name() string { return contract.SourceNameInternal }

// Caps: Trusted (internal events need no sender screen) and no
// ReplyTo (no wire message to reply to).
func (s *Source) Caps() contract.Caps { return contract.Caps{Trusted: true} } // internal agora traffic — not screened

// HealthCheck is a tautological nil — we live in the same process.
func (s *Source) HealthCheck(context.Context) error { return nil }

// Run drains the emit buffer into the bus's fan-in stream until ctx is
// cancelled. The buffer (not out) is the backpressure point, so Emit stays
// non-blocking. Returns nil on cancel (graceful shutdown, not an error).
func (s *Source) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-s.buf:
			select {
			case out <- ev:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// Emit implements contract.MessageEmitter: push ev onto the inbound buffer,
// non-blocking. Source is forced to SourceNameInternal so the inbound flow always
// routes it internally. A full buffer → contract.ErrBridgeBusy.
func (s *Source) Emit(ev contract.MessageEvent) error {
	ev.Source = contract.SourceNameInternal
	select {
	case s.buf <- ev:
		return nil
	default:
		return contract.ErrBridgeBusy
	}
}

// NewSink returns the agora RETURN sink (④b-2, docs/agora-delegation-return.md
// §3 B1): an internal-woken worker session's reply is the RETURN leg of an agent→
// agent dispatch and must reach the CALLER, not be dropped. The session arbiter
// resolved t.ChatID to the caller's scope (Target{Source:internal}); this sink
// buffers the turn's content and, on Finish, re-Emits ONE internal event addressed
// there, so the caller resumes with the worker's reply as an ordinary inbound.
//
// Zero decision, zero agora import: the destination scope is computed by the
// session core (resolveReturnTarget) and handed in via t.ChatID; the bridge only
// re-Emits through its own in-memory channel.
func (s *Source) NewSink(_ context.Context, t contract.Target, scope string, _ string, _ contract.SinkPrefs) contract.Sink {
	return &returnSink{src: s, targetScope: t.ChatID, fromScope: scope}
}

// Send is a no-op — the internal source has no out-of-band feedback channel (an
// internal event's only consumer is the downstream turn, which replies via its
// own sink).
func (s *Source) Send(_ context.Context, _ contract.Target, text string) error {
	slog.Debug("bridge: Send no-op (internal source has no out-of-band channel)", "len", len(text))
	return nil
}

// SendFile is a no-op for the same reason as Send.
func (s *Source) SendFile(_ context.Context, _ contract.Target, path, caption string) error {
	slog.Debug("bridge: SendFile no-op (internal source has no file channel)", "path", path)
	return nil
}

// SendButtons errors: internal events carry no inline-button context, so a
// button against the internal sink is a wiring bug we surface loudly.
func (s *Source) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", errors.New("bridge: SendButtons unsupported (internal source has no UI)")
}

// returnSink is the agora return leg's sink. On Finish (called exactly once by
// the turn) it emits ONE internal event carrying the full reply, addressed at
// targetScope (the caller session's scope).
type returnSink struct {
	src         *Source
	targetScope string
	fromScope   string // the worker's member-inbox scope — stamped as the return event's author (D7)
}

// ContentDelta is a no-op: this sink acts only on Finish, which receives the
// COMPLETE reply (== the concatenation of all ContentDeltas). Emitting per-delta
// would wake the caller repeatedly; trace deltas are progress noise dropped too.
func (rs *returnSink) ContentDelta(string) {}

// LastSent returns "" — the return leg emits an INTERNAL event (no platform
// message id), so there is nothing for reply routing to anchor on.
func (rs *returnSink) LastSent() string { return "" }

// TraceDelta drops progress annotations — a return event is the worker's finished
// product, not a live stream. Keeping trace out of the caller's inbox keeps the
// return leg one clean message.
func (rs *returnSink) TraceDelta(string) {}

// Finish emits the accumulated reply as one internal event to the caller scope.
// An EMPTY reply (worker produced no content) is NOT emitted — there is nothing to
// return, and an empty inbound would wake the caller for no reason. A missing
// target scope is a wiring bug (the arbiter only routes here when it resolved a
// caller scope): log loud and drop rather than emit a mis-addressed event.
//
// Loops (A→B→A) carry no mechanical cap by design (a count/rate cap would kill a
// legitimately-looping agent — the loop is structurally identical). A degenerate
// loop instead ends at the MODEL's judgement: when a woken worker has nothing to add
// it calls stay_silent (leaf/tools/21-stay-silent.go) → empty ExitRequest → this sink
// is never Finished → nothing is returned → the dispatch chain stops here. Org
// permissions bound who can be reached in the first place.
func (rs *returnSink) Finish(full string) error {
	if strings.TrimSpace(full) == "" {
		slog.Debug("bridge return: empty worker reply — nothing to return", "to_scope", rs.targetScope)
		return nil
	}
	if rs.targetScope == "" {
		slog.Warn("bridge return: no target scope — dropping worker reply (arbiter wiring bug)", "reply_len", len(full))
		return nil
	}
	// Dispatch left nil — the caller is a real session and this is its inbound
	// reply, not a further dispatch (it must not re-record a caller edge).
	ev := contract.MessageEvent{
		ChatID:     rs.targetScope, // gateway maps src=internal → ChatID = scope verbatim
		MessageID:  fmt.Sprintf("agora-return-%d", time.Now().UnixNano()),
		Text:       full,
		ReceivedAt: time.Now(),
	}
	// D7: stamp the worker's identity so the caller's history attributes this reply to the
	// member inbox that produced it (ComposeUserMsgs renders an internal event as an agent
	// author, F91①) — otherwise concurrent workers' returns are indistinguishable anonymous
	// rows (F86). Empty fromScope (unwired) → the compose fallback "internal".
	if rs.fromScope != "" {
		ev.UserID = "inbox:" + rs.fromScope
		ev.UserName = "agora worker " + rs.fromScope
	}
	if err := rs.src.Emit(ev); err != nil {
		// ErrBridgeBusy here loses the return (buffer full). Surface it so finishLog
		// logs a delivery failure — the worker's product is still in its session
		// history, recoverable by a re-call.
		return fmt.Errorf("bridge return: emit reply to %s: %w", rs.targetScope, err)
	}
	slog.Info("bridge return: worker reply emitted to caller", "to_scope", rs.targetScope, "reply_len", len(full))
	return nil
}

// BareProductFinish (contract.BareProductSink marker): ContentDelta above is a
// no-op and Finish's full is emitted VERBATIM as the caller's inbound
// MessageEvent.Text — the turn core must hand it the bare trimmed reply, never
// the sunk ContentDelta accumulation (a tool-round preamble glued onto the
// reply would pollute the caller's session history).
func (rs *returnSink) BareProductFinish() {}

// SendFile fails LOUD: the agora return leg carries only the text reply, it has no
// file channel. Returning nil here made a worker's deliver_file report success while
// the file never reached the caller. Mirror captureSink (the sub-loop sink): surface
// the rejection so deliver_file fails and the worker folds the artifact into its text
// product or uses file_storage. (File-return over the bridge is a later refinement.)
func (rs *returnSink) SendFile(path, _ string) error {
	return fmt.Errorf("agora return leg has no file channel: cannot deliver %q — include it in your text reply or use file_storage", path)
}

var (
	_ contract.Source          = (*Source)(nil)
	_ contract.MessageEmitter  = (*Source)(nil)
	_ contract.Sink            = (*returnSink)(nil)
	_ contract.BareProductSink = (*returnSink)(nil)
)
