package compose

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// The static prompt layers + their runtime-file load/seed mechanics, shared by all
// flows. Each layer has a built-in const default and a runtime-editable file at
// $BOB_HOME/prompt/<name>.md. The const is the seed (written once if the file is
// absent); the file is the live source — an operator, or agora's per-(company,role)
// override, edits the file and the next turn picks it up. memory/platform are NOT
// here — those come from their subsystems (or, for agora, the per-turn directory +
// playbook the flow injects).

// identityLayer is the turn-lite system prompt — a single identity layer. The reply
// language MIRRORS the user rather than being pinned to Chinese: the harness's own
// notices localize off the ingress-resolved language (i18n.Detect → "zh"/"default"),
// but that resolution never reaches the model, so a hardcoded "回复中文" answered an
// English sender in Chinese. Left to the model, which reads the actual message and
// distinguishes languages i18n.Detect's two-bucket MVP cannot.
const identityLayer = "你是 bob，一个乐于助人的助手。回复简洁自然，并且用用户说话的语言回复（用户说中文就用中文，说英文就用英文）。"

// Static main-prompt layers ported from skeleton (prompt/60-source-static.go).
// These are const-only for now: the strings are copied verbatim (Chinese).

// constitutionLayer is the top "you are an honest AI" layer (skeleton's
// defaultConstitution). Pure static text, no name placeholder.
const constitutionLayer = `你是个诚实的 AI 助手，忠实的执行指令是你的信条。`

// preferencesLayer is the tool-habit / capability-shortfall layer (skeleton's
// defaultPreferences).
const preferencesLayer = `你习惯使用工具，并遵守工具的约定。如果被要求执行无法做到的事情，例如视频附件不能看，承认收到即可并告知你做不到。`

// toolPolicyLayer is the cross-cutting tool-discipline appendix. Tool-AGNOSTIC by
// design: tool NAMES and per-tool protocol come from each tool's own spec in the
// bag (plus its learned addendum), NOT this layer. The skeleton's verbatim text
// named tools/modes trunk doesn't wire (mode=get / DNS breaker / scrapling chains
// / specific tool names), feeding the main model a phantom protocol it could even
// echo back — a prompt-hygiene + correctness problem (S-1). Keep this generic;
// specific protocol returns WITH the tools, carried by their specs.
const toolPolicyLayer = `# 工具使用纪律
- 工具结果里带 "hint" 字段时必须遵循它。
- 同一工具对同一目标连续失败两次，就换方法或直接承认做不到，不要反复重试同一调用。
- 搜索/查找没有结果时可以回答"不知道"，但要说明你查过什么；回答必须有实据。
- 做不到的事（例如看不了的附件）就承认收到并说明做不到，不要假装完成。`

// LoopingDiscipline is the work-discipline prompt layer a LOOPING turn gets
// (docs/turn-driver-split.md §4/§5). PROMPT string (code, Chinese — model-facing,
// not a user notice). It states the mode's contract: convergence exit (deliver
// when genuinely done), quiet process (nothing renders until the final delivery),
// and honest self-containment — the three things a model bred on chat turns gets
// wrong in a work turn. Shared here (not per-flow): flow/normal injects it for a
// looping-STAMPED session, flow/agora for EVERY member turn (agora workers always
// run the looping driver). Const-only — NOT a loadable/editable layer file.
const LoopingDiscipline = "当前处于长任务工作模式：\n" +
	"- 持续工作直到任务真正完成再交付，不要中途汇报或提前收尾；把任务拆成步骤逐一做完。\n" +
	"- 你的工作过程对用户不可见（不流式展示），最终回复是用户唯一看到的东西——它必须自包含：结论、关键依据、产出位置，以及任何没做完/不确定的部分如实列明。\n" +
	"- 上一条例外：对话中途出现新的用户消息（标着「工作途中追加」），是用户在你干活期间又说了话。不要因此丢下原任务，但**最终回复里必须正面回应它**——用户看不到过程，你不回应就等于他没被听见。\n" +
	"- 遇到信息缺口先用工具补齐；确实补不齐的，把缺口写进交付说明，不要编造。"

// advisorCriteriaLayer is the DEFAULT acceptance standard the harness-triggered
// advisor judges a delivery against (docs/advisor.md §4). Unlike its four
// neighbours below it is NOT a main-prompt layer — it is seeded + edited as a file
// like them, but only ever reaches the advisor's own sub-turn brief (via
// TurnSpec.AdvisorCriteria): the standard must not be shown to the model being
// judged. PROMPT string (Chinese — model-facing). Deliberately ends on the
// don't-nitpick clause: a reviewer with no explicit criteria drifts into grading
// polish, and a redo costs a whole extra sub-turn.
const advisorCriteriaLayer = `# 交付验收准则

判断这次交付能不能发出去。逐条对照：

- **自包含**：结论、关键依据、产出位置都在交付文本里，读的人不需要回看过程。
- **有据**：声称做过的事在记录里能找到对应的成功结果（写了文件、发了消息、查到了数据）；
  声称的事实有本轮实际取得的证据支撑，凭印象转述不算。
- **不掩盖**：没做完、没拿到、不确定的部分在交付里如实写明，不能省略或含糊带过。
- **对题**：回应的是用户真正要的东西，不是一个相邻的、更容易做的替代品。

**只判能不能交付，不追求完美。**措辞、排版、可以更好但不影响使用的细节，一律放行；
只有关键内容缺失、或存在与记录不符的陈述，才判不合格。`

// mainPromptLayers are the static text layers a flow seeds AND renders into the main
// system prompt (SetStaticLayers reads them back by the same name).
var mainPromptLayers = []struct{ name, def string }{
	{"constitution", constitutionLayer},
	{"identity", identityLayer},
	{"preferences", preferencesLayer},
	{"tool_policy", toolPolicyLayer},
}

// seedOnlyLayers are seeded + edited like the main layers but NEVER rendered into a
// system prompt — they are read on demand by whoever owns them. Kept as its own list
// rather than a comment on a shared one: "seed everything in the list, render
// everything in the list" is the obvious refactor, and for the advisor's acceptance
// standard rendering it would hand the model being judged its own grading sheet
// (docs/advisor.md §4). TestStaticLayers_AdvisorNeverRenders pins this.
var seedOnlyLayers = []struct{ name, def string }{
	{"advisor", advisorCriteriaLayer},
}

// promptDir is $home/prompt — the runtime-editable layer files live here.
func promptDir(home string) string { return filepath.Join(home, "prompt") }

// seedPromptFiles writes every editable text's default to its file IF the file is
// absent — a fresh deploy gets editable copies, an already-edited file is never
// clobbered. Covers BOTH lists: the main prompt layers and the seed-only ones (the
// advisor's acceptance standard), which are editable the same way but never render
// into a system prompt. Best-effort: on failure loadPromptLayer falls back to the const.
func seedPromptFiles(home string) {
	dir := promptDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("compose: cannot create prompt dir; layers fall back to defaults", "dir", dir, "err", err)
		return
	}
	for _, l := range append(append([]struct{ name, def string }{}, mainPromptLayers...), seedOnlyLayers...) {
		path := filepath.Join(dir, l.name+".md")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// Atomic seed: write a temp then rename, so a crash mid-write can't leave a
			// PARTIAL layer file that loadPromptLayer would then load verbatim. Never-clobber
			// semantics preserved (we only get here when the target is absent).
			tmp := path + ".tmp"
			if werr := os.WriteFile(tmp, []byte(l.def), 0o644); werr != nil {
				slog.Warn("compose: seed prompt file failed", "path", path, "err", werr)
				continue
			}
			if werr := os.Rename(tmp, path); werr != nil {
				slog.Warn("compose: seed prompt rename failed", "path", path, "err", werr)
				_ = os.Remove(tmp)
			}
		}
	}
}

// loadPromptLayer reads a layer's runtime file, falling back to the built-in
// default on any error (missing/unreadable). Read per turn so an edit applies
// without a restart.
func loadPromptLayer(home, name, def string) string {
	b, err := os.ReadFile(filepath.Join(promptDir(home), name+".md"))
	// Empty/whitespace counts as "no content" → fall back to the const, so an
	// operator-blanked file can't silently strip a core layer like
	// constitution/identity from the system prompt.
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return def
	}
	return string(b)
}
