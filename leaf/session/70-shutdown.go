package session

import (
	"log/slog"
	"time"
)

// shutdownTurnWait is the default deadline Shutdown waits for in-flight turn /
// drain goroutines when timeout <= 0. A well-behaved turn honors its ctx and
// exits within seconds; a tool blocked on uncancellable I/O would otherwise pin
// shutdown forever.
const shutdownTurnWait = 10 * time.Second

// Shutdown waits for all goroutines spawned by Submit / drain (everything under
// m.handleWg) to finish, bounded by timeout (<= 0 → shutdownTurnWait). Logs
// survivors so the operator knows what hung, and warns when messages are still
// queued — those leave NO persistent trace (a busy-queued message is never WAL'd;
// only an in-flight batch is, and boot recovery clears its rows silently), so the
// senders simply re-send.
func (m *Manager) Shutdown(timeout time.Duration) {
	if timeout <= 0 {
		timeout = shutdownTurnWait
	}
	done := make(chan struct{})
	go func() { m.handleWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		m.mu.Lock()
		busy := len(m.busy)
		m.mu.Unlock()
		slog.Warn("session shutdown: in-flight turns did not finish within deadline",
			"deadline", timeout, "busy_sessions", busy,
			"hint", "a tool blocked on uncancellable I/O; check the stuck session")
	}
	m.logUnprocessed()
}

// logUnprocessed warns if the manager is stopping with queued messages.
func (m *Manager) logUnprocessed() {
	m.mu.Lock()
	msgs, sess := 0, 0
	for _, b := range m.pending {
		if len(b) > 0 {
			msgs += len(b)
			sess++
		}
	}
	m.mu.Unlock()
	if msgs > 0 {
		slog.Warn("session stopped with queued messages — busy-queued messages carry no WAL row, so they are lost with the process (senders re-send; no boot notice)",
			"messages", msgs, "sessions", sess)
	}
}
