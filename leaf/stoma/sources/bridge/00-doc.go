// Package bridge is the "internal" Source: it has no upstream wire protocol —
// in-process callers (the send_message tool now; the agora dispatcher / cron /
// proactive delivery later) push MessageEvents straight into the gateway via
// Emit, and the gateway processes them as if they had arrived from a real chat.
//
// Why a Source-shaped object (not a bare channel)? The gateway's existing fan-in
// (Source.Run + chan MessageEvent) is the single canonical inbound path — scope
// resolution, slash dispatch, sid resolution, the turn. Reusing it keeps an
// internally-emitted message indistinguishable from a human-typed one at every
// level below the gateway.
//
// Bidirectional (trunk-rebuild, docs/agora-port.md §6.3): inbound via Emit/Run,
// plus the agora return leg out via NewSink/returnSink.
//   - Name() = contract.SourceNameInternal ("internal"); the inbound flow's
//     classify() routes it internally, and the session resolver maps it to the
//     target scope verbatim (ev.ChatID).
//   - Run drains an in-memory buffer (cap = bufferCap) into the bus; Emit is
//     non-blocking and returns contract.ErrBridgeBusy when the buffer is full.
//   - NewSink is the agora RETURN sink (④b-2, docs/agora-delegation-return.md):
//     an internal-woken worker session's reply is routed BACK to the caller. The
//     sink buffers the turn's content and, on Finish, emits ONE internal event
//     addressed at the caller's scope (Target.ChatID, resolved by the session
//     arbiter), so the caller resumes with the worker's reply. Empty replies emit
//     nothing; SendFile is a no-op (file-return is a later refinement).
//   - Send / SendFile (source) are no-ops (no out-of-band channel); SendButtons
//     errors (no UI); HealthCheck is a tautological nil (same process).
package bridge
