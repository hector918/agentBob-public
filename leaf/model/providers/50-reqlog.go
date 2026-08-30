package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/heartwood/scrub"
	"agentbob/logging/ringfile"
)

// reqLogger writes raw outbound LLM request bodies to a single capped
// file (default $BOB_HOME/logs/llm-requests.log). Purpose: prompt-cache
// analysis — operator wants to see the exact bytes that went over the
// wire so they can diff turns, identify cache-busting variations, and
// measure the cacheable-prefix length.
//
// Storage policy: one file, capped at maxBytes. When a new write would
// push the file past the cap, the OLDEST half is dropped and writing
// continues. The file therefore always holds approximately the last
// maxBytes/2 .. maxBytes of traffic. (Pure ring-buffer in a file is
// awkward to tail; this trim-and-continue scheme keeps `tail -f`
// working and the file always parseable as JSONL.)
//
// Format: JSONL. Each request emits TWO lines — a "req" record at
// dispatch time and a "res" record after the response (success or
// error) is in hand. They share a `pair` integer so a reader can
// correlate without relying on ordering (though they ARE adjacent
// in normal flow):
//
//	{"t":"2026-05-18T14:30:00.123Z","kind":"req","pair":42,
//	 "provider":"openai-compat","entry":"smart","model":"Qwen3.6-...",
//	 "url":"...","body":{...raw request payload as JSON...}}
//	{"t":"2026-05-18T14:30:02.456Z","kind":"res","pair":42,
//	 "provider":"openai-compat","entry":"smart","model":"Qwen3.6-...",
//	 "status":200,"latency_ms":2333,"body":{...response JSON...}}
//
// `entry` is the MultiPool entry name (smart / small / …) plumbed
// via WithEntryName on the ctx — the same value users put in
// models.yaml.
//
// System-prompt redaction: the req `body` has its system prompt replaced
// with a "<system omitted: N chars>" marker before the record is written
// (see redactSystemContent). The system prompt is the large near-constant
// cacheable prefix; logging it verbatim every turn would dominate the file
// and crowd out the turn-varying tail. Covers both the OpenAI messages-
// array `role:"system"` shape and the Anthropic top-level `system` field.
//
// Streaming responses (ChatStream): the res record's body is a small
// summary JSON with stream=true, the accumulated output_text, and
// usage counts rather than the raw SSE bytes (which would balloon
// the log without helping cache analysis).
//
// Safe for concurrent use — single mutex inside the embedded RingFile.
// Misconfiguration (unwritable path, disk full) logs a single WARN and
// disables the logger for the process lifetime; it never returns an
// error to the LLM request path or blocks it.
//
// REFACTOR 3.1 follow-up: the append/cap/trim/cached-fd
// machinery moved into logging/ringfile shared with permissions/
// 20-judgelog.go and credentials/50-calllog.go. This struct is now a
// thin wrapper that owns only the domain-specific bits (pair-id
// correlation, ctx-keyed entry-name plumbing, req/res JSON record
// shapes); the byte-equivalent writeRecord + trimLocked pair was
// deleted.
type reqLogger struct {
	rf *ringfile.RingFile
}

// Module-level singleton — initialised once in pipeline-startup. The
// provider HTTP send paths reach it via LogRequest() rather than
// threading a reference through the Client struct (which would touch
// half a dozen call sites in non-logging code).
var (
	globalReqLogger     *reqLogger
	globalReqLoggerOnce sync.Once
)

// InitReqLogger configures the singleton. Subsequent calls are no-ops
// (sync.Once). Path is the full file path; maxBytes is the soft cap
// (the actual file may briefly exceed by one record before trim runs).
// Both <= 0 = logging disabled.
//
// Delegates the file-handle lifecycle to ringfile.Open, which keeps
// the R4Y-17 cached-fd optimisation and the
// WARN-once-self-disable contract. Subsystem prefix "model: llm
// requests" tags ringfile's WARN messages so they're greppable
// alongside the existing "permissions: judge log" / "credentials:
// call log" streams.
func InitReqLogger(path string, maxBytes int64) {
	globalReqLoggerOnce.Do(func() {
		rf, _ := ringfile.Open(path, maxBytes, "model: llm requests")
		if rf == nil {
			return
		}
		globalReqLogger = &reqLogger{rf: rf}
		slog.Info("llm request log enabled", "path", path, "max_bytes", maxBytes)
	})
}

// CloseReqLogger closes the singleton's cached file handle. Best-effort
// idempotent shutdown hook for graceful process exit (logger stays
// disabled afterwards so any straggler call short-circuits via the
// ringfile's disabled flag). Not called in the hot path.
func CloseReqLogger() {
	l := globalReqLogger
	if l == nil || l.rf == nil {
		return
	}
	l.rf.Close()
}

// pairCounter mints monotonically-increasing ids correlating each
// req record with its matching res record. Wraparound at int64-max
// would take many millennia of LLM requests; not a concern.
var pairCounter atomic.Int64

// ctxEntryKey is the context key for the MultiPool entry name (e.g.
// "smart" / "small"). Set by MultiPool before calling chatter.Chat /
// ChatStream; read by LogRequest. Empty when ctx wasn't decorated.
type ctxEntryKey struct{}

// WithEntryName attaches the MultiPool entry name to ctx so the
// downstream reqlog can include it in the log record. Cheap; the
// chatter doesn't otherwise care about the entry name.
func WithEntryName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxEntryKey{}, name)
}

func entryNameFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxEntryKey{}).(string)
	return v
}

// LogRequest captures one outbound LLM request and returns a pair id
// for the matching LogResponse call. Best-effort: returns 0 when the
// logger is disabled (so LogResponse can short-circuit), never blocks
// the actual request, never returns an error.
//
// ctx is read for the MultiPool entry name (via WithEntryName). The
// provider / url / model fields come from the caller; body is the
// raw JSON payload posted in http.NewRequestWithContext.
func LogRequest(ctx context.Context, provider, url, model string, body []byte) int64 {
	l := globalReqLogger
	if l == nil {
		return 0
	}
	pair := pairCounter.Add(1)
	l.appendReq(pair, provider, entryNameFromCtx(ctx), url, model, body)
	return pair
}

// LogResponse records the result of the request identified by pair.
// pair=0 (LogRequest returned 0 because logging was disabled) is a
// silent no-op. body may be nil (network error before any bytes
// arrived); err may be nil (success); both may be set (e.g. a 5xx
// returned a body AND we mapped status to an error).
//
// For streaming responses, body should be a small summary JSON
// (stream=true, output_text, usage) rather than the raw SSE bytes.
func LogResponse(pair int64, provider, url, model string, status int, latency time.Duration, body []byte, err error) {
	l := globalReqLogger
	if l == nil || pair == 0 {
		return
	}
	// entry name was already captured in the matching req record; we
	// re-pass provider/url/model for self-contained res records (so
	// a reader picking up just the res line still has context without
	// having to walk back to the req).
	l.appendRes(pair, provider, url, model, status, latency, body, err)
}

// redactSystemContent replaces the system prompt in a request body with a
// short "<system omitted: N chars>" marker before logging. The system
// prompt is the large, near-constant cacheable prefix — re-logging it
// verbatim on every turn dominates the file and crowds out the
// turn-varying tail the operator actually diffs. The marker keeps the
// byte length so cacheable-prefix measurement still works.
//
// Handles both wire shapes: an OpenAI-style messages array (each element
// with role=="system" → content redacted) and an Anthropic-style
// top-level "system" field (string or content-block array). On any parse
// failure the original body is returned unchanged — best-effort, never
// drops a record.
//
// NOTE: redaction round-trips the body through a map, so a redacted record's
// TOP-LEVEL object keys come out json.Marshal-sorted (alphabetical), NOT in the
// original wire order. Byte-exact wire diffing therefore holds only for records WITHOUT
// a system prompt (returned verbatim); a system-bearing record is key-sorted. Arrays
// (the messages list, response bodies) keep their order, so cacheable-prefix length and
// message-order diffing are unaffected — only top-level key order shifts.
func redactSystemContent(body []byte) []byte {
	if !json.Valid(body) {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	changed := false

	if msgs, ok := m["messages"].([]any); ok {
		for _, mi := range msgs {
			msg, ok := mi.(map[string]any)
			if !ok || msg["role"] != "system" {
				continue
			}
			if c, ok := msg["content"]; ok {
				msg["content"] = redactionMarker(c)
				changed = true
			}
		}
	}
	if sys, ok := m["system"]; ok {
		m["system"] = redactionMarker(sys)
		changed = true
	}

	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// redactionMarker renders a "<system omitted: N chars>" placeholder where
// N is the JSON-serialised length of the original content (string or
// array of blocks), so the redacted record still reflects how big the
// dropped prefix was.
func redactionMarker(content any) string {
	n := 0
	switch v := content.(type) {
	case string:
		n = len(v)
	default:
		if b, err := json.Marshal(v); err == nil {
			n = len(b)
		}
	}
	return fmt.Sprintf("<system omitted: %d chars>", n)
}

func (l *reqLogger) appendReq(pair int64, provider, entry, url, model string, body []byte) {
	body = redactSystemContent(body)
	// Scrub secrets before they hit disk: a credential that entered the prompt by a path
	// scrub didn't already cover (a pasted key, OCR/attachment text) would otherwise be
	// logged verbatim. scrub is idempotent + false-negative-biased, so cache-diff analysis
	// is unaffected; markers are quote-free so the JSON body stays valid.
	body = []byte(scrub.Scrub(string(body)))
	rec, mErr := json.Marshal(map[string]any{
		"t":        time.Now().UTC().Format(time.RFC3339Nano),
		"kind":     "req",
		"pair":     pair,
		"provider": provider,
		"entry":    entry,
		"model":    model,
		"url":      url,
		"body":     json.RawMessage(body),
	})
	if mErr != nil {
		// Body wasn't valid JSON: fall back to a string field.
		rec, _ = json.Marshal(map[string]any{
			"t":        time.Now().UTC().Format(time.RFC3339Nano),
			"kind":     "req",
			"pair":     pair,
			"provider": provider,
			"entry":    entry,
			"model":    model,
			"url":      url,
			"body_raw": string(body),
		})
	}
	l.rf.WriteRecord(rec)
}

func (l *reqLogger) appendRes(pair int64, provider, url, model string, status int, latency time.Duration, body []byte, err error) {
	m := map[string]any{
		"t":          time.Now().UTC().Format(time.RFC3339Nano),
		"kind":       "res",
		"pair":       pair,
		"provider":   provider,
		"model":      model,
		"url":        url,
		"status":     status,
		"latency_ms": latency.Milliseconds(),
	}
	if err != nil {
		m["error"] = scrub.Scrub(err.Error())
	}
	if len(body) > 0 {
		body = []byte(scrub.Scrub(string(body))) // scrub secrets before disk (see appendReq)
		// Try as JSON first (response body for a normal LLM call).
		// Fall back to string for SSE summaries / non-JSON error bodies.
		if json.Valid(body) {
			m["body"] = json.RawMessage(body)
		} else {
			m["body_raw"] = string(body)
		}
	}
	rec, _ := json.Marshal(m)
	l.rf.WriteRecord(rec)
}
