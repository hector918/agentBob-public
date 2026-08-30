package trunk

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestHousekeeperFiresAndStops: a registered task runs on its period, and stop
// halts it.
func TestHousekeeperFiresAndStops(t *testing.T) {
	h := newHousekeeper()
	var mu sync.Mutex
	runs := 0
	h.Register(Task{Name: "t", Period: 10 * time.Millisecond, Run: func(context.Context) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	}})
	h.start()
	defer h.stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := runs
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task did not run at least twice; runs=%d", runs)
}

// TestHousekeeperPriorityOrder: when several tasks come due together, the worker
// runs them highest-Priority first.
func TestHousekeeperPriorityOrder(t *testing.T) {
	h := newHousekeeper()
	var mu sync.Mutex
	var order []string
	mk := func(name string, prio int) Task {
		return Task{Name: name, Period: 10 * time.Millisecond, Priority: prio, Run: func(context.Context) error {
			mu.Lock()
			if len(order) < 3 {
				order = append(order, name)
			}
			mu.Unlock()
			return nil
		}}
	}
	// Register low→high; the scheduler must still run high first.
	h.Register(mk("low", 1))
	h.Register(mk("high", 9))
	h.Register(mk("mid", 5))
	h.start()
	defer h.stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		got := append([]string(nil), order...)
		mu.Unlock()
		if n >= 3 {
			if got[0] != "high" || got[1] != "mid" || got[2] != "low" {
				t.Fatalf("priority order = %v, want [high mid low]", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("not all tasks ran; order=%v", order)
}

// TestHousekeeperPanicIsolated: a panicking task does not kill the scheduler;
// a sibling keeps running.
func TestHousekeeperPanicIsolated(t *testing.T) {
	h := newHousekeeper()
	var mu sync.Mutex
	good := 0
	h.Register(Task{Name: "boom", Period: 10 * time.Millisecond, Priority: 9, Run: func(context.Context) error {
		panic("boom")
	}})
	h.Register(Task{Name: "good", Period: 10 * time.Millisecond, Priority: 1, Run: func(context.Context) error {
		mu.Lock()
		good++
		mu.Unlock()
		return nil
	}})
	h.start()
	defer h.stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := good
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sibling task starved by a panicking task; good=%d", good)
}

// TestHousekeeperFailureBackoff: consecutive failures (error or panic) push a
// task's nextDue out exponentially, capped at failBackoffCap; one success
// resets the streak.
func TestHousekeeperFailureBackoff(t *testing.T) {
	h := newHousekeeper()
	h.ctx, h.cancel = context.WithCancel(context.Background())
	defer h.cancel()

	st := &scheduled{Task: Task{Name: "flaky", Period: time.Minute, Run: func(context.Context) error {
		return errors.New("nope")
	}}}

	before := time.Now()
	h.runOnce(st) // 1st failure → 2×Period
	if st.fails != 1 {
		t.Fatalf("fails = %d after one failure, want 1", st.fails)
	}
	if d := st.nextDue.Sub(before); d < 2*time.Minute {
		t.Fatalf("nextDue only %v out after 1st failure, want ≥ 2×Period", d)
	}
	h.runOnce(st) // 2nd failure → 4×Period
	if d := st.nextDue.Sub(before); d < 4*time.Minute {
		t.Fatalf("nextDue only %v out after 2nd failure, want ≥ 4×Period", d)
	}

	// A long streak stays bounded by the cap.
	for i := 0; i < 30; i++ {
		h.runOnce(st)
	}
	if d := time.Until(st.nextDue); d > failBackoffCap+time.Minute {
		t.Fatalf("backoff %v exceeds cap %v", d, failBackoffCap)
	}

	// Panic counts as a failure too.
	fails := st.fails
	st.Run = func(context.Context) error { panic("boom") }
	h.runOnce(st)
	if st.fails != fails+1 {
		t.Fatalf("panic did not count as failure: fails = %d, want %d", st.fails, fails+1)
	}

	// One success resets the streak.
	st.Run = func(context.Context) error { return nil }
	h.runOnce(st)
	if st.fails != 0 {
		t.Fatalf("fails = %d after success, want 0", st.fails)
	}

	// A task whose Period already exceeds the cap is never scheduled sooner
	// than its own Period.
	long := &scheduled{Task: Task{Name: "long", Period: 24 * time.Hour, Run: func(context.Context) error {
		return errors.New("nope")
	}}}
	long.nextDue = time.Now().Add(long.Period) // as collect() would set it
	want := long.nextDue
	h.runOnce(long)
	if long.nextDue.Before(want.Add(-time.Second)) {
		t.Fatalf("backoff pulled a long-period task earlier: nextDue %v, want ≥ %v", long.nextDue, want)
	}
}

// TestHousekeeperProvidedByTrunk: New publishes the housekeeper so a module can
// resolve it via TryRequire.
func TestHousekeeperProvidedByTrunk(t *testing.T) {
	tr := New()
	if _, ok := TryRequire[Housekeeper](tr.Registry()); !ok {
		t.Fatalf("trunk did not provide a Housekeeper")
	}
}
