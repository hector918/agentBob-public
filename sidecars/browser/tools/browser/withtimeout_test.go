package browser

import (
	"context"
	"testing"
)

// TestWithTimeoutCancelIdempotent pins the root-cause fix for the Navigate
// auto-follow double-cancel panic: the composite CancelFunc returned by
// withTimeout must be safe to call more than once (context.CancelFunc's
// contract). Before the sync.Once guard, the second call ran close() on an
// already-closed channel and panicked. Navigate's auto-follow path calls
// cancel() explicitly AND again via defer, so this is a live double-call.
func TestWithTimeoutCancelIdempotent(t *testing.T) {
	agentCtx, agentCancel := context.WithCancel(context.Background())
	defer agentCancel()

	sess := &Session{BrowserCtx: context.Background()}
	_, cancel := sess.withTimeout(agentCtx, 5)

	// Two calls must not panic (recover would fail the test otherwise).
	cancel()
	cancel()
}
