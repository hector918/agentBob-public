// Package compose is the SHARED turn-composition machinery the flows reuse. Per
// architecture-vision §7 a flow copies ORCHESTRATION (a few lines) but NOT logic —
// the logic is shared here. flow/normal and flow/agora each hold their own STRATEGY
// (which identity to mint, which authority projects the tool/skill bag, which
// prompt layers carry what) but call into this package for the MECHANISM that is
// byte-identical between them: the static prompt layers, the user-text join, the
// attachment batching, the skill-layer assembly, and the warrant-bound channel
// opener. (Voice/audio ASR is NOT composed here — it runs at INGESTION, F165,
// folding the transcript into MessageEvent.Text before composition.)
//
// A Composer holds the shared deps a flow injects (the prompt factory and
// $BOB_HOME). The free funcs are pure and take their inputs as arguments — a flow
// calls them directly.
package compose

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
)

// Composer holds the shared composition deps a flow injects. It is the seam the
// flow hands its prompt factory and home dir to; the methods here are the
// composition MECHANISM both flows share.
type Composer struct {
	// Home is $BOB_HOME — roots the runtime-editable prompt-layer files.
	Home string
	// Prompts hands out fresh prompt builders (one per turn).
	Prompts contract.PromptFactory
}

// NewBuilder hands the flow a fresh prompt builder.
func (c *Composer) NewBuilder() contract.Prompt { return c.Prompts.New() }

// SeedPromptFiles writes each static layer's default to its $BOB_HOME/prompt file
// IF absent — a flow calls it once at Start so an operator (or agora's per-role
// override) has an editable copy. Delegates to seedPromptFiles.
func (c *Composer) SeedPromptFiles() { seedPromptFiles(c.Home) }

// SetStaticLayers pins the four static main-prompt layers (constitution / identity
// / preferences / tool_policy) on the builder, LOADED from their runtime files
// (seeded from the built-in const defaults). The order here IS their render order.
// A flow calls this then sets its own memory/skills/platform/runtime layers.
func (c *Composer) SetStaticLayers(p contract.Prompt) {
	p.SetLayer("constitution", loadPromptLayer(c.Home, "constitution", constitutionLayer))
	p.SetLayer("identity", loadPromptLayer(c.Home, "identity", identityLayer))
	p.SetLayer("preferences", loadPromptLayer(c.Home, "preferences", preferencesLayer))
	p.SetLayer("tool_policy", loadPromptLayer(c.Home, "tool_policy", toolPolicyLayer))
}

// AdvisorCriteria loads the acceptance standard the harness-triggered advisor judges
// a delivery against ($BOB_HOME/prompt/advisor.md, seeded from advisorCriteriaLayer;
// docs/advisor.md §4). A flow puts it on TurnSpec.AdvisorCriteria — NOT on the prompt
// builder: the standard must not be shown to the model being judged. Read per turn, so
// an edit applies without a restart; a blanked file falls back to the built-in default
// (an operator wiping the file must not silently disarm the acceptance gate).
func (c *Composer) AdvisorCriteria() string {
	return loadPromptLayer(c.Home, "advisor", advisorCriteriaLayer)
}

// ComposeSkills fills the "skills" prompt layer from the authorized projection: an
// index (name + description) the model reads on demand via skill_view. Empty
// projection → no layer. The index instructs the model to call skill_view, so it is
// rendered ONLY when skill_view is in the turn's tool bag: skill:use:X and
// tool:use:skill_view are independent grants, and telling the model to call a tool it
// cannot invoke would surface as an "unknown tool" error (a config gap masquerading as
// a hallucination) rather than a working skill index.
func ComposeSkills(p contract.Prompt, skills contract.SkillSet, tools contract.ToolSet) {
	if skills == nil {
		return
	}
	if tools == nil {
		return
	}
	if _, ok := tools.Lookup("skill_view"); !ok {
		return // skill_view not granted this turn — an index pointing at it would only mislead
	}
	var index strings.Builder
	for _, info := range skills.List() {
		index.WriteString("- " + info.Name)
		if len(info.Triggers) > 0 { // recall cues: anchors / discriminators (see docs/skill-recall-index.md)
			index.WriteString("（" + strings.Join(info.Triggers, "、") + "）")
		}
		index.WriteString("：" + info.Description + "\n")
	}
	if index.Len() == 0 {
		return
	}
	// Task-first framing: recall the METHOD before reaching for a tool (the method tells you
	// which tools to use). 括号里是触发线索,用来对上任务。
	layer := "做任务前先看下面有没有现成方法（技能）对得上；命中就用 skill_view 按名字读它的完整指引照着做（指引会告诉你用哪些工具）。括号里是该技能的触发线索：\n" + index.String()
	p.SetLayer("skills", strings.TrimRight(layer, "\n"))
}

// ComposeToolSelection fills the "tool_selection" layer: a scenario→tool rubric built
// from the SelectionHints of the tools VISIBLE this turn (sorted by Priority, small
// first), so the model has a "when to reach for which tool" map made of exactly the
// tools it can actually call. Tools without a hint contribute nothing; an empty set
// leaves the layer empty (Build skips it). See contract.SelectionHint.
func ComposeToolSelection(p contract.Prompt, tools contract.ToolSet) {
	if tools == nil {
		return
	}
	type hinted struct {
		name string
		hint *contract.SelectionHint
	}
	var hs []hinted
	for _, s := range tools.Specs() {
		if s.SelectionHint != nil {
			hs = append(hs, hinted{s.Name, s.SelectionHint})
		}
	}
	if len(hs) == 0 {
		return
	}
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].hint.Priority < hs[j].hint.Priority })
	var b strings.Builder
	b.WriteString("工具按场景选——命中下面哪条场景，就用那个工具：")
	for _, h := range hs {
		fmt.Fprintf(&b, "\n- 当【%s】→ 用 `%s`：%s", h.hint.When, h.name, h.hint.Then)
	}
	p.SetLayer("tool_selection", b.String())
}

// ComposeUserText joins the batch's user text, lists the turn's attachments by name,
// and appends any pre-extracted voice transcript. The attachment list is the model's
// only handle on a specific file: it is image-blind (no bytes), so to OCR the right
// image — or disambiguate several — it must reference a filename it can see here.
func ComposeUserText(events []contract.MessageEvent) string {
	return composeEvents(events)
}

// PreferTags resolves a main turn's soft model preference: the session's own
// armed tags when it has any (/uncensored stickiness, /model tag prefs — the
// user's word is exclusive, an ambient default must not tie against it),
// otherwise ["smart"] so conversation traffic ranks smart-class entries above
// small utility models WITHOUT a hard Requires — when every smart entry is
// dead or saturated the pick still degrades to whatever serves, instead of
// failing the turn. (Priority alone can't express this: it is one global
// number read in every tag circle, and the small models legitimately outrank
// their peers inside the compress circle.)
func PreferTags(session []string) []string {
	if len(session) > 0 {
		return session
	}
	return []string{"smart"}
}

// ComposeUserMsgs breaks the event batch into one UserMsg PER event — each with its own
// composed text (the event's text + its own attachment list + transcripts) and its sender
// (MsgAuthor). The turn persists one user row per entry, so a group's speakers stay
// individually attributed. Events with no renderable text are skipped.
func ComposeUserMsgs(events []contract.MessageEvent) []contract.UserMsg {
	out := make([]contract.UserMsg, 0, len(events))
	for _, ev := range events {
		text := composeOneEvent(ev)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, contract.UserMsg{
			Author:      eventAuthor(ev),
			Text:        text,
			Attachments: placedAttachments(ev.Attachments),
		})
	}
	return out
}

// eventAuthor attributes an inbound event's user row. An external event is a HUMAN
// speaker (id/name from the source). An INTERNAL event is an agent 回程 — an agora
// worker's output relayed via the bridge returnSink, or a dispatch/cron wake — so it
// is marked kind=agent (symmetric with the assistant-row agentAuthor), never landing
// in caller history as an anonymous human speaker. Empty id/name → "internal".
func eventAuthor(ev contract.MessageEvent) contract.MsgAuthor {
	if !ev.IsInternal() {
		return contract.MsgAuthor{Kind: "human", ID: ev.UserID, Name: ev.UserName}
	}
	id, name := ev.UserID, ev.UserName
	if id == "" {
		id = "internal"
	}
	if name == "" {
		name = "internal"
	}
	return contract.MsgAuthor{Kind: "agent", ID: id, Name: name}
}

// composeOneEvent renders ONE event's user text — ComposeUserText's body over a
// single-event slice.
func composeOneEvent(ev contract.MessageEvent) string {
	return composeEvents([]contract.MessageEvent{ev})
}

// composeEvents is the single renderer behind ComposeUserText and composeOneEvent:
// the joined text + one combined attachment list + the voice transcripts. Any new
// attachment-rendering rule lands here once, so the derived UserText view and the
// per-event UserMsgs can't drift apart.
func composeEvents(events []contract.MessageEvent) string {
	var b strings.Builder
	b.WriteString(joinEvents(events))
	if desc := describeAttachments(events); desc != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(desc)
	}
	// Voice/audio transcripts are folded into ev.Text at ingestion (session submit,
	// F165) — no separate "[语音转写]" section here any more.
	return strings.TrimSpace(b.String())
}

// describeAttachments renders the turn's DOWNLOADED attachments as a list so the model
// can reference a specific one. It shows each file's SPACE-RELATIVE PATH (e.g.
// "inbox/photo-222.jpg") — the exact string a tool's FileChannel reads — so the model
// passes it straight to image / a file-upload without any per-tool name→path
// resolver. Placement names the inbox file from the clean display name, so the path's
// base IS the friendly name; the path stays resolvable across turns (preserved in
// history) until the inbox is swept. The 图片 LABEL uses the same IsImageContent
// predicate as the ocr tool, so a file shown as 图片 is exactly what the tool accepts.
// Empty when none.
func describeAttachments(events []contract.MessageEvent) string {
	var lines []string
	for _, ev := range events {
		for _, a := range ev.Attachments {
			if a.Path == "" {
				// F56: a NAMED attachment the user shared that couldn't be downloaded /
				// placed (Path never set). The source appends it so the model KNOWS
				// something was shared — render an honest "couldn't read it" line rather
				// than dropping it (else a caption-bearing failed attachment is invisible
				// and the model answers as if it were plain text). Gated on a FileName so
				// an anonymous placeholder isn't listed. (A voice note whose transcription
				// failed carries its failnote in ev.Text + a placed Path, so it doesn't
				// reach this Path=="" branch — no double note.)
				if name := prompt.StripControl(a.FileName); name != "" {
					lines = append(lines, "- "+attachmentLabel(a)+" "+name+"：（未能读取——下载失败或超出大小限制）")
				}
				continue
			}
			// Render the Path, not the raw FileName: the Path is the tool-usable handle.
			// prompt.StripControl neutralises any control char so a crafted name can't inject
			// extra "- …" list lines — but it does NOT truncate (placement already bounds
			// the base; capping the path here would clip the tail/extension and the model's
			// echo would no longer resolve). The name was sanitised into the path at
			// placement; this is defence in depth.
			ref := prompt.StripControl(a.Path)
			if ref == "" {
				continue
			}
			line := "- " + attachmentLabel(a) + "：" + ref
			if a.FromReply {
				line += "（来自被回复的消息）"
			}
			// Say when the words are already here. A voice note rendered as a bare path is
			// indistinguishable from an untouched file, and the model pays a tool round to
			// fetch a transcript that is sitting a few lines above — measured at 16 s per
			// clip on, on top of the ~14 s ingestion already spent. The audio
			// tool's own selection hint says the same thing, but a hint is read AFTER the
			// tool is a candidate; this is what keeps it from becoming one.
			if a.Transcribed {
				line += "（内容已转写在上文，无需再取）"
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "[本轮附件]\n" + strings.Join(lines, "\n")
}

// attachmentLabel is the human-facing kind tag. Image content is labelled 图片 (via the
// shared contract predicate, so it matches what the ocr tool accepts) so the model
// knows it can OCR it.
func attachmentLabel(a contract.Attachment) string {
	if a.IsImageContent() {
		return "图片"
	}
	switch a.Kind {
	case "voice", "audio":
		return "音频"
	case "video":
		return "视频"
	case "document":
		return "文件"
	default:
		return "附件"
	}
}

// placedAttachments returns the attachments that were downloaded AND placed in the
// space (Path set) — the ones a tool can actually act on and the prompt listed. The
// turn persists these on the speaker's user row (UserMsg.Attachments) so the
// conversation's attachment set is recoverable from history. nil when none.
func placedAttachments(atts []contract.Attachment) []contract.Attachment {
	var out []contract.Attachment
	for _, a := range atts {
		if a.Path != "" {
			out = append(out, a)
		}
	}
	return out
}

// BatchAttachments flattens the event batch's attachments into one list for the
// turn (most sources carry a single event; a coalesced batch may carry several).
// The image tool anchors a model-blind image_path against this list.
func BatchAttachments(events []contract.MessageEvent) []contract.Attachment {
	var out []contract.Attachment
	for _, ev := range events {
		out = append(out, ev.Attachments...)
	}
	return out
}

// RenderRuntime is the computed bottom stable layer (skeleton's now-reference):
// the current DATE. DATE granularity is deliberate — it changes once a day, so the
// prefix prompt cache (llama.cpp cache_prompt) invalidates only daily, not per
// request. It sits last (just above the per-iter nudge) so every stable layer
// above it stays cached. Time-of-day precision would break the whole system+
// history cache every request, so it is kept OUT of the system prompt — the `now`
// tool serves precise time on demand. Set once per turn (turn-start date), stable
// across the turn's rounds.
func RenderRuntime() string {
	now := time.Now()
	weekday := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()]
	return "当前日期：" + now.Format("2006-01-02") + "（" + weekday + "）"
}

// EventsLang returns the first event's resolved reply language ("" if none → the
// i18n default). The ingress cascade already stamped ev.Lang on every event; the
// flows reuse it — to seed TurnSpec.Lang for the turn core's own notices, and for
// a pre-turn Sink.Finish reply built before a specific event is in hand — without
// re-detecting.
func EventsLang(evs []contract.MessageEvent) string {
	for _, e := range evs {
		if e.Lang != "" {
			return e.Lang
		}
	}
	return ""
}

// OneLine collapses a description to a single line for the /prompt dump's tool/skill
// list.
func OneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// joinEvents concatenates a batch's user text (one event for most sources; a
// drained pending batch may carry several). An event replying to an earlier
// message gets its quoted-reply context line prepended (docs/prompt.md §3.1).
func joinEvents(events []contract.MessageEvent) string {
	parts := make([]string, 0, len(events))
	for _, ev := range events {
		part := strings.TrimSpace(ev.Text)
		if line := prompt.ReplyLine(ev); line != "" {
			if part != "" {
				part = line + "\n" + part
			} else {
				part = line
			}
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}
