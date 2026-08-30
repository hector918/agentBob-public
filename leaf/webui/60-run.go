package webui

import (
	"net/http"
	"strings"
	"sync"

	"agentbob/contract"
)

// webuiRunnable is the allowlist of slash commands safe to run HEADLESS from the
// dock: global, admin, and side-effect-bounded by their arguments (not by a chat
// scope). Scope-dependent commands (/new, /session, /trace, /prompt, …) need a
// real chat context and are deliberately excluded; /webui (token mint) is
// pointless over http. Mirrors the skeleton's webuiRunnable intent.
var webuiRunnable = map[string]bool{
	"model":       true,
	"agora":       true,
	"accounts":    true,
	"gate":        true, // /gate allow <source> <uid> — the rejected-row "allow" action
	"arrangement": true, // /arrangement cancel <id> — the per-row "cancel" action
	"help":        true,
}

// captureSink is a contract.Sink that accumulates the reply instead of
// delivering it — the dock renders the captured string. Finish carries the
// COMPLETE content (== concatenation of ContentDeltas), so it replaces the buffer.
type captureSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *captureSink) ContentDelta(text string) { s.mu.Lock(); s.buf.WriteString(text); s.mu.Unlock() }
func (s *captureSink) TraceDelta(string)        {}
func (s *captureSink) Finish(full string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if full != "" {
		s.buf.Reset()
		s.buf.WriteString(full)
	}
	return nil
}
func (s *captureSink) SendFile(string, string) error { return nil }
func (s *captureSink) LastSent() string              { return "" } // no platform message

// String reads the captured reply under the same mutex the writers take.
func (s *captureSink) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.buf.String() }

var _ contract.Sink = (*captureSink)(nil)

// handleRun executes a management slash command from the dock (admin only),
// synchronously, and returns the captured reply text. The synthetic event is a
// webui-origin admin DM — enough for Dispatch (which parses Event.Text) and for
// the allowlisted handlers (which act on their args, not a chat scope).
func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.slash == nil {
		http.Error(w, "command runner unavailable", http.StatusServiceUnavailable)
		return
	}
	line, err := readBody(r, 4096)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	line = strings.TrimSpace(line)
	name := slashName(line)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !webuiRunnable[name] {
		w.Header().Set("X-Run-Status", "error")
		_, _ = w.Write([]byte("命令 /" + name + " 不能从仪表盘运行（需要聊天上下文，或不对外开放）。"))
		return
	}
	sink := &captureSink{}
	// Dispatch returns the command's error (nil = success); the reply text is
	// captured in the sink either way. X-Run-Status lets the dashboard mark the
	// result page with a green ✓ / red ✗ without parsing the text.
	runErr := s.slash.Dispatch(r.Context(), contract.SlashContext{
		Event: contract.MessageEvent{
			Source: "webui", ChatType: contract.ChatDM, ChatID: "webui",
			UserID: "webui-admin", UserName: "admin", IsAdmin: true, Lang: "zh", Text: line,
		},
		Sink:    sink,
		IsAdmin: true,
		Lang:    "zh",
	})
	out := sink.String()
	if out == "" {
		out = "(no output)"
	}
	if runErr != nil {
		w.Header().Set("X-Run-Status", "error")
	} else {
		w.Header().Set("X-Run-Status", "ok")
	}
	_, _ = w.Write([]byte(out))
}

// slashName extracts the lowercased command name from a "/name args" line.
func slashName(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if i := strings.IndexAny(line, " \t\n"); i >= 0 {
		line = line[:i]
	}
	return strings.ToLower(line)
}
