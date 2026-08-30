package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"agentbob/contract"
)

// read_conversation_log is CORE-OWNED machinery (docs/turn-driver-split.md §5,
// 内置拍板) with TWO injection sites and no catalog registration, warrant grant
// or suite naming: runSubTurn injects it into every ShareHistory sub-turn's bag
// (the advisor reads its DELEGATOR), and maybeArmHistoryTool injects it into a
// compacted MAIN turn's bag (the turn reads its OWN pre-summary originals —
// pull 化 P1). The warrant exemption is fenced by what the tool IS: read-only,
// pinned to the one sid ToolContext.History wraps, no cross-session reach — do
// NOT cite this site as precedent for core-injecting anything that writes.
// The capability (ToolContext.History) and its access tool travel together; a
// bag without History never carries the tool. Pull-based: index/search first,
// read only what's needed. Caps: 16k chars/entry, 20 ids/read, 20k chars/
// read-call (surgical pulls — a huge read-back would immediately re-cross the
// compaction line it was armed under), tool-call arguments searchable (2k
// each) but never displayed.

// compactReadbackNote is rendered AT PROJECTION TIME by buildMessages onto a
// leading summary row (never persisted — the store must not freeze a tool
// name, wording or language, and a re-compaction must never re-summarize
// retrieval plumbing as a phantom topic). It lives next to maybeArmHistoryTool
// because the two share one predicate: the note appears exactly when the tool
// is armed, so it can never be a dangling promise.
const compactReadbackNote = "（本对话更早的原文并未丢失：需要细节时用 read_conversation_log 工具——action=search 按关键词定位，action=index 向更早翻页，action=read 按编号取原文。）"

// maybeArmHistoryTool arms the own-log read-back on a COMPACTED session
// (pull 化 P1, docs/turn-driver-split.md §5-③): once the loop-top replay
// starts at a summary marker, the summary is a table of contents — the model
// gets read_conversation_log pointed at its OWN sid to pull originals back
// (search / index / read; results ride the belt like any tool row). Called by
// both drivers right after maybeCompressInPlace — which returns a FRESH read
// after writing a batch, so arming lands on the same loop-top as the summary.
// Idempotent (History non-nil = already armed; also keeps a ShareHistory
// sub's DELEGATOR view untouched). NOT armed in sub-turns (spec.IsSub): a
// sub's own scratch lives in the subStore whose replay-tail row-kinds differ,
// and the read-back is a parent affordance — buildMessages gates the note on
// the same condition. Un-compacted sessions never pay the tool-spec tokens.
func (c *core) maybeArmHistoryTool(spec *contract.TurnSpec, replay []contract.Message) {
	if spec.IsSub || spec.History != nil || c.store == nil ||
		len(replay) == 0 || replay[0].RowKind != contract.RowKindSummary {
		return
	}
	spec.History = storeHistory{store: c.store, sid: spec.Sid}
	spec.Tools = withTool(spec.Tools, historyTool{own: true})
}

const (
	histEntryCap    = 16000 // per-entry display truncation
	histReadIDCap   = 20    // max ids per read call
	histReadTotal   = 20000 // total chars per read call — surgical pulls; a bigger dump would re-cross the compaction line that armed the tool
	histSearchSnip  = 2000  // per tool-call arguments captured for search only
	histIndexLimit  = 200   // default index page size
	histIndexMax    = 500   // max index page size
	histSearchTopK  = 10    // default search results
	histSearchTopKM = 50    // max search results
)

// historyTool reads a conversation log through ToolContext.History. Two
// habitats, one implementation: a ShareHistory sub reads its DELEGATOR's log
// (own=false, the advisor); a main turn whose history has been compacted reads
// its OWN log back past the summary (own=true, pull 化 P1 — the summary is a
// table of contents, not a replacement; the originals never left the store).
type historyTool struct{ own bool }

func (t historyTool) Spec() contract.ToolSpec {
	desc := "只读查看委托方(主对话)的完整对话记录（仅在顾问子任务中可用）。历史不会自动进入你的上下文——"
	if t.own {
		desc = "只读查看本对话的完整记录，包括已被压缩摘要替代的更早原文。摘要只是目录，原文都在——"
	}
	return contract.ToolSpec{
		Name: "read_conversation_log",
		Description: desc +
			"先用 action=index 浏览清单（最新在前）或 action=search 按关键词定位，" +
			"再用 action=read 按编号取需要的原文。example: {\"action\":\"search\",\"query\":\"库存 报错\"}",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "action":      {"type": "string", "enum": ["index", "search", "read"], "description": "index=分页清单；search=关键词检索；read=按编号读原文"},
    "query":       {"type": "string", "description": "search 用：要找的关键词"},
    "message_ids": {"type": "array", "items": {"type": "integer"}, "description": "read 用：要读的消息编号（≤20）"},
    "offset":      {"type": "integer", "description": "index 用：跳过最近的 N 条（向更早翻页）"},
    "limit":       {"type": "integer", "description": "index 用：本页条数（默认 200，上限 500）"},
    "top_k":       {"type": "integer", "description": "search 用：返回条数（默认 10，上限 50）"}
  },
  "required": ["action"]
}`),
		NoAutoCompress: true, // 原文引用不可被摘要转述
	}
}

// Serialize false: read-only, safe alongside other calls.
func (historyTool) Serialize() bool { return false }

func (historyTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	if tc.History == nil {
		return contract.ErrResult("当前上下文没有可读的对话记录")
	}
	var a struct {
		Action     string `json:"action"`
		Query      string `json:"query"`
		MessageIDs []int  `json:"message_ids"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		TopK       int    `json:"top_k"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("参数解析失败：" + err.Error())
	}
	msgs, err := tc.History.Messages(ctx, 0)
	if err != nil {
		return contract.ErrResult("读取对话记录失败：" + err.Error())
	}
	entries := serializeHistory(msgs)
	switch a.Action {
	case "index":
		return contract.OKResult(historyIndex(entries, a.Offset, a.Limit))
	case "search":
		if strings.TrimSpace(a.Query) == "" {
			return contract.ErrResult("search 需要 query").WithHint("给出要找的关键词")
		}
		return contract.OKResult(historySearch(entries, a.Query, a.TopK))
	case "read":
		if len(a.MessageIDs) == 0 {
			return contract.ErrResult("read 需要 message_ids").WithHint("先 index/search 拿编号")
		}
		return contract.OKResult(historyRead(entries, a.MessageIDs))
	default:
		return contract.ErrResult("未知 action：" + a.Action).WithHint("用 index / search / read")
	}
}

// histEntry is one serialized conversation row for the log view.
type histEntry struct {
	id         int
	role       string // user / assistant / tool
	speaker    string // user rows: the sender name (group attribution)
	text       string // display text (truncated at histEntryCap)
	chars      int    // original length
	tools      []string
	searchText string // tool-call arguments — searchable, never displayed
	truncated  bool
}

// serializeHistory maps store rows to log entries. Tool-call arguments go to
// searchText (searchable, hidden); a tool row's display text is its result.
func serializeHistory(msgs []contract.Message) []histEntry {
	out := make([]histEntry, 0, len(msgs))
	for i, m := range msgs {
		if m.RowKind == contract.RowKindReplay {
			continue // compaction re-appended tail copy — the original is already listed (store contract: full-table viewers drop replay rows)
		}
		if m.Role == "tool" && m.ToolName == "read_conversation_log" {
			// The tool's own prior dumps: derived data, not conversation. Left in,
			// a repeat search top-ranks last call's result row (it concentrates the
			// query terms) and read pulls the same originals twice (own-log habitat:
			// results persist into the very store being read).
			continue
		}
		role := m.Role
		if m.RowKind == contract.RowKindSummary {
			// The summary row may be the ONLY surviving account of pruned early
			// history — show it as such, never hide it (an amputated log would
			// read as "the conversation started here").
			role = "summary(早期对话的压缩摘要)"
		} else if m.Role == "system" {
			continue // other system rows — not part of the visible dialogue
		}
		e := histEntry{id: i, role: role, chars: len(m.Content), text: m.Content}
		if m.Role == "user" && m.Author.Kind == "human" {
			e.speaker = m.Author.Name
		}
		if m.Role == "tool" && m.ToolName != "" {
			e.tools = []string{m.ToolName}
		}
		var snips []string
		for _, call := range m.ToolCalls {
			e.tools = append(e.tools, call.Name)
			if s := call.Arguments; s != "" {
				snips = append(snips, histTruncateRunes(s, histSearchSnip)) // rune-safe: CJK args must not split mid-rune
			}
		}
		e.searchText = strings.Join(snips, " ")
		if len(e.text) > histEntryCap {
			// truncateRunes: a byte cut can split a CJK rune → invalid UTF-8 in the
			// advisor's context (some endpoints hard-4xx on it).
			e.text = histTruncateRunes(e.text, histEntryCap) + "\n\n[...已截断]"
			e.truncated = true
		}
		out = append(out, e)
	}
	return out
}

func histLabel(e histEntry) string {
	label := strings.ToUpper(e.role)
	if e.speaker != "" {
		label += "(" + e.speaker + ")"
	}
	if len(e.tools) > 0 {
		label += " [tools: " + strings.Join(e.tools, ", ") + "]"
	}
	return label
}

// historyIndex renders the paginated manifest, newest first.
func historyIndex(entries []histEntry, offset, limit int) string {
	if len(entries) == 0 {
		return "对话记录为空。"
	}
	if limit <= 0 || limit > histIndexMax {
		if limit <= 0 {
			limit = histIndexLimit
		} else {
			limit = histIndexMax
		}
	}
	if offset < 0 {
		offset = 0
	}
	total := len(entries)
	start := total - offset - limit
	if start < 0 {
		start = 0
	}
	end := total - offset
	if end <= 0 {
		return fmt.Sprintf("offset 超出范围（共 %d 条）。", total)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 对话清单（共 %d 条，本页编号 [%d]-[%d]，最新在前）\n", total, entries[start].id, entries[end-1].id)
	for i := end - 1; i >= start; i-- {
		e := entries[i]
		trunc := ""
		if e.truncated {
			trunc = "（截断）"
		}
		fmt.Fprintf(&b, "[%d] %s (%d 字节)%s\n", e.id, histLabel(e), e.chars, trunc)
	}
	if start > 0 {
		fmt.Fprintf(&b, "↓ 更早还有 %d 条（offset=%d 继续向前翻）\n", start, offset+limit)
	}
	b.WriteString("用 action=read + message_ids 读原文。")
	return b.String()
}

// historyRead returns full entry texts by id, deduped, within the total cap.
func historyRead(entries []histEntry, ids []int) string {
	if len(ids) > histReadIDCap {
		ids = ids[:histReadIDCap]
	}
	byID := make(map[int]histEntry, len(entries))
	for _, e := range entries {
		byID[e.id] = e
	}
	seen := map[int]bool{}
	total := 0
	var parts []string
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		e, ok := byID[id]
		if !ok {
			parts = append(parts, fmt.Sprintf("[%d] 编号不存在", id))
			continue
		}
		line := fmt.Sprintf("[%d] %s (%d 字节):\n\n%s", e.id, histLabel(e), e.chars, e.text)
		total += len(line)
		if total > histReadTotal {
			parts = append(parts, "[...本次输出达到上限，其余编号请分次读]")
			break
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// --- BM25 over the log (camelCase split + CJK bigrams) ---

func histTokenize(text string) []string {
	var camel strings.Builder
	for i, r := range text {
		if i > 0 && r >= 'A' && r <= 'Z' {
			camel.WriteByte(' ')
		}
		camel.WriteRune(r)
	}
	lowered := strings.ToLower(camel.String())
	fields := strings.FieldsFunc(lowered, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var toks []string
	for _, f := range fields {
		toks = append(toks, f)
	}
	// CJK bigrams: an unspaced Chinese sentence is one ascii token — bigrams let a
	// multi-char query like 「库存」 match 「查一下库存数据」.
	var cjk []rune
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fff || r >= 0x3400 && r <= 0x4dbf {
			cjk = append(cjk, r)
		}
	}
	for i := 0; i+1 < len(cjk); i++ {
		toks = append(toks, string(cjk[i:i+2]))
	}
	return toks
}

func historySearch(entries []histEntry, query string, topK int) string {
	if topK <= 0 || topK > histSearchTopKM {
		if topK <= 0 {
			topK = histSearchTopK
		} else {
			topK = histSearchTopKM
		}
	}
	type doc struct {
		e  histEntry
		tf map[string]int
		dl int
	}
	docs := make([]doc, 0, len(entries))
	df := map[string]int{}
	totalLen := 0
	for _, e := range entries {
		toks := histTokenize(e.text + " " + strings.Join(e.tools, " ") + " " + e.searchText)
		if len(toks) == 0 {
			continue
		}
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		for t := range tf {
			df[t]++
		}
		docs = append(docs, doc{e: e, tf: tf, dl: len(toks)})
		totalLen += len(toks)
	}
	if len(docs) == 0 {
		return "没有可检索的内容。"
	}
	avgdl := float64(totalLen) / float64(len(docs))
	qtoks := map[string]bool{}
	for _, t := range histTokenize(query) {
		qtoks[t] = true
	}
	const k1, b = 1.2, 0.75
	type hit struct {
		e       histEntry
		score   float64
		matched int
	}
	var hits []hit
	for _, d := range docs {
		score, matched := 0.0, 0
		for qt := range qtoks {
			tf := d.tf[qt]
			if tf == 0 {
				continue
			}
			matched++
			idf := math.Log(1 + (float64(len(docs))-float64(df[qt])+0.5)/(float64(df[qt])+0.5))
			score += idf * float64(tf) * (k1 + 1) / (float64(tf) + k1*(1-b+b*float64(d.dl)/avgdl))
		}
		if matched == 0 {
			continue
		}
		if matched == len(qtoks) && len(qtoks) > 1 {
			score *= 1.1 // 全词命中加成
		}
		hits = append(hits, hit{e: d.e, score: score, matched: matched})
	}
	if len(hits) == 0 {
		return fmt.Sprintf("没有匹配「%s」的消息（已检索 %d 条）。", query, len(docs))
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].e.id > hits[j].e.id
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	var bld strings.Builder
	fmt.Fprintf(&bld, "# 「%s」的检索结果（%d 条）\n", query, len(hits))
	var ids []string
	for _, h := range hits {
		fmt.Fprintf(&bld, "[%d] %s (%.2f 分, %d 字符)\n", h.e.id, histLabel(h.e), h.score, h.e.chars)
		ids = append(ids, fmt.Sprintf("%d", h.e.id))
	}
	bld.WriteString("用 action=read + message_ids=[" + strings.Join(ids, ",") + "] 读原文。")
	return bld.String()
}

var _ contract.Tool = historyTool{}

// histTruncateRunes clips s to at most maxBytes without splitting a rune.
func histTruncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	keep := maxBytes
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep]
}

// ── advisor (docs/advisor.md) ────────────────────────────────────────────────
//
// The advisor is a STRONGER reviewer model consulted in a depth-1 sub-turn. Unlike
// delegate (a worker in a CLEAN context) its whole point is auditing the ACTUAL
// trajectory: the sub gets a read-only view of this conversation (ShareHistory →
// read_conversation_log) so it checks claims against the record instead of trusting
// a retelling. Model pinned to the `advisor` tag; the pool's tag-fallback routes to
// `smart` when no dedicated model is deployed — which keeps the reviewer at or above
// the §6.10 escalation ceiling, so a post-escalation consult is never "asking
// yourself".
//
// It is NOT a tool: the model cannot summon it. The HARNESS triggers it at the
// moments only the harness can detect — a stall (maybeAdvisorDiagnose, below) and a
// delivery carrying an artifact (advisorReviewGate, 85-rubric.go). The rubric
// arbiter is the third rider on the same chassis. The pull-based advisorTool this
// replaced went unused: the model was told to consult "when stuck / before
// delivering", but those are conditions only the harness can actually evaluate
// (docs/advisor.md §0).

const (
	// advisorStale is the staleness that triggers a diagnosis. Deliberately BEFORE
	// the nudge ladder's first tier (switchFamilyStale 9) and before one tool's fail
	// cap can end the turn (sameFailMax 6): the consult must land while the model
	// still has room to change direction, not once it is being told to give up.
	advisorStale = 6
	// maxAdvisorDiagnoses caps diagnoses per turn — each is a sub-turn (10-100x a
	// plain round). A turn that keeps stalling past that is the nudge ladder's job.
	maxAdvisorDiagnoses = 3
	// maxAdvisorAttempts caps ATTEMPTS, including the ones that came back empty. A
	// failed consult deliberately doesn't spend the diagnosis budget (the next stall
	// deserves a real one), but without a second ceiling a turn whose advisor is
	// "up but mute" would pay a full sub-turn per stall episode for the whole fuse.
	maxAdvisorAttempts = 5
	// advisorNoteLayer carries the latest diagnosis for the REST of the turn.
	// Deliberately NOT the "nudge" layer: loopTopNudge rewrites/clears that every
	// round from the staleness ladder, so a diagnosis parked there would evaporate
	// the moment the model made one step of progress — while the advice exists
	// precisely to steer the rounds AFTER it. Transient (I1): dies with the turn.
	advisorNoteLayer = "advisor_note"
)

// advisorDiagnoseGuide is the stall-diagnosis persona (SubSpec.Guide → slim prompt).
// Its product is pasted into the asking model's prompt, so it must read as
// instructions to act on, not as an essay about the situation.
const advisorDiagnoseGuide = "你是一位策略顾问：主对话的模型正在做一个长任务，已经连续多轮没有进展，由你来指路。\n" +
	"- 你有 read_conversation_log 工具，可以只读查看主对话的完整记录（index 浏览 / search 检索 / read 取原文）。" +
	"先读记录：原任务是什么、已经试过哪几条路、每次具体是怎么失败的。\n" +
	"- 给可执行的下一步，不是分析报告：明确说该换什么方法、该放弃哪条路、缺的信息去哪里取。" +
	"要有明确倾向，不要各打五十大板。\n" +
	"- 如果记录显示任务本身有歧义、或某个前提根本不成立，直接指出来。\n" +
	"- 一次性给完（对话在你回复后结束），控制在几条要点内——你的原话会被直接放进对方的工作提示里。"

// advisorReviewGuide is the delivery-acceptance persona. Same JSON verdict contract
// as the rubric judge / arbiter, so one parser (parseRubricVerdict) serves all three.
// The self-confirmation clause is deliberate: within one turn the SAME advisor may
// have handed out the very direction it is now reviewing (docs/advisor.md §4).
const advisorReviewGuide = "你是交付验收人：主对话的模型声称任务完成、准备把产出交给用户，由你决定放不放行。\n" +
	"- 你有 read_conversation_log 工具，可以只读查看主对话的完整记录。先核对产出里的关键声明是否有据：" +
	"说写了的文件是否真的写成、说查到的数据是否真的查到、结论是否被工具结果支撑。\n" +
	"- 本轮的工作方向可能出自你先前给的诊断建议——复核时不得因为它出自你就放行，一律以记录为准。\n" +
	"- 按任务书给的验收准则判；准则没覆盖的地方用常识。只判能不能交付，不追求完美。\n" +
	"- 只输出一个 JSON 对象，不要代码块围栏、不要解释：" +
	"{\"pass\": true 或 false, \"reason\": \"不合格时一句话指出缺什么、错在哪；合格时一句话说明依据\"}"

// advisorNotePrefix wraps a diagnosis in the same 内部 marker the nudges use (G4), so
// a small model won't paste the consult verbatim into its user-facing reply. It states
// no FACT about the current round on purpose: the note rides the prompt until the next
// diagnosis overwrites it, so a "你已经连续多轮没有进展" opener would still be there —
// and false — on the recovered round that finally delivers.
const advisorNotePrefix = "[内部 — 不要引用进回复] 以下是顾问读完本对话完整记录后给出的诊断，按它调整你的下一步：\n"

// runAdvisor runs ONE advisor consult on the shared chassis and returns its product
// ("" when none was obtained — the caller then degrades, never blocks). A sub-turn
// never consults: depth 1 is the hard rule, and while a TOOL inside a sub is refused
// by its SubRunner, this path calls runSubTurn directly, so the fence has to be here.
func (c *core) runAdvisor(ctx context.Context, spec contract.TurnSpec, task, guide string) string {
	if spec.IsSub {
		return ""
	}
	res := c.runSubTurn(ctx, spec, contract.SubSpec{
		Task:  task,
		Guide: guide, // read_conversation_log rides ShareHistory (内置), no suite naming
		// Hard tag: a dedicated advisor model when deployed, else the pool's
		// fallback ruleset routes advisor → smart.
		ModelRequires: []string{"advisor"},
		ShareHistory:  true,
	})
	if res.Err != nil {
		slog.Warn("turn: advisor consult failed", "sid", spec.Sid, "err", res.Err)
		return ""
	}
	if strings.TrimSpace(res.Product) == "" {
		// The most common real-world failure (a model that emits its whole answer as
		// reasoning, leaving content blank — see judgeRubric's note) reaches here as a
		// clean, empty result. Log it: without this the consult is invisible in
		// production, and a mute-but-billing advisor would burn its attempt budget
		// with nothing in the record to explain where the wall clock went.
		slog.Warn("turn: advisor consult returned nothing", "sid", spec.Sid)
		return ""
	}
	if res.Partial {
		// A sub-turn that ran out of rounds / tripped a guard still returns TEXT — its
		// own salvage brief ("本次调用了 …，但没能给出完整结果"). That is an apology,
		// not advice: parking it in the prompt under "以下是顾问给出的诊断" would put
		// words in the advisor's mouth, and a verdict parsed out of it would be noise.
		// Treated as no product everywhere (I18: an honest partial is never passed off
		// as clean); the caller then degrades exactly as it does for an outage.
		slog.Warn("turn: advisor consult ended partial — treating as no advice",
			"sid", spec.Sid, "raw", truncateBytes(collapseLine(res.Product), 120))
		return ""
	}
	return strings.TrimSpace(res.Product)
}

// maybeAdvisorDiagnose fires the stall diagnosis at the loop top (docs/advisor.md §2)
// — the LOOPING driver's call, since the advisor is that mode's standard equipment.
// Exact-match on advisorStale so it fires ONCE per stall episode: progress resets the
// watermark, and a fresh 6-round stall earns a fresh diagnosis (overwriting the layer).
func (c *core) maybeAdvisorDiagnose(ctx context.Context, spec contract.TurnSpec, st *turnState, iter int) {
	if spec.Prompt == nil || st.advisorDiagnoses >= maxAdvisorDiagnoses ||
		st.advisorAttempts >= maxAdvisorAttempts || staleRounds(st, iter) != advisorStale {
		return
	}
	st.advisorAttempts++
	out := c.runAdvisor(ctx, spec, advisorDiagnoseTask(st, iter), advisorDiagnoseGuide)
	if out == "" {
		// An outage / partial must not consume the DIAGNOSIS budget — the next stall
		// episode still deserves a real consult. The attempt budget above is what
		// stops a permanently mute advisor from being re-dialled all turn.
		return
	}
	st.advisorDiagnoses++
	spec.Prompt.SetLayer(advisorNoteLayer, advisorNotePrefix+out)
	// Full text on the trace stream (trace:on): a human reading a stalled turn after
	// the fact sees exactly what the model was told. Dropped when trace is off, so it
	// costs nothing then.
	spec.Sink.TraceDelta("🩺 顾问诊断：\n" + out)
	slog.Info("turn: advisor diagnosis pushed", "sid", spec.Sid, "iter", iter, "n", st.advisorDiagnoses)
}

// advisorDiagnoseTask is the diagnosis brief. Deliberately thin: the advisor pulls the
// trajectory itself (ShareHistory) — a retold summary here would be exactly the biased
// framing the read-back exists to bypass — so the brief carries only the two numbers
// the harness knows and the model doesn't (staleness, failures since progress).
//
// It does NOT carry the acceptance standard (拍板). The standard reaches the
// REVIEWER and nobody else: a diagnosis is quoted verbatim into the working model's
// prompt, so folding criteria in here would leak them back to the model being judged
// through the coach — the one hole in "the graded never sees the grading sheet".
func advisorDiagnoseTask(st *turnState, iter int) string {
	return fmt.Sprintf(
		"主对话已经连续 %d 轮没有取得新进展（其间工具失败 %d 次）。请读对话记录，判断它卡在哪里，给出接下来具体该怎么走。",
		staleRounds(st, iter), st.failedSinceProgress,
	)
}
