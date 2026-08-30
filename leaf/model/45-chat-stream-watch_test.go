package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

// fakeStreamChatter feeds a fixed sequence of StreamEvents into the
// pool's ChatStreamWatch read loop. Doesn't implement Chat (the test
// only exercises the streaming path).
type fakeStreamChatter struct {
	events []contract.StreamEvent
}

func (f *fakeStreamChatter) ChatStream(ctx context.Context, _ string, _ []contract.Message, _ []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	out := make(chan contract.StreamEvent, len(f.events)+1)
	go func() {
		defer close(out)
		for _, ev := range f.events {
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}

func (f *fakeStreamChatter) Chat(_ context.Context, _ string, _ []contract.Message, _ []contract.ToolSpec) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, errors.New("Chat not used in streaming tests")
}
func (f *fakeStreamChatter) Ping(_ context.Context) error { return nil }

// TestChatStreamWatch_AccumulatesText — basic streaming text builds the
// final ChatResponse.Content correctly.
func TestChatStreamWatch_AccumulatesText(t *testing.T) {
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			{Text: "hello "},
			{Text: "world"},
			{Done: true, Usage: contract.Usage{InputTokens: 5, OutputTokens: 2}},
		},
	})
	p := newTestPool(row)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}
	if resp.Content != "hello world" {
		t.Errorf("Content wrong: %q", resp.Content)
	}
	if resp.Usage.OutputTokens != 2 {
		t.Errorf("Usage.OutputTokens=%d want 2", resp.Usage.OutputTokens)
	}
}

// TestChatStreamWatch_AccumulatesToolCallDeltas — ToolDelta events
// reconstruct a complete ToolCall with concatenated arguments.
func TestChatStreamWatch_AccumulatesToolCallDeltas(t *testing.T) {
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			{ToolDelta: &contract.ToolCallDelta{Index: 0, ID: "t1", Name: "patch"}},
			{ToolDelta: &contract.ToolCallDelta{Index: 0, ArgumentsDelta: `{"path":"`}},
			{ToolDelta: &contract.ToolCallDelta{Index: 0, ArgumentsDelta: `orc.md"}`}},
			{Done: true},
		},
	})
	p := newTestPool(row)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d: %#v", len(resp.ToolCalls), resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "t1" || tc.Name != "patch" {
		t.Errorf("tool_call meta wrong: %#v", tc)
	}
	if tc.Arguments != `{"path":"orc.md"}` {
		t.Errorf("tool_call args wrong: %q", tc.Arguments)
	}
}

// TestChatStreamWatch_EmptyToolArgsNormalized — a tool_call that never
// receives any ArgumentsDelta (no-param tool: anthropic emits no
// input_json_delta, openai-compat may send only the name) must finalize
// with Arguments=="{}" to match the block path
// (providers/30-anthropic.go), not "" which is invalid JSON for the
// dispatch-side json.RawMessage decode.
func TestChatStreamWatch_EmptyToolArgsNormalized(t *testing.T) {
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			{ToolDelta: &contract.ToolCallDelta{Index: 0, ID: "t1", Name: "now"}},
			{Done: true},
		},
	})
	p := newTestPool(row)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d: %#v", len(resp.ToolCalls), resp.ToolCalls)
	}
	if got := resp.ToolCalls[0].Arguments; got != "{}" {
		t.Errorf("empty tool args should normalize to %q, got %q", "{}", got)
	}
}

// TestChatStreamWatch_InlineToolCallSalvaged — a Qwen/Hermes-style
// fine-tune that emits <tool_call>{…}</tool_call> as PLAIN TEXT (no
// structured ToolDelta) must have the marker promoted into resp.ToolCalls
// and stripped from resp.Content — mirroring the block path's salvage in
// providers/00-client.go so streaming and non-streaming behave identically.
func TestChatStreamWatch_InlineToolCallSalvaged(t *testing.T) {
	// Split the marker across several text deltas to prove accumulation +
	// post-stream extraction (the stream never carries a ToolDelta).
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			{Text: "let me edit that "},
			{Text: `<tool_call>{"name":"patch",`},
			{Text: `"arguments":{"path":"orc.md","append":"hi"}}`},
			{Text: "</tool_call>"},
			{Done: true, Usage: contract.Usage{InputTokens: 9, OutputTokens: 4}},
		},
	})
	p := newTestPool(row)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 salvaged tool_call, got %d: %#v", len(resp.ToolCalls), resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "patch" {
		t.Errorf("salvaged tool_call name = %q, want patch", tc.Name)
	}
	if tc.Arguments != `{"path":"orc.md","append":"hi"}` {
		t.Errorf("salvaged tool_call args = %q", tc.Arguments)
	}
	// Markup must be stripped; the surrounding prose stays (TrimSpace'd).
	if strings.Contains(resp.Content, "<tool_call>") || strings.Contains(resp.Content, "</tool_call>") {
		t.Errorf("inline markup not stripped from Content: %q", resp.Content)
	}
	if resp.Content != "let me edit that" {
		t.Errorf("cleaned Content = %q, want %q", resp.Content, "let me edit that")
	}
}

// TestChatStreamWatch_StructuredToolCallNoInlineSalvage — when a real
// structured ToolDelta arrives, the inline salvage step must NOT run even
// if the text content also happens to contain a <tool_call> marker. The
// structured call is authoritative; double-extraction would fire the same
// tool twice.
func TestChatStreamWatch_StructuredToolCallNoInlineSalvage(t *testing.T) {
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			// A stray marker in text that must be left alone because a
			// structured tool_call is present.
			{Text: `<tool_call>{"name":"other","arguments":{}}</tool_call>`},
			{ToolDelta: &contract.ToolCallDelta{Index: 0, ID: "t1", Name: "patch"}},
			{ToolDelta: &contract.ToolCallDelta{Index: 0, ArgumentsDelta: `{"path":"orc.md"}`}},
			{Done: true},
		},
	})
	p := newTestPool(row)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}
	// Exactly the structured call — no inline double-extraction.
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected exactly 1 tool_call (structured only), got %d: %#v", len(resp.ToolCalls), resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "t1" || tc.Name != "patch" || tc.Arguments != `{"path":"orc.md"}` {
		t.Errorf("structured tool_call altered by inline salvage: %#v", tc)
	}
	// Inline salvage (double-extraction) didn't run — the structured call is
	// authoritative. But the residual plain `<tool_call>` markup that rode along
	// in the text is stripped rather than leaked to the user (B8), mirroring the
	// sidecar strip. The block above was complete, so Content is left empty.
	if strings.Contains(resp.Content, "<tool_call>") {
		t.Errorf("residual plain tool_call markup leaked despite structured tool_call present (B8); Content = %q", resp.Content)
	}
}

// TestChatStreamWatch_WatcherCancelsStream — watcher returns an error
// after the second delta; ChatStreamWatch returns that error wrapped
// and downstream events are drained.
func TestChatStreamWatch_WatcherCancelsStream(t *testing.T) {
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			{Text: "hi"},
			{Text: " there"},
			{Text: " more"},
			{Done: true},
		},
	})
	p := newTestPool(row)
	defer p.Close()

	tripErr := errors.New("watcher abort")
	calls := 0
	watcher := func(_ contract.StreamEvent, _ *contract.StreamAccumulator) error {
		calls++
		if calls == 2 {
			return tripErr
		}
		return nil
	}
	_, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, watcher)
	if !errors.Is(err, tripErr) {
		t.Errorf("expected watcher error propagated, got: %v", err)
	}
}

// TestToolArgsBloatWatcher_Trips — accumulator with one tool_call
// whose arguments exceed the threshold trips the watcher with
// ErrToolArgsBloat.
func TestToolArgsBloatWatcher_Trips(t *testing.T) {
	accum := &contract.StreamAccumulator{
		ToolCalls: []contract.ToolCall{
			{Name: "patch", Arguments: strings.Repeat("x", ToolArgsBloatThreshold+1)},
		},
	}
	err := ToolArgsBloatWatcher(contract.StreamEvent{}, accum)
	if !errors.Is(err, ErrToolArgsBloat) {
		t.Errorf("expected ErrToolArgsBloat, got: %v", err)
	}
}

// TestToolArgsBloatWatcher_BelowThreshold — short args, no trip.
func TestToolArgsBloatWatcher_BelowThreshold(t *testing.T) {
	accum := &contract.StreamAccumulator{
		ToolCalls: []contract.ToolCall{
			{Name: "clarify", Arguments: `{"question":"how long?"}`},
		},
	}
	if err := ToolArgsBloatWatcher(contract.StreamEvent{}, accum); err != nil {
		t.Errorf("short args should not trip: %v", err)
	}
}

// TestChatStreamWatch_BloatWatcherIntegration — full path: stream
// pumps tool_call deltas past the threshold, the built-in
// ToolArgsBloatWatcher trips, ChatStreamWatch returns ErrToolArgsBloat.
func TestChatStreamWatch_BloatWatcherIntegration(t *testing.T) {
	// Build deltas that cross the threshold (relative, so the test stays
	// valid if ToolArgsBloatThreshold is retuned).
	chunk := strings.Repeat("x", 500)
	events := []contract.StreamEvent{
		{ToolDelta: &contract.ToolCallDelta{Index: 0, ID: "t1", Name: "patch"}},
	}
	// Pump enough 500-rune chunks to clear the threshold with margin.
	nChunks := ToolArgsBloatThreshold/500 + 2
	for i := 0; i < nChunks; i++ {
		events = append(events, contract.StreamEvent{ToolDelta: &contract.ToolCallDelta{Index: 0, ArgumentsDelta: chunk}})
	}
	events = append(events, contract.StreamEvent{Done: true})

	row := entry("smart", 0, &fakeStreamChatter{events: events})
	p := newTestPool(row)
	defer p.Close()

	_, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, ToolArgsBloatWatcher)
	if !errors.Is(err, ErrToolArgsBloat) {
		t.Fatalf("expected ErrToolArgsBloat, got: %v", err)
	}
	if !errors.Is(err, ErrStreamWatcherTripped) {
		t.Errorf("ErrToolArgsBloat must wrap ErrStreamWatcherTripped, got: %v", err)
	}
}

// TestChatStreamWatch_BloatProtectionAutoOnToolRound — the turn core passes only
// its own watcher (it can't import this package), so the pool must AUTO-apply
// ToolRoundWatcher on a tool round (req.Tools set). Same bloating stream as above
// but with a NIL caller watcher: the abort must still trip via the auto-compose.
func TestChatStreamWatch_BloatProtectionAutoOnToolRound(t *testing.T) {
	chunk := strings.Repeat("x", 500)
	events := []contract.StreamEvent{
		{ToolDelta: &contract.ToolCallDelta{Index: 0, ID: "t1", Name: "patch"}},
	}
	for i := 0; i < ToolArgsBloatThreshold/500+2; i++ {
		events = append(events, contract.StreamEvent{ToolDelta: &contract.ToolCallDelta{Index: 0, ArgumentsDelta: chunk}})
	}
	events = append(events, contract.StreamEvent{Done: true})

	row := entry("smart", 0, &fakeStreamChatter{events: events})
	p := newTestPool(row)
	defer p.Close()

	// nil caller watcher + a tool round (Tools advertised) → pool composes
	// ToolRoundWatcher → bloat still aborts.
	_, err := p.ChatStreamWatch(context.Background(),
		contract.ModelRequest{Tools: []contract.ToolSpec{{Name: "patch"}}}, nil, nil)
	if !errors.Is(err, ErrToolArgsBloat) {
		t.Fatalf("auto-compose should abort bloat on a tool round with nil watcher, got: %v", err)
	}

	// Contrast: NO tools advertised + nil watcher → no auto-compose → the same
	// bloating stream completes (no protection applied), proving the gate.
	row2 := entry("smart", 0, &fakeStreamChatter{events: events})
	p2 := newTestPool(row2)
	defer p2.Close()
	if _, err := p2.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil); err != nil {
		t.Fatalf("no-tools + nil watcher should not auto-apply bloat protection, got: %v", err)
	}
}

// TestChatStreamWatch_SuccessRecordsBookkeeping is the
// regression: a clean watch-stream success must flow through
// recordChatSuccess — bumping totalCalls AND resetting the entry's
// consecutiveFails (liveness). Before the fix streamOneWatch silently
// returned without either, so the main tool-round path starved
// /model + bob_model_usage of traffic and let a healthy backend
// accumulate consecutiveFails toward a false death.
func TestChatStreamWatch_SuccessRecordsBookkeeping(t *testing.T) {
	row := entry("smart", 0, &fakeStreamChatter{
		events: []contract.StreamEvent{
			{Text: "ok"},
			{Done: true, Usage: contract.Usage{InputTokens: 7, OutputTokens: 3}},
		},
	})
	// Prime the entry near the false-death threshold to prove success
	// clears it.
	row.consecutiveFails = consecutiveFailThreshold - 1

	p := newTestPool(row)
	defer p.Close()

	_, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}

	row.mu.Lock()
	gotCalls := row.totalCalls
	gotFails := row.consecutiveFails
	gotIn := row.totalInputTokens
	gotOut := row.totalOutputTokens
	row.mu.Unlock()

	if gotCalls != 1 {
		t.Errorf("totalCalls = %d, want 1 (success bookkeeping skipped)", gotCalls)
	}
	if gotFails != 0 {
		t.Errorf("consecutiveFails = %d, want 0 (success must reset liveness)", gotFails)
	}
	if gotIn != 7 || gotOut != 3 {
		t.Errorf("usage totals = (in %d, out %d), want (7, 3)", gotIn, gotOut)
	}
}

// TestChatStreamWatch_FirstContentStallFailsOver is the
// regression for the missing first-content deadline: an entry that
// accepts the request then produces no content must be cut at
// firstContentTimeout, take an errStreamStall liveness strike, and
// fail over to the next candidate — which serves the turn.
func TestChatStreamWatch_FirstContentStallFailsOver(t *testing.T) {
	orig := firstContentTimeout
	firstContentTimeout = 20 * time.Millisecond
	defer func() { firstContentTimeout = orig }()

	stallC := &fakeChatter{stall: true}
	liveC := &fakeChatter{text: "served"}
	rowA := entry("a", 0, stallC)
	rowB := entry("b", 0, liveC)
	// pick ranks Priority descending, so A (higher number) is tried first.
	rowA.info.Priority = 1
	rowB.info.Priority = 0
	p := newTestPool(rowA, rowB)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStreamWatch: %v", err)
	}
	if resp.Content != "served" || resp.Model != "b" {
		t.Errorf("failover wrong: content=%q model=%q", resp.Content, resp.Model)
	}

	// The stalled entry took a real liveness strike (errStreamStall via
	// recordError); the live entry recorded a success.
	rowA.mu.Lock()
	aFails := rowA.consecutiveFails
	rowA.mu.Unlock()
	if aFails == 0 {
		t.Errorf("stalled entry A consecutiveFails = 0, want >0 (stall must recordError)")
	}
	rowB.mu.Lock()
	bCalls := rowB.totalCalls
	rowB.mu.Unlock()
	if bCalls != 1 {
		t.Errorf("live entry B totalCalls = %d, want 1", bCalls)
	}
}

// TestChatStreamWatch_WatcherTrip_NoCandidateRetry — when a watcher
// trips, ChatStreamWatch does NOT failover to another candidate
// (it's a conversation-level abort, not entry-level).
func TestChatStreamWatch_WatcherTrip_NoCandidateRetry(t *testing.T) {
	a := &fakeStreamChatter{events: []contract.StreamEvent{
		{ToolDelta: &contract.ToolCallDelta{Index: 0, ID: "t1", Name: "p"}},
		{ToolDelta: &contract.ToolCallDelta{Index: 0, ArgumentsDelta: strings.Repeat("y", ToolArgsBloatThreshold+10)}},
		{Done: true},
	}}
	b := &fakeStreamChatter{events: []contract.StreamEvent{
		{Text: "should never run"},
		{Done: true},
	}}
	rowA := entry("a", 0, a)
	rowB := entry("b", 0, b)
	p := newTestPool(rowA, rowB)
	defer p.Close()

	_, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, ToolArgsBloatWatcher)
	if !errors.Is(err, ErrToolArgsBloat) {
		t.Fatalf("expected ErrToolArgsBloat, got: %v", err)
	}
}

// TestChatStreamWatch_MarkupTrip_CommittedSurfaces pins F71: the markup arrives as TEXT
// (so committed=true — visible content already streamed), and a markup trip after that
// must NOT fail over — re-streaming a second response into the same sink would leave a
// garbled duplicate on a non-edit sink. The pool surfaces ErrStreamCommitted (the turn
// then keeps the shown partial + an interrupt marker) while STILL recordError-ing the
// entry so a persistently-broken backend dies at the threshold across turns.
func TestChatStreamWatch_MarkupTrip_CommittedSurfaces(t *testing.T) {
	bad := &fakeStreamChatter{events: []contract.StreamEvent{
		{Text: `<|tool_call>call:todo{todos:[{content:<|"|>x<|"|>}]}<tool_call|>`},
		{Done: true},
	}}
	good := &fakeStreamChatter{events: []contract.StreamEvent{
		{Text: "served"},
		{Done: true, Usage: contract.Usage{InputTokens: 5, OutputTokens: 2}},
	}}
	rowA := entry("bad", 0, bad)
	rowB := entry("good", 0, good)
	rowA.info.Priority = 1 // pick ranks Priority desc → A tried first
	rowB.info.Priority = 0
	p := newTestPool(rowA, rowB)
	defer p.Close()

	resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, ToolRoundWatcher)
	if !errors.Is(err, ErrStreamCommitted) {
		t.Fatalf("markup trip after committed content must surface ErrStreamCommitted (no failover), got resp=%q err=%v", resp.Content, err)
	}
	if resp.Content == "served" {
		t.Error("must NOT fail over to the clean entry after content was committed (garbled duplicate)")
	}
	// The broken entry is still recorded exactly once (die-at-threshold machinery intact,
	// no phantom-success reset / double-count).
	rowA.mu.Lock()
	aFails := rowA.consecutiveFails
	rowA.mu.Unlock()
	if aFails != 1 {
		t.Errorf("markup entry consecutiveFails = %d, want exactly 1 (one recordError, no double-count / phantom-success reset)", aFails)
	}
}

// TestChatStreamWatch_CommittedNoFailover: a mid-stream provider error AFTER content
// was already streamed (committed) must NOT fail over — failing over re-streams a
// second response into the SAME sink (garbled duplicate). Before any content, it
// still fails over to recover a complete answer (the documented invariant).
func TestChatStreamWatch_CommittedNoFailover(t *testing.T) {
	streamErr := errors.New("provider SSE broke mid-stream")

	t.Run("committed → surface, no failover", func(t *testing.T) {
		a := entry("a", 0, &fakeStreamChatter{events: []contract.StreamEvent{
			{Text: "partial "},
			{Err: streamErr},
		}})
		b := entry("b", 0, &fakeStreamChatter{events: []contract.StreamEvent{
			{Text: "FROM_B"}, {Done: true},
		}})
		p := newTestPool(a, b)
		defer p.Close()

		var seen strings.Builder
		watcher := func(ev contract.StreamEvent, _ *contract.StreamAccumulator) error {
			seen.WriteString(ev.Text)
			return nil
		}
		_, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, watcher)
		if !errors.Is(err, ErrStreamCommitted) {
			t.Fatalf("committed mid-stream error must surface as ErrStreamCommitted, got %v", err)
		}
		if strings.Contains(seen.String(), "FROM_B") {
			t.Errorf("must NOT fail over after content committed — sink saw entry b: %q", seen.String())
		}
		if seen.String() != "partial " {
			t.Errorf("sink should hold only entry a's committed partial, got %q", seen.String())
		}
	})

	t.Run("pre-content → fails over", func(t *testing.T) {
		a := entry("a", 0, &fakeStreamChatter{events: []contract.StreamEvent{
			{Err: streamErr}, // errors before any content lands
		}})
		b := entry("b", 0, &fakeStreamChatter{events: []contract.StreamEvent{
			{Text: "recovered"}, {Done: true},
		}})
		p := newTestPool(a, b)
		defer p.Close()

		resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{}, nil, nil)
		if err != nil {
			t.Fatalf("a pre-content error must fail over cleanly, got %v", err)
		}
		if resp.Content != "recovered" {
			t.Errorf("expected failover to entry b (%q), got %q", "recovered", resp.Content)
		}
	})
}
