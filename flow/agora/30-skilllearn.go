package agora

import (
	"context"
	"encoding/json"
	"strings"

	"agentbob/contract"
)

// 30-skilllearn.go — the agora flow's skill failure-learning collection
// (docs/wip-skill-learn-reflect.md). On a DEGRADED member turn, read that turn's
// just-persisted messages, find the skills the model engaged (skill_view) + a medium
// trajectory snapshot, and report one per engaged skill to leaf/skills' failure sink;
// leaf/learn then reflects on the real trajectory into the skill's INSIGHT.md. No turn
// hook, no flow/normal — agora is where members do production work with skills.

const (
	// snapshotReplayCap bounds the history slice read to build a snapshot.
	snapshotReplayCap = 40
	// snapshotUserCap / snapshotReplyCap bound the snapshot's user-ask + output parts
	// (the action outline is naturally short). Keeps a snapshot "medium".
	snapshotUserCap  = 800
	snapshotReplyCap = 800
)

func (f *flow) msgs() contract.MessageStore { return f.msgStore.get(f.reg) }

func (f *flow) skillFailSink() contract.SkillFailureSink { return f.skillFail.get(f.reg) }

func (f *flow) memberFailSink() contract.MemberFailureSink { return f.memberFail.get(f.reg) }

// collectFailures snapshots a degraded member turn ONCE (reads the messages once) and
// feeds both learners (docs/wip-skill-learn-reflect.md + docs/wip-member-learn.md):
//   - member: EVERY degraded agora turn is a member failure — reported by scope (agora
//     resolves scope → the member's role(s)), regardless of any skill use.
//   - skill: reported per skill the model engaged via skill_view (none → skip skills).
//
// Absent both sinks / no session → no-op.
func (f *flow) collectFailures(ctx context.Context, spec contract.TurnSpec, res contract.TurnResult) {
	ms := f.msgs()
	skillSink := f.skillFailSink()
	memberSink := f.memberFailSink()
	if ms == nil || (skillSink == nil && memberSink == nil) {
		return
	}
	history, err := ms.GetReplay(ctx, spec.Sid, snapshotReplayCap)
	if err != nil || len(history) == 0 {
		return
	}
	engaged, actions, windowMiss := parseSkillTurn(history)
	// Prefer the rubric-rejected product over res.Reply: on a rubric give-up Reply is the
	// stock apology, so the learner must reflect on the REAL flawed output (the most common
	// skill-learning trigger). Non-rubric degraded turns carry no rejected product → Reply.
	product := res.RejectedProduct
	if product == "" {
		product = res.Reply
	}
	snapshot := buildSnapshot(spec.UserText, actions, product, windowMiss)
	if memberSink != nil {
		memberSink.RecordFailure(ctx, spec.Scope, snapshot)
	}
	if skillSink != nil {
		for skill := range engaged {
			skillSink.RecordFailure(skill, snapshot)
		}
	}
}

// parseSkillTurn scans THIS turn (from the last user row) for skills engaged via a
// SUCCESSFUL skill_view, plus an action outline (each tool call → 成功/失败). The
// window is bounded to this turn: if no user row is in the replay (only when this
// turn scrolled past the replay cap), it uses an EMPTY window rather than the whole
// slice — better to learn nothing this cycle than to fold an earlier turn's tool
// calls into this turn's failure snapshot. windowMiss reports that empty-window case
// so the snapshot can say "动作未知" instead of falsely "无工具调用".
func parseSkillTurn(history []contract.Message) (engaged map[string]bool, actions []string, windowMiss bool) {
	start := len(history) // no user row found → empty window (collect nothing)
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			start = i
			// A turn now persists one user row PER speaker — extend back through the
			// contiguous trailing user block so the whole turn (not just the last
			// speaker) is in the window.
			for start > 0 && history[start-1].Role == "user" {
				start--
			}
			break
		}
	}
	engaged = map[string]bool{}
	okByID := map[string]bool{}
	for _, m := range history[start:] {
		if m.Role == "tool" {
			okByID[m.ToolCallID] = envelopeOK(m.Content)
		}
	}
	for _, m := range history[start:] {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			ok := okByID[tc.ID]
			if tc.Name == "skill_view" && ok {
				if name := skillViewName(tc.Arguments); name != "" {
					engaged[name] = true
				}
			}
			actions = append(actions, tc.Name+"→"+okLabel(ok))
		}
	}
	// A miss = this turn scrolled past the replay cap, so no user row anchors the
	// window; distinguish it from a genuine no-tool turn (empty window found).
	return engaged, actions, start == len(history)
}

// envelopeOK parses the D1 tool-result envelope ({"ok":bool,...}) for its ok flag.
func envelopeOK(content string) bool {
	var e struct {
		OK bool `json:"ok"`
	}
	return json.Unmarshal([]byte(content), &e) == nil && e.OK
}

func skillViewName(args string) string {
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	return strings.TrimSpace(a.Name)
}

func okLabel(ok bool) string {
	if ok {
		return "成功"
	}
	return "失败"
}

// buildSnapshot renders the medium failure snapshot: user ask + action outline +
// degraded output, each part bounded. windowMiss=true means the action window
// scrolled past the replay cap (no user row anchored it), so the actions are UNKNOWN
// rather than genuinely empty — render honestly so a long tool-heavy turn isn't
// mislabeled "无工具调用".
func buildSnapshot(userText string, actions []string, reply string, windowMiss bool) string {
	var b strings.Builder
	b.WriteString("【用户请求】\n")
	b.WriteString(capRunes(strings.TrimSpace(userText), snapshotUserCap))
	b.WriteString("\n\n【本轮动作】\n")
	switch {
	case len(actions) > 0:
		b.WriteString(strings.Join(actions, "; "))
	case windowMiss:
		b.WriteString("（动作窗口超出回放上限，本轮动作未知）")
	default:
		b.WriteString("（无工具调用）")
	}
	b.WriteString("\n\n【未达标产出】\n")
	b.WriteString(capRunes(strings.TrimSpace(reply), snapshotReplyCap))
	return b.String()
}

func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
