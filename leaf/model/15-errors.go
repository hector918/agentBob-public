package model

import (
	"context"
	"errors"
	"slices"
	"strings"

	"agentbob/leaf/model/providers"
)

// ErrNoModelAvailable is the sentinel the model pool returns when it has
// nothing that can serve a request right now: no entry matches the
// required tags, every match is dead/paused/in-cooldown, the pinned
// entry is gone, or every candidate was tried this request and all
// failed. Callers test for it with errors.Is; the wrapped error carries
// the human-readable detail for logs.
//
// It exists so upstream paths (small-model fallback, the agora suspend
// path) classify the failure by TYPE rather than by substring-matching
// the pool's error text. The prior string-match helper drifted out of
// sync with pick's wording and silently broke small-model fallback at
// two call sites (R18) — a typed sentinel makes that class
// of bug impossible.
var ErrNoModelAvailable = errors.New("no model available")

// ErrContextExceeded is the sentinel the failover driver returns when the
// prompt's token count exceeds the served model's context window — a
// RECOVERABLE prompt-level fault, not a pool outage. Every same-window
// candidate hits it identically (they all see the same oversized history and
// the same window), so churning the rest of the pool would just re-collect the
// same context-size 400; instead the driver stops on the first hit and surfaces
// this, so the turn can COMPACT its history and retry.
//
// Distinct from ErrNoModelAvailable, which is a pool-HEALTH / outage condition
// (nothing can serve the request at all): a caller that force-compacts on
// ErrContextExceeded but gives up on ErrNoModelAvailable classifies the two by
// TYPE (errors.Is) rather than by re-reading the pool's error text.
var ErrContextExceeded = errors.New("prompt exceeds model context window")

// errStreamStall is the sentinel for "stream opened, no first content within
// firstContentDeadline" — the pool actively timed the attempt out, so the backend
// is effectively unreachable at the SLA the pool requires. streamOneWatch
// wraps it so recordError counts the stall by errors.Is (overriding the
// cancel/deadline filter) instead of fragile string matching.
var errStreamStall = errors.New("stream stall: no content within deadline")

// ErrThinkingOnly re-exports the providers sentinel so pool-internal code and its
// tests read it without the package hop. Both provider paths raise it; see
// providers/35-toolcall-extract.go for what it means and how the pool classifies
// it (fails over, but touches neither the cooling breaker nor the exhaustion page).
var ErrThinkingOnly = providers.ErrThinkingOnly

// ErrStreamCommitted marks a post-first-content streaming failure: the provider
// errored (or the stream broke) AFTER content was already streamed to the user's
// sink, so the partial answer is COMMITTED (visible). The failover driver treats it
// as non-retryable — failing over would re-stream a second response into the SAME
// sink, leaving the user a garbled duplicate. This enforces the documented invariant
// (30-failover_test.go): a stream is retriable ONLY before the first content event;
// a post-content failure surfaces to the caller.
var ErrStreamCommitted = errors.New("stream failed after content was committed")

// errHardTimeout marks a context.DeadlineExceeded whose elapsed deadline
// was the PROVIDER's own per-call hard cap (streamHardTimeout bounds both
// the streaming and non-streaming provider paths), not the caller's ctx.
// The two are indistinguishable by error shape — http.Client.Do wraps
// either expiry in the same *url.Error chain — so the pool-side
// recordError wrapper classifies by the caller's ctx: if it is still
// alive when DeadlineExceeded surfaces, the deadline that fired can only
// be the provider's own cap, i.e. a backend that accepted the request
// then never completed it. That is this entry's health failure:
// RecordError's caller-deadline filter lets the sentinel through so the
// error counts toward cooling (without it a wedged backend never trips
// consecutiveFails, never cools, and burns the full hard cap on every
// pick). errStreamStall stays distinct — it specifically means "no FIRST
// content within firstContentTimeout".
var errHardTimeout = errors.New("provider hard timeout: backend accepted the request but never completed")

// toolArgsTruncatedPatterns are case-insensitive substring needles
// IsToolArgsTruncatedErr matches against an error's .Error() string.
// Each LLM backend phrases "the model sent a tool call whose JSON args
// were truncated mid-string" differently:
//   - llama.cpp strict tool-call parser
//   - Anthropic native API on a truncated tool_use input block
//   - OpenAI / OpenRouter / generic openai-compat tool_calls parser
//
// Add a new pattern here whenever a new "every candidate failed parsing
// tool call args" form shows up in agent.log; the salvage path + the
// recordError pool-cascade guard both pick the change up automatically.
var toolArgsTruncatedPatterns = []string{
	"failed to parse tool call arguments",
	"invalid json in tool_use",
	"tool_use input is not valid json",
	"tool_calls",
}

// IsToolArgsTruncatedErr classifies an error as "the model's response
// truncated its tool call's argument JSON mid-string" — a PROMPT-level
// failure (the content this conversation generates exceeds the model's
// output token budget), NOT a backend-health failure.
//
// Two consumers:
//
//   - agent-orchestrator/40-dispatch.go's T1 salvage path uses it to
//     decide whether to retry the failed Chat with `Tools: nil` +
//     toolArgsTruncatedNudgeText pushed.
//
//   - model/10-multipool.go::RecordError uses it to SKIP bumping a
//     candidate's fail counter when its failure was this class. The
//     same prompt that breaks one candidate breaks every candidate in
//     the pool's cascade (they all see the same conversation history
//     and the same model fine-tune behaviour), so without this filter
//     one bad turn marks every candidate dead and drives the pool
//     into "all entries tentatively recovered" flap for 5 minutes —
//     punishing healthy backends for a prompt-level fault.
//
// Substring match because the upstream error path wraps server
// response bodies as opaque strings, not typed errors — errors.Is/As
// won't reach inside them. Case-insensitive + multi-pattern so a
// provider rewording one phrase doesn't silently disable either guard.
//
// False positives are bounded: the salvage path's worst case is a
// harmless no-tools retry; the recordError guard's worst case is a
// genuinely-broken candidate that keeps returning malformed JSON
// surviving longer in the pool (admin notices via repeated "no model
// available" errors). False negatives regress to the pre-// behaviour — cascade marks-everyone-dead — also acceptable.
func IsToolArgsTruncatedErr(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	// Filter out generic-JSON-elsewhere matches by requiring a parse
	// verb in the conjunction. Same posture as the upstream salvage
	// matcher used to be.
	if !strings.Contains(low, "parse") &&
		!strings.Contains(low, "invalid json") &&
		!strings.Contains(low, "not valid json") {
		return false
	}
	for _, needle := range toolArgsTruncatedPatterns {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// contextExceededPatterns are case-insensitive substring needles
// IsContextExceededErr matches against an error's .Error() string. The
// heterogeneous backends each phrase "the prompt is larger than the model's
// context window" differently, and all wrap it as an opaque status-body string
// (errors.Is/As can't reach inside):
//   - llama.cpp strict context guard ("exceed_context_size_error",
//     "exceeds the available context size")
//   - OpenAI / OpenRouter / openai-compat ("maximum context length",
//     "context_length_exceeded", "reduce the length")
//   - Anthropic native ("prompt is too long", "input is too long")
//
// This is a strict SUBSET of isPromptLevel4xx: it matches ONLY the OVERFLOW
// forms a compaction can actually fix — NOT a generic bad-params 400 (which
// isPromptLevel4xx also skips off the health breaker, but which shrinking the
// history would not help). Add a new pattern here whenever a fresh overflow
// phrasing shows up in agent.log.
//
// Needles are kept SPECIFIC on purpose: broad forms like a bare "too many
// tokens" or "reduce the length" also appear in rate-limit / output-length
// (max_tokens too high) 400s that shrinking the HISTORY would not fix —
// matching those would wrongly STOP failover to a healthy peer and force a
// pointless compaction. So the OpenAI form is anchored to its full phrase.
var contextExceededPatterns = []string{
	"exceed_context_size_error",
	"exceeds the available context size",
	"maximum context length",
	"context_length_exceeded",
	"context window",
	"reduce the length of the messages",
	"prompt is too long",
	"input is too long",
}

// IsContextExceededErr classifies an error as "the prompt exceeded the served
// model's context window" — a RECOVERABLE prompt-level fault the turn fixes by
// compacting history and retrying, distinct from both a backend-health failure
// and a whole-pool outage (ErrNoModelAvailable). Same string-match posture and
// robustness rationale as IsToolArgsTruncatedErr above: the provider paths wrap
// status bodies as opaque fmt.Errorf strings, so errors.Is can't reach the
// signature; case-insensitive + multi-pattern so a provider rewording one
// phrase doesn't silently disable the compact-retry path. Returns false on nil.
//
// Note it also matches ErrContextExceeded's own text ("context window"), so the
// classification stays idempotent once the driver has wrapped the sentinel.
func IsContextExceededErr(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, needle := range contextExceededPatterns {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// statusArrow is the separator both providers' non-2xx chatter errors put
// between the request line and the HTTP status: `POST <url> → <status>: <body>`
// (00-client.go / 30-anthropic.go). Since D3 the providers'
// in-stream error frames carry the same arrow whenever their code/type
// fields yield a status (`provider error → <code>: <msg>`), so an in-stream
// 400 classifies identically to a status-line 400. isPromptLevel4xx keys off
// the arrow to recover the status code without a typed error chain (those
// paths wrap the status as a plain fmt.Errorf string).
const statusArrow = " → "

// isPromptLevel4xx reports whether err is a CONTENT-level backend rejection —
// the request itself was malformed or too large for the model (context
// overflow → 400 / 413 / 422, bad params → 400, payload too large → 413), as
// opposed to an entry-HEALTH 4xx (401/403 key revoked, 404/405 wrong endpoint,
// 429 rate-limited). The server is alive and answered; it refused THIS request
// for a reason no other candidate would dodge.
//
// Same posture as IsToolArgsTruncatedErr's pool-cascade guard: a context-
// overflow on a big conversation is a prompt-level fault that every candidate
// would hit identically (the picker has no ContextWindow pre-routing — the
// overflow only surfaces inside the pool), so counting it toward an entry's
// cooling breaker can drag a healthy small-window entry dead for 5 minutes and
// push even small requests to fallback. recordError skips it.
//
// NARROWED (B3 round 2): only 400/413/422 are skipped. The earlier
// blanket "any 4xx" let an entry's OWN health faults — 401/403 (key revoked /
// forbidden), 429 (this entry is rate-limited), 404/405 (endpoint misconfig) —
// escape the cooling breaker, so a dead-key entry stayed live and burned one
// guaranteed-failing attempt on every request before failing over (and the
// all-entries-dead alert never fired). Those codes ARE per-entry health and
// now count. 5xx likewise counts. Transport failures never classify here —
// they carry no " → <status>" arrow.
//
// String-match because the provider paths wrap status errors as opaque
// fmt.Errorf strings, not typed errors (errors.As can't reach the status).
func isPromptLevel4xx(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	idx := strings.Index(s, statusArrow)
	if idx < 0 {
		return false
	}
	// resp.Status starts with the 3-digit numeric code, e.g. "400 Bad Request".
	code := s[idx+len(statusArrow):]
	if len(code) < 3 {
		return false
	}
	switch code[:3] {
	case "400", "413", "422":
		return true
	default:
		return false
	}
}

// statusMarkers are the two shapes a backend status code reaches the pool in.
// The OpenAI-compatible / Anthropic clients render `POST <url> → <status>: <body>`
// (statusArrow); the sidecar providers (rapidocr, and anything else speaking a
// plain HTTP contract rather than a chat API) render `<name>: HTTP <code>: <body>`.
// "HTTP " is matched case-SENSITIVELY on purpose — a lowercased haystack would
// match the `http://` inside every URL a transport error quotes.
var statusMarkers = []string{statusArrow, "HTTP "}

// backendStatusCode digs the 3-digit HTTP status out of a backend error's text,
// covering both statusMarkers shapes. Text-matching for the same reason
// IsContextExceededErr does it: the provider paths wrap a status + body as an
// opaque fmt.Errorf string, so errors.As has nothing to reach for.
func backendStatusCode(err error) (string, bool) {
	s := err.Error()
	for _, marker := range statusMarkers {
		idx := strings.Index(s, marker)
		if idx < 0 {
			continue
		}
		code := s[idx+len(marker):]
		if len(code) < 3 {
			continue
		}
		if code[0] < '0' || code[0] > '9' || code[1] < '0' || code[1] > '9' || code[2] < '0' || code[2] > '9' {
			continue
		}
		return code[:3], true
	}
	return "", false
}

// transientStatuses are the statuses that mean "come back in a moment", not
// "this backend is broken":
//   - 429 — the backend's own queue is full (it is asking us to line up)
//   - 502 / 503 / 504 — a reverse proxy in front of the backend has no healthy
//     upstream RIGHT NOW (restarting container, cold worker, saturated pool)
//
// Everything else is either the request's fault (4xx) or a genuine backend
// fault (500) that waiting will not fix.
var transientStatuses = []string{"429", "502", "503", "504"}

// transientTransportNeedles match a connection that never completed a
// round-trip — a backend that was mid-restart when we knocked looks exactly
// like this (the proxy accepts, then EOFs / refuses). Matched only when the
// error carries NO status: a real status is the more precise signal, so a 500
// whose body happens to contain the word "eof" must not be read as transient.
var transientTransportNeedles = []string{
	"connection refused",
	"connection reset",
	"broken pipe",
	"eof",
	"no such host",
	"i/o timeout",
	"network is unreachable",
	"connection timed out",
	"server closed idle connection",
}

// isTransientBackendErr classifies an error as "the backend is BUSY or
// momentarily unreachable" — the class the pool answers by queueing and
// retrying the same entry (withBusyRetry) instead of declaring the kind
// unavailable and paging an admin.
//
// Why this class needs to exist: the sidecar backends are
// deliberately restart-happy — the OCR sidecar hard-exits after an idle
// window and docker brings it straight back — so a seconds-long window where
// the fronting nginx has no upstream is DESIGNED-IN, not an incident. A
// single-candidate kind (ocr, asr) has nowhere to fail over to, so before this
// classifier a 1-second blip became: hard tool failure + "no fallback left"
// admin page + the model quietly substituting a different tool. Observed
// 23:48:20 (one 502; the same kind served fine 70s later).
//
// Deliberately NOT transient: caller cancellation, a committed stream (failing
// over would double-write the sink), and the prompt-level classes that every
// candidate would hit identically — retrying those just burns the budget.
func isTransientBackendErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrStreamCommitted) || errors.Is(err, ErrStreamWatcherTripped) {
		return false
	}
	if IsContextExceededErr(err) || IsToolArgsTruncatedErr(err) || isPromptLevel4xx(err) {
		return false
	}
	if code, ok := backendStatusCode(err); ok {
		return slices.Contains(transientStatuses, code)
	}
	low := strings.ToLower(err.Error())
	for _, needle := range transientTransportNeedles {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}
