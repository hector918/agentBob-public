package adminline

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/adminline/store"
)

// fakeStore is an in-memory store.Store: captures appends + marks.
type fakeStore struct {
	mu       sync.Mutex
	appended []store.AdminAlert
	marks    map[int64]store.AdminForwardState
	seq      int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{marks: map[int64]store.AdminForwardState{}}
}
func (f *fakeStore) AppendAlert(_ context.Context, a store.AdminAlert) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	a.ID = f.seq
	f.appended = append(f.appended, a)
	return f.seq, nil
}
func (f *fakeStore) MarkAlertForwarded(_ context.Context, id int64, s store.AdminForwardState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks[id] = s
	return nil
}
func (f *fakeStore) PruneAdminLine(context.Context, int64) (int64, error) { return 0, nil }

// fakeDeliverer captures delivered alerts.
type fakeDeliverer struct {
	mu        sync.Mutex
	delivered []store.AdminAlert
}

func (d *fakeDeliverer) Deliver(_ context.Context, a store.AdminAlert) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivered = append(d.delivered, a)
	return nil
}

func alert(origin string) store.AdminAlert {
	return store.AdminAlert{TS: 1, Level: contract.AdminError, Origin: origin, Text: "x", Forwarded: store.ForwardNone}
}

// TestThrottleAndRollup: within a window only `max` alerts of one (origin,level)
// forward; the rest are persisted+suppressed and folded into one rollup when the
// window rolls.
func TestThrottleAndRollup(t *testing.T) {
	fs, fd := newFakeStore(), &fakeDeliverer{}
	clock := time.Unix(1000, 0)
	l := newLine(fs, fd)
	l.window, l.max = 60*time.Second, 2 // worker not started — safe to override
	l.now = func() time.Time { return clock }
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		l.handle(ctx, alert("model-pool"))
	}
	// 2 forwarded, 2 suppressed; nothing rolled yet.
	if len(fd.delivered) != 2 {
		t.Fatalf("delivered = %d, want 2 (throttle)", len(fd.delivered))
	}
	if len(fs.appended) != 4 {
		t.Fatalf("appended = %d, want 4 (all persisted)", len(fs.appended))
	}

	// Roll the window; the next alert flushes the rollup for the 2 suppressed,
	// then forwards itself.
	clock = clock.Add(61 * time.Second)
	l.handle(ctx, alert("model-pool"))
	// deliveries: 2 + rollup + 1 = 4
	if len(fd.delivered) != 4 {
		t.Fatalf("delivered = %d, want 4 (2 + rollup + 1)", len(fd.delivered))
	}
	var sawRollup bool
	for _, d := range fd.delivered {
		if d.Origin == rollupOrigin {
			sawRollup = true
		}
	}
	if !sawRollup {
		t.Errorf("no rollup line delivered for the suppressed alerts")
	}
	// Suppressed rows were marked.
	suppressed := 0
	fs.mu.Lock()
	for _, s := range fs.marks {
		if s == store.ForwardSuppressed {
			suppressed++
		}
	}
	fs.mu.Unlock()
	if suppressed != 2 {
		t.Errorf("suppressed marks = %d, want 2", suppressed)
	}
}

// TestUnattachedPersistsOnly: with no deliverer, alerts are persisted but never
// delivered.
func TestUnattachedPersistsOnly(t *testing.T) {
	fs := newFakeStore()
	l := newLine(fs, nil)
	l.handle(context.Background(), alert("x"))
	if len(fs.appended) != 1 {
		t.Errorf("appended = %d, want 1", len(fs.appended))
	}
}

// TestNotifyQueueFullFallback: a full queue persists synchronously rather than
// dropping (the worker is not running here, so the buffer fills).
func TestNotifyQueueFullFallback(t *testing.T) {
	fs := newFakeStore()
	l := newLine(fs, nil)
	for i := 0; i < queueDepth+5; i++ {
		l.Notify(context.Background(), contract.AdminWarn, "flood", "n=%d", i)
	}
	// queueDepth queued (not yet persisted) + 5 spilled to synchronous persist.
	if len(fs.appended) < 5 {
		t.Errorf("synchronous fallback persisted %d, want >=5", len(fs.appended))
	}
}
