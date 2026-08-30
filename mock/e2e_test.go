package mock_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/flow/inbound"
	"agentbob/flow/normal"
	"agentbob/flow/router"
	"agentbob/heartwood/prompt"
	"agentbob/leaf/gate"
	"agentbob/leaf/model"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/session"
	"agentbob/leaf/slash"
	"agentbob/leaf/stoma"
	"agentbob/leaf/turn"
	"agentbob/mock"
	"agentbob/trunk"
)

// recSink captures the reply the pipeline renders and signals when Finish runs.
type recSink struct {
	full string
	done chan struct{}
}

func (s *recSink) ContentDelta(string) {}
func (s *recSink) TraceDelta(string)   {}
func (s *recSink) Finish(full string) error {
	s.full = full
	close(s.done)
	return nil
}
func (s *recSink) SendFile(string, string) error { return nil }
func (s *recSink) LastSent() string              { return "" }

// emitSource is a programmatic source: it emits one event, then hands its
// recSink to whatever turn replies. Stands in for the local REPL so the test
// drives the whole pipeline without stdin.
type emitSource struct {
	text string
	sink *recSink
}

func (emitSource) Name() string                      { return "local" }
func (emitSource) Caps() contract.Caps               { return contract.Caps{Trusted: true} } // local: exempt from the screen
func (emitSource) HealthCheck(context.Context) error { return nil }
func (s emitSource) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	ev := contract.MessageEvent{
		Source: "local", ChatType: contract.ChatDM,
		ChatID: "e2e", UserID: "u", IsAdmin: true, Text: s.text,
	}
	select {
	case out <- ev:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s emitSource) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return s.sink
}
func (emitSource) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (emitSource) Send(context.Context, contract.Target, string) error             { return nil }
func (emitSource) SendFile(context.Context, contract.Target, string, string) error { return nil }

// TestE2E_FullPipeline boots the whole trunk (pgpool + gate + model + session +
// turn + stoma + flow) against a mock OpenAI-compat backend and asserts a real
// reply flows end to end: a programmatic source emits a message, and the mock's
// reply comes back through the model pool → provider client (real HTTP/SSE) →
// turn → the source's sink.
//
// Env-gated on PGPOOL_TEST_DSN (the throwaway rebuild Postgres). No real LLM, no
// network — everything but the model's intelligence is real.
func TestE2E_FullPipeline(t *testing.T) {
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run the end-to-end mock test")
	}

	srv := mock.NewModelServer(func(lastUser string) string { return "收到：" + lastUser })
	defer srv.Close()

	home := t.TempDir()
	yaml := "entries:\n  - name: mock\n    provider: openai\n    model: mock\n    base_url: " +
		srv.URL + "/v1\n    tags: [smart]\n"
	if err := os.WriteFile(filepath.Join(home, "models.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	sink := &recSink{done: make(chan struct{})}
	src := emitSource{text: "你好 bob", sink: sink}

	tk := trunk.New()
	tk.Register(pgpool.New(dsn))
	tk.Register(gate.New(filepath.Join(home, "sources"))) // empty dir → no policies
	tk.Register(model.New(home))
	tk.Register(slash.New())
	tk.Register(session.New(session.Config{}))
	tk.Register(prompt.New())
	tk.Register(turn.New())
	tk.Register(router.New())
	tk.Register(normal.New(home))
	tk.Register(stoma.New([]contract.Source{src}))
	tk.Register(inbound.New())

	if err := tk.Start(context.Background()); err != nil {
		t.Fatalf("trunk start: %v", err)
	}
	defer tk.Stop(context.Background())

	select {
	case <-sink.done:
		if !strings.Contains(sink.full, "收到：你好 bob") {
			t.Fatalf("reply = %q, want it to contain the echoed message", sink.full)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reply reached the sink within 10s")
	}
	// Brief grace so the turn's post-Finish history persist completes before the
	// deferred Stop cancels the run context (the reply itself already landed).
	time.Sleep(200 * time.Millisecond)
}
