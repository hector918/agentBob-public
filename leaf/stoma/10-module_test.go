package stoma

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// fakeSource is a configurable contract.Source for driving the bus.
type fakeSource struct {
	name      string
	emit      []string // events to emit, then behave per runErr
	runErr    error    // returned after emitting; nil → block until ctx is done
	healthErr error    // what HealthCheck returns
}

func (f *fakeSource) Name() string        { return f.name }
func (f *fakeSource) Caps() contract.Caps { return contract.Caps{} }

func (f *fakeSource) HealthCheck(context.Context) error { return f.healthErr }

func (f *fakeSource) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	for _, t := range f.emit {
		select {
		case out <- contract.MessageEvent{Source: f.name, Text: t}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.runErr != nil {
		return f.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSource) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return nil
}
func (f *fakeSource) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (f *fakeSource) Send(context.Context, contract.Target, string) error             { return nil }
func (f *fakeSource) SendFile(context.Context, contract.Target, string, string) error { return nil }

// TestBus_FanInAndStop drives the bus through the trunk: a source's events
// arrive on Events(), SourceByName resolves, and Stop closes the channel.
// panelStub is a no-op contract.PanelRegistry so the bus can Start in isolation
// (in production the webui module provides the real one, which the bus Needs).
type panelStub struct{}

func (panelStub) RegisterPanel(contract.Panel) {}
func (panelStub) Panels() []contract.Panel     { return nil }

// regWithPanels returns a registry carrying the no-op panel registry the bus needs.
func regWithPanels() *trunk.Registry {
	reg := trunk.NewRegistry()
	trunk.Provide[contract.PanelRegistry](reg, panelStub{})
	return reg
}

func TestBus_FanInAndStop(t *testing.T) {
	src := &fakeSource{name: "x", emit: []string{"a", "b"}}
	reg := regWithPanels()
	m := New([]contract.Source{src})
	if err := m.Start(context.Background(), reg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	gw := trunk.Require[contract.Gateway](reg)

	for _, want := range []string{"a", "b"} {
		select {
		case ev := <-gw.Events():
			if ev.Text != want {
				t.Fatalf("event = %q, want %q", ev.Text, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for %q", want)
		}
	}
	if gw.SourceByName("x") != src {
		t.Fatal("SourceByName(x) did not return the source")
	}
	if gw.SourceByName("nope") != nil {
		t.Fatal("SourceByName(unknown) should be nil")
	}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case _, ok := <-gw.Events():
		if ok {
			t.Fatal("Events channel must be closed after Stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events channel not closed after Stop (timeout)")
	}
	if m.Health() != trunk.StateStopped {
		t.Fatalf("Health after Stop = %v, want Stopped", m.Health())
	}
}

// TestBus_DeadSourceMarked: a source whose Run errors out is marked dead in the
// health snapshot (a later successful probe must not resurrect it).
func TestBus_DeadSourceMarked(t *testing.T) {
	src := &fakeSource{name: "x", runErr: errors.New("boom")}
	m := New([]contract.Source{src})
	m.healthInterval = 0                            // disable the probe loop; we assert the Run-exit path only
	m.bootAt = time.Now().Add(-2 * bootRetryWindow) // steady state — no boot-window retry
	if err := m.Start(context.Background(), regWithPanels()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	// poll for the run goroutine to record the death.
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := m.Snapshot()
		if len(snap) == 1 && snap[0].Status == "dead" && snap[0].LastError == "boom" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("source not marked dead: %+v", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBus_ProbeMarksDegraded: consecutive probe failures cross the threshold
// and flip the source to degraded; a success resets it to ready.
func TestBus_ProbeMarksDegraded(t *testing.T) {
	probeErr := errors.New("unreachable")
	src := &fakeSource{name: "x", healthErr: probeErr}
	m := New([]contract.Source{src})
	ctx := context.Background()

	for i := 0; i < healthFailThreshold-1; i++ {
		m.probeAll(ctx)
		if s := m.Snapshot()[0]; s.Status != "ready" {
			t.Fatalf("after %d failures status = %q, want ready (below threshold)", i+1, s.Status)
		}
	}
	m.probeAll(ctx) // crosses the threshold
	if s := m.Snapshot()[0]; s.Status != "degraded" || s.LastError != "unreachable" {
		t.Fatalf("after threshold: %+v, want degraded/unreachable", s)
	}

	src.healthErr = nil // recovers
	m.probeAll(ctx)
	if s := m.Snapshot()[0]; s.Status != "ready" || s.FailStreak != 0 {
		t.Fatalf("after recovery: %+v, want ready/streak-0", s)
	}
}

func TestModule_Contract(t *testing.T) {
	m := New(nil)
	if m.Name() != "stoma" {
		t.Fatalf("Name = %q", m.Name())
	}
	if prov := m.Provides(); len(prov) != 1 || prov[0] != trunk.TypeOf[contract.Gateway]() {
		t.Fatalf("Provides = %v, want [contract.Gateway]", prov)
	}
	if needs := m.Needs(); len(needs) != 1 || needs[0] != trunk.TypeOf[contract.PanelRegistry]() {
		t.Fatalf("Needs = %v, want [contract.PanelRegistry]", needs)
	}
	if m.Optional() {
		t.Fatal("stoma must be critical (Optional=false)")
	}
	if m.Health() != trunk.StateNew {
		t.Fatalf("fresh Health = %v, want StateNew", m.Health())
	}
}

// panicLine is a contract.AdminLine whose Notify panics — modelling a misbehaving
// admin sink (nil deref / format misuse / third-party adapter bug). It closes
// entered first so the test can confirm the deliverer actually ran.
type panicLine struct{ entered chan struct{} }

func (p panicLine) Notify(context.Context, contract.AdminLevel, string, string, ...any) {
	close(p.entered)
	panic("admin sink boom")
}

// TestAlert_PanickingAdminLineDoesNotCrash pins the BUG-4-symmetric fix: alert fires
// Notify on a fresh goroutine with a deferred recover, so a panicking admin sink is
// logged + swallowed instead of crashing the whole bob process (a synchronous Notify,
// or an async one without recover, would take the process down here).
func TestAlert_PanickingAdminLineDoesNotCrash(t *testing.T) {
	entered := make(chan struct{})
	m := &Module{adminLine: func() contract.AdminLine { return panicLine{entered: entered} }}

	m.alert(contract.AdminError, "test", "source %q stopped: %v", "x", errors.New("boom"))

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("alert never invoked Notify")
	}
	// Reaching here (process alive) is the assertion — a missing recover would have
	// crashed the binary when the goroutine panicked. Settle so that crash would land.
	time.Sleep(50 * time.Millisecond)
}
