package turn

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
)

// arbiterPool scripts the two judge tiers: Chat serves the BLIND judge (always
// fail), ChatStreamWatch serves the arbiter sub-turn's rounds with a canned
// verdict. subCalls counts arbiter sub-turn rounds (the once-per-turn cap).
type arbiterPool struct {
	fakePool
	blind    string // blind judge verdict JSON (Chat)
	arbiter  string // arbiter sub reply (ChatStreamWatch)
	subCalls int
}

func (p *arbiterPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{Content: p.blind}, nil
}

func (p *arbiterPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.subCalls++
	accum := &contract.StreamAccumulator{Text: p.arbiter}
	_ = w(contract.StreamEvent{Text: p.arbiter}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{Content: p.arbiter}, nil
}

func arbiterFixture(pool contract.ModelPool) (*core, contract.TurnSpec, *turnState) {
	c := &core{store: newSubStore(), pool: pool}
	st := newTurnState()
	st.rubricArmed = map[string]string{"报告": "结论必须有引用支撑"}
	spec := contract.TurnSpec{Sid: "arb", UserText: "写报告", Sink: &recSink{}, Prompt: sysPrompt("sys")}
	return c, spec, st
}

// TestArbiter_OverturnsBlindFail: blind FAIL + arbiter PASS → the gate opens
// (verdict superseded), traced as an overturn.
func TestArbiter_OverturnsBlindFail(t *testing.T) {
	pool := &arbiterPool{blind: `{"pass":false,"reason":"缺引用"}`, arbiter: `{"pass":true,"reason":"引用在第3轮工具结果里，初审没看到"}`}
	c, spec, st := arbiterFixture(pool)

	blocked, judged, _ := c.judgeWithEscalation(context.Background(), spec, st, "产出X", nil)
	if blocked || !judged {
		t.Fatalf("blocked=%v judged=%v, want overturned (open, judged)", blocked, judged)
	}
	if pool.subCalls == 0 {
		t.Fatal("arbiter sub-turn never ran")
	}
}

// TestArbiter_ConfirmsWithRecordBackedReason: blind FAIL + arbiter FAIL → still
// blocked, and the arbiter's (evidence-backed) reason supersedes the blind one.
func TestArbiter_ConfirmsWithRecordBackedReason(t *testing.T) {
	pool := &arbiterPool{blind: `{"pass":false,"reason":"缺引用"}`, arbiter: `{"pass":false,"reason":"声称引用第3轮，但记录里第3轮是另一份文件"}`}
	c, spec, st := arbiterFixture(pool)

	blocked, _, reasons := c.judgeWithEscalation(context.Background(), spec, st, "产出X", nil)
	if !blocked || !strings.Contains(reasons, "另一份文件") {
		t.Fatalf("blocked=%v reasons=%q, want confirmed with the arbiter's reason", blocked, reasons)
	}
}

// TestArbiter_OncePerTurn: the second blind FAIL in the same turn does NOT buy a
// second sub-turn — the blind verdict stands unescalated.
func TestArbiter_OncePerTurn(t *testing.T) {
	pool := &arbiterPool{blind: `{"pass":false,"reason":"缺引用"}`, arbiter: `{"pass":true}`}
	c, spec, st := arbiterFixture(pool)

	if blocked, _, _ := c.judgeWithEscalation(context.Background(), spec, st, "产出1", nil); blocked {
		t.Fatal("first fail should be overturned by the arbiter")
	}
	calls := pool.subCalls
	if blocked, _, reasons := c.judgeWithEscalation(context.Background(), spec, st, "产出2", nil); !blocked || !strings.Contains(reasons, "缺引用") {
		t.Fatalf("second fail must stand WITH the fresh blind reason, got blocked=%v reasons=%q", blocked, reasons)
	}
	if pool.subCalls != calls {
		t.Fatalf("second escalation ran (%d → %d sub calls) — cap broken", calls, pool.subCalls)
	}
}

// TestArbiter_SubTurnNeverEscalates: IsSub turns keep the blind verdict (a sub
// calling runSubTurn would be depth 2 — hard rule).
func TestArbiter_SubTurnNeverEscalates(t *testing.T) {
	pool := &arbiterPool{blind: `{"pass":false,"reason":"缺引用"}`, arbiter: `{"pass":true}`}
	c, spec, st := arbiterFixture(pool)
	spec.IsSub = true

	if blocked, _, _ := c.judgeWithEscalation(context.Background(), spec, st, "产出", nil); !blocked {
		t.Fatal("a sub-turn must not escalate — blind verdict stands")
	}
	if pool.subCalls != 0 {
		t.Fatal("sub-turn escalation ran — depth-2 breach")
	}
}

// TestArbiter_UnparseableKeepsBlind: garbage from the arbiter → blind verdict
// stands (fail-open at the escalation level only; the gate stays closed).
func TestArbiter_UnparseableKeepsBlind(t *testing.T) {
	pool := &arbiterPool{blind: `{"pass":false,"reason":"缺引用"}`, arbiter: "我觉得还行吧"}
	c, spec, st := arbiterFixture(pool)

	blocked, _, reasons := c.judgeWithEscalation(context.Background(), spec, st, "产出", nil)
	if !blocked || !strings.Contains(reasons, "缺引用") {
		t.Fatalf("blocked=%v reasons=%q, want the blind verdict kept", blocked, reasons)
	}
}

// TestArbiter_SubErrorKeepsBlindAndBurnsCap: an errored arbiter sub keeps the
// blind verdict AND consumes the per-turn escalation (an advisor outage must not
// buy up to 3 × 600s retry stalls in one turn).
func TestArbiter_SubErrorKeepsBlindAndBurnsCap(t *testing.T) {
	pool := &arbiterPool{blind: `{"pass":false,"reason":"缺引用"}`, arbiter: ""} // empty product → no verdict
	c, spec, st := arbiterFixture(pool)

	blocked, _, reasons := c.judgeWithEscalation(context.Background(), spec, st, "产出", nil)
	if !blocked || !strings.Contains(reasons, "缺引用") {
		t.Fatalf("blocked=%v reasons=%q, want blind verdict kept", blocked, reasons)
	}
	if !st.arbiterUsed {
		t.Fatal("an errored escalation must still burn the per-turn cap")
	}
	calls := pool.subCalls
	if blocked, _, _ := c.judgeWithEscalation(context.Background(), spec, st, "产出", nil); !blocked || pool.subCalls != calls {
		t.Fatal("no retry after a burnt cap")
	}
}
