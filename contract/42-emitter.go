package contract

import "errors"

// MessageEmitter is the internal-source ingress seam: an in-process caller (the
// send_message tool, cron, proactive delivery) pushes a MessageEvent straight
// into the gateway's inbound stream, which then processes it EXACTLY as a wire
// event (scope resolution / slash / sid / turn). The bridge source (its Name()
// == SourceNameInternal) implements it; a consumer resolves it via
// gw.SourceByName(SourceNameInternal).(MessageEmitter) so it never imports the
// concrete source.
type MessageEmitter interface {
	// Emit pushes ev onto the internal source's inbound buffer. NON-BLOCKING: a
	// full buffer returns ErrBridgeBusy so the caller surfaces a retryable error
	// instead of deadlocking. Source is forced to SourceNameInternal on the way in.
	Emit(ev MessageEvent) error
}

// ErrBridgeBusy is returned by MessageEmitter.Emit when the internal source's
// inbound buffer is full — a backpressure signal the caller retries/backs off,
// never a permanent failure.
var ErrBridgeBusy = errors.New("contract: internal bridge busy (buffer full)")
