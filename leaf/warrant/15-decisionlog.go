package warrant

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"agentbob/heartwood/clock"
	"agentbob/logging/ringfile"
)

// decisionLogger writes one JSONL line per warrant decision to a
// dedicated file (default $BOB_HOME/logs/warrant.log). Purpose: an admin
// can `tail -f` or `jq` over it to see every capability decision
// (allow/deny + reason) without grepping a noisy agent log. A dedicated
// stream also lets us cap retention tighter than the general log.
//
// Format: one self-contained JSON record per line, ~200 bytes each:
//
//	{"t":"2026-06-16T14:30:00.123Z","decision":"allow",
//	 "principal":"hector","capability":"tool:use:web_search"}
//	{"t":"...","decision":"deny","principal":"guest",
//	 "capability":"tool:use:terminal","reason":"... not granted to \"guest\""}
//
// Storage policy: 10MB cap (warrant records are ~200 bytes, so 10MB is
// weeks of audit). When a write would push past the cap, the oldest half
// is trimmed in place (trim-and-continue, keeps `tail -f` working +
// the file parseable as JSONL).
//
// This struct is a thin wrapper over logging/ringfile — it owns only the
// record shape + singleton plumbing; the append/cap/trim/cached-fd
// machinery + WARN-once-self-disable contract live in ringfile.
//
// Safe for concurrent use — single mutex inside the embedded RingFile.
// Misconfiguration (unwritable path, disk full) logs one WARN and
// disables the logger for the process lifetime; never returns an error
// to / blocks the warrant decision path.
type decisionLogger struct {
	rf *ringfile.RingFile
}

var (
	globalDecisionLogger     *decisionLogger
	globalDecisionLoggerOnce sync.Once
)

// InitDecisionLogger configures the singleton. Subsequent calls are
// no-ops (sync.Once). Wired from the warrant module Start; path is the
// full file path, maxBytes is the soft cap. Both <= 0 = logging disabled.
//
// Delegates the file-handle lifecycle to ringfile.Open (cached-fd reuse
// + WARN-once-self-disable). Subsystem prefix "warrant: decision log"
// tags ringfile's WARN messages so they're greppable.
func InitDecisionLogger(path string, maxBytes int64) {
	globalDecisionLoggerOnce.Do(func() {
		rf, _ := ringfile.Open(path, maxBytes, "warrant: decision log")
		if rf == nil {
			return
		}
		globalDecisionLogger = &decisionLogger{rf: rf}
		slog.Info("warrant: decision log enabled", "path", path, "max_bytes", maxBytes)
	})
}

// CloseDecisionLogger closes the singleton's cached file handle.
// Best-effort idempotent shutdown hook (logger stays disabled afterwards
// so any straggler call short-circuits via the ringfile disabled flag).
// Not called in the hot path.
func CloseDecisionLogger() {
	l := globalDecisionLogger
	if l == nil || l.rf == nil {
		return
	}
	l.rf.Close()
}

// logDecision appends one warrant decision record. Best-effort; never
// returns an error. reason is omitted from the record when empty (the
// allow path carries no reason).
func logDecision(allow bool, principal, capability, reason string) {
	l := globalDecisionLogger
	if l == nil || l.rf == nil {
		return
	}
	rec := map[string]any{
		"t":          clock.Now().Format(time.RFC3339Nano),
		"decision":   decisionString(allow),
		"principal":  principal,
		"capability": capability,
	}
	if reason != "" {
		rec["reason"] = reason
	}
	if body, err := json.Marshal(rec); err == nil {
		l.rf.WriteRecord(body)
	}
}

func decisionString(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}

// logAuthorize appends ONE summary record per Authorize/AuthorizeSkills projection
// (kind="tool"/"skill") — the principal + the names it was granted this turn. One
// line per projection (not per-tool), so a default-off matrix can't flood the log,
// while the operator still sees what each turn's principal was authorized for.
func logAuthorize(kind, principal string, granted []string) {
	l := globalDecisionLogger
	if l == nil || l.rf == nil {
		return
	}
	rec := map[string]any{
		"t":         clock.Now().Format(time.RFC3339Nano),
		"decision":  "authorize",
		"kind":      kind,
		"principal": principal,
		"granted":   granted,
	}
	if body, err := json.Marshal(rec); err == nil {
		l.rf.WriteRecord(body)
	}
}
