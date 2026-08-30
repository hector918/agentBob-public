package turn

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"agentbob/contract"
)

// This file is in-place context compaction (spec §6.9, A5): when the replay nears
// the model's input budget, summarize the OLDER part and re-append the recent tail
// after a summary marker — same sid, so replay = "from the last summary marker"
// (buildMessages). No state rebind / task-list migration / cross-generation feed
// settling — the entire A5 payoff, for the cost of leaving old rows in the table.
//
// Two layers with a hard contract (hector):
//
//   - UPSTREAM (compactHistory + split) owns STRUCTURE only: where the cut line
//     falls (keep ~budget/3 of recent tail, never split a calls/result pair) and
//     how the result lands (summary marker row + re-appended tail, one atomic
//     batch). It never reasons about size beyond the keep walk, and it never
//     refuses because of content.
//   - DOWNSTREAM (compressPass) owns COMPRESSION only: given text of ANY size it
//     runs ONE pass — sliding-window cut at logical boundaries, one summarize
//     call per segment, join, plus a final merge call when the join fits. A join
//     still over the window is returned as-is: it lands as the summary row,
//     which doubles as the WORK MARKER — the next loop-top's trigger sees the
//     still-oversized replay and runs another pass over it. Convergence rides
//     the turn loop (bounded by its own iteration caps); there is no internal
//     while, no depth counter, no size-based refusal.
//
// Failure is one thing only: a summarize call failed → this round didn't
// compress (logged), the history is untouched (recoverable), next loop-top
// retries.

// summaryPrefix marks a compaction summary row (row_kind=summary — the row's
// IDENTITY lives in the row_kind column, store-stamped and unspoofable; the
// prefix is human-facing decoration only and nothing may key on it). Written
// with role=user: the post-compaction window then always opens with a user
// message, which is what lets the split layer stay structure-only (no "recent
// must hold a user row" rule) — strict templates that reject user-less
// conversations are satisfied by construction.
const summaryPrefix = "[Summary of the earlier part of this conversation]"

// maybeCompressInPlace runs at the loop top (proactive path): if the replay
// (system prompt + history from the last summary) is under 80% of the model's
// input budget it is a no-op (logging a "yellow zone" line past 65%); otherwise
// it summarizes the older part and writes a compaction batch. Best-effort: any
// obstacle (unknown budget, an unsplittable history, a failed summarize/persist)
// abandons WITH a log line — the turn proceeds on the un-compacted history
// (recoverable).
//
// Sizing is 称重制 (88-weigh.go): the trigger weighs system + replay + the
// advertised tool specs on the MAIN request's own tokenizer scale — exact
// counts, no estimate-and-correct — and takes the LAST REAL prompt reading
// (resp.Usage.InputTokens, the promptFloor) as a truth floor when it's larger:
// usage sees the chat-template overhead the weigh can't, and on pools without
// a tokenize path it is the only real number. The 0.80 line covers what
// remains genuinely unknowable at loop-top: THIS turn's new rows stacking
// before the next call and cross-tokenizer variance when a failover serves
// the round on a sibling backend. The reactive path (forceCompact) stays as
// the net for whatever slips through.
//
// It returns the replay the round should build from — the loop-top read, or a
// fresh post-compaction read when a batch was written (same-loop-top truth for
// the read-back arming); nil only on a read error (the round then reads for
// itself, fail-closed there).
func (c *core) maybeCompressInPlace(ctx context.Context, spec contract.TurnSpec) []contract.Message {
	// Read FIRST, gate after: the replay is returned on every non-error path so
	// the round reuses one read AND the drivers' arm step (maybeArmHistoryTool)
	// always sees the truth — an unknown-budget pool must not hide an existing
	// summary marker from the read-back arming (review round 3: that hid the
	// note's tool forever on window-less pools).
	replay, err := c.store.GetReplay(ctx, spec.Sid, historyReplayCap)
	if err != nil {
		return nil
	}
	sreq := sizingReq(spec)
	budget := c.budgetFor(sreq)
	c.recordCtxBudget(spec.Sid, budget) // gauge for read-only panels (/session)
	if budget <= 0 {
		return replay // unknown budget → native long-context fallback (spec §6.9)
	}
	// Weigh what the round ACTUALLY builds: renderSpeakers adds a "[name]: "
	// prefix to every group user row, which buildMessages includes but a raw-replay
	// reading omits — so a busy group could exceed budget while this read under 80%.
	// renderSpeakers on a SHALLOW COPY (independent Content fields) keeps `replay` clean
	// for compactHistory below (which must persist unprefixed content).
	weighed := c.weigh(ctx, sreq, buildSystem(spec.Prompt)) +
		c.weighMsgs(ctx, sreq, renderSpeakers(append([]contract.Message(nil), replay...), spec.Scope))
	if spec.Tools != nil {
		weighed += c.weighToolSpecs(ctx, sreq, spec.Tools.Specs())
	}
	floor := c.lastPromptFloor(spec.Sid)
	trigger := weighed
	if floor > trigger {
		trigger = floor
	}
	if trigger < int(float64(budget)*0.80) {
		if trigger >= int(float64(budget)*0.65) {
			slog.Info("turn: context yellow zone", "sid", spec.Sid, "weighed_tokens", weighed, "floor_tokens", floor, "budget", budget)
		}
		return replay
	}
	// keepTokens and the split walk are on the same scale as the trigger
	// (weighMsg), so the kept tail really is ~budget/3.
	if !c.compactHistory(ctx, spec, replay, budget/3) {
		return replay // couldn't reduce → keep the oversized history (recoverable; logged inside)
	}
	// History rewritten — re-read so the SAME loop-top arms the read-back tool
	// against the fresh summary (no one-round window where the projected note
	// promises a tool that isn't in the suite) and the round reuses this read.
	fresh, err := c.store.GetReplay(ctx, spec.Sid, historyReplayCap)
	if err != nil {
		return nil // round reads for itself
	}
	return fresh
}

// forceCompact is the reactive compaction path: after the pool rejected the
// prompt as too large for the model's context window (roundOversized), reduce
// history UNCONDITIONALLY — no est/threshold gate, since the model itself is the
// authority that the prompt overflowed. Reads the current replay, then runs the
// shared compaction core. Returns true iff a batch was written; false is the
// no-progress signal for the Run loop — the recent tail alone already spans the
// replay (nothing left to summarize) or the summarize/persist failed, so the loop
// stops and tells the user honestly rather than looping forever.
func (c *core) forceCompact(ctx context.Context, spec contract.TurnSpec) (shrank bool) {
	replay, err := c.store.GetReplay(ctx, spec.Sid, historyReplayCap)
	if err != nil {
		return false
	}
	// keepTokens tracks the proactive path (budget/3, the winner's window). An
	// unknown budget (a pool that advertises none) leaves keep=0, which makes
	// splitForCompaction keep the minimal well-formed tail — the maximal
	// reduction, exactly right when we can't size the window but the model has
	// already told us the prompt is too big.
	return c.compactHistory(ctx, spec, replay, c.budgetFor(sizingReq(spec))/3)
}

// compactHistory is the UPSTREAM structure layer: split replay into
// (older, recent), hand the rendered older to the downstream compressor, and
// land the result as one atomic batch (summary marker + recent tail).
// keepTokens targets how much recent tail to preserve.
//
// Returns true iff a batch was actually written — the no-progress signal both
// callers key on. Every false is logged: an unsplittable history (the tail
// alone spans the replay), a re-summarize that cannot help (older is ONLY an
// already-small summary — the oversized bulk lives in the un-compactable
// tail), a compressor that made no progress (a parroting model echoes its
// input), or a failed summarize/persist.
func (c *core) compactHistory(ctx context.Context, spec contract.TurnSpec, replay []contract.Message, keepTokens int) bool {
	// The split walk consumes keepTokens on the SAME scale the trigger weighed
	// (the main request's tokenizer, via the shared weight cache) — the two
	// accountings can never drift apart.
	sreq := sizingReq(spec)
	weight := func(m contract.Message) int { return c.weighMsg(ctx, sreq, m) }
	// Cap the kept tail at a THIRD OF WHAT'S HERE, in whatever units weight
	// measures. keepTokens is a real-token target; on the estimator-fallback
	// path the walk measures est units, and a dense (CJK/mojibake) history can
	// read entirely under the target — the walk then consumes every row and
	// concludes "nothing to summarize" while the model just 400'd the very
	// same replay (实弹: 220K real read as 43.7K est ≈ budget/3 →
	// forceCompact gave up → the honest too-long notice on a compactable
	// history). "Keep ≤ 1/3 of the total" is scale-free — it holds on both
	// rulers, guarantees the cut lands strictly inside the replay, and makes
	// every reactive pass a multiplicative reduction (convergent under
	// repeated 400s). keepTokens==0 (unknown budget) keeps its old meaning:
	// minimal well-formed tail, the maximal reduction.
	if keepTokens > 0 {
		total := 0
		for _, m := range replay {
			total += weight(m)
		}
		if keepTokens > total/3 {
			keepTokens = total / 3
		}
	}
	older, recent, ok := splitForCompaction(replay, keepTokens, weight)
	if !ok {
		slog.Warn("turn: compaction abandoned — history unsplittable (tail alone spans the replay)",
			"sid", spec.Sid, "rows", len(replay))
		return false
	}
	olderText := renderForSummary(older)
	// No futility PREDICTION here (an earlier guard skipped "older is already a
	// compact summary" — a guess about what re-compression would achieve, and it
	// misjudged in both directions under a degraded scale). The measured
	// progress check below is the only stop: re-compressing a summary row that
	// still SHRINKS is the multi-pass work-marker convergence and proceeds; one
	// that can't shrink fails the measurement and stops the retry loop — real
	// outcomes, not forecasts (hector: 识别真实错误,不预测).
	summary := c.compressPass(ctx, olderText)
	if summary == "" {
		return false // summarize failed (logged at the call) → keep the oversized history (recoverable)
	}
	// The LLM summary drops mechanical inbox paths, so a model that later wants an
	// EARLIER-turn file (to ocr / upload it) would lose the handle once the summary
	// replaces the `[本轮附件]` text. Re-attach the durable structured file list (read
	// from the attachments column, untouched by text compaction) to the summary so those
	// paths stay in the replayed window.
	marker := summaryPrefix + "\n" + summary + attachmentManifest(ctx, c.store, spec.Sid)
	// Progress check — the parroting net: the summary row is the work marker the
	// next loop-top compresses again, so a marker that doesn't weigh less than
	// the rows it replaces would burn one summarize per loop-top forever. Both
	// sides on the trigger's own ruler (weighMsg, main scale): the marker as it
	// will actually be PERSISTED (prefix + manifest included — review: comparing
	// bare summary bytes let a manifest-heavy marker "make progress" while the
	// stored history grew) against the older rows' full replay weight (tool-call
	// Arguments included — review: rendered bytes miss Arguments, so an
	// Arguments-heavy older read as tiny and a normal summary was rejected as
	// "no progress" on a perfectly compactable history).
	newWeight := c.weighMsg(ctx, sreq, contract.Message{Role: "user", Content: marker})
	oldWeight := c.weighMsgs(ctx, sreq, older)
	if newWeight >= oldWeight {
		slog.Warn("turn: compaction made no progress — abandoning this round",
			"sid", spec.Sid, "older_tokens", oldWeight, "marker_tokens", newWeight)
		return false
	}
	// NB: the read-back note (compactReadbackNote) is deliberately NOT persisted
	// — buildMessages renders it at projection time on the same predicate that
	// arms the tool (16-looping-tools.go), so the note and the tool can never
	// desync, the store never freezes a tool name / wording / language, and a
	// re-compaction never re-summarizes retrieval plumbing as a phantom topic.
	if err := c.store.AppendCompactionBatch(ctx, spec.Sid, marker, recent); err != nil {
		slog.Warn("turn: compaction batch failed — proceeding un-compacted", "sid", spec.Sid, "err", err)
		return false
	}
	// The pre-compaction floor describes a window that no longer exists — drop
	// it so the rewritten history doesn't re-trigger every turn until a fresh
	// round re-learns the real size.
	c.clearPromptFloor(spec.Sid)
	slog.Info("turn: compacted in place", "sid", spec.Sid, "older_rows", len(older), "recent_rows", len(recent),
		"in_bytes", len(olderText), "out_bytes", len(summary))
	return true
}

// splitForCompaction divides replay into (older, recent) keeping ~keepTokens in
// recent, with the spec §6.9-2 rules: recent ends at the last row; the cut never
// lands inside an assistant-calls→tool-results pair. weight measures one row on
// the caller's scale (compactHistory passes the shared 称重 cache, so the walk
// and the trigger use one accounting). Returns ok=false to abandon (nothing left
// to summarize after the pair adjustment — the tail alone spans the replay).
//
// STRUCTURE ONLY — no content rule: the summary row lands as a user message
// (see summaryPrefix), so the post-compaction window opens with a user row by
// construction and recent needs no user row of its own. (The old "recent must
// hold ≥1 user row" rule pulled the cut back to a single leading user row —
// e.g. a one-instruction looping research turn — and made such sessions
// permanently uncompactable: 实弹.)
func splitForCompaction(replay []contract.Message, keepTokens int, weight func(contract.Message) int) (older, recent []contract.Message, ok bool) {
	if len(replay) < 2 {
		return nil, nil, false
	}
	// Walk from the end accumulating tokens until the budget is reached — that index
	// is the recent boundary (recent always includes the last row).
	cut := len(replay) - 1
	tok := 0
	for i := len(replay) - 1; i >= 0; i-- {
		tok += weight(replay[i])
		cut = i
		if tok >= keepTokens {
			break
		}
	}
	// The cut must not split a calls/result pair: a leading tool row means its
	// assistant-calls is in older — move the cut back to include it (and any
	// preceding tool rows of the same pair).
	for cut > 0 && replay[cut].Role == "tool" {
		cut--
	}
	if cut <= 0 {
		return nil, nil, false // everything is recent → nothing to summarize
	}
	return replay[:cut], replay[cut:], true
}

// renderForSummary renders the older rows as the downstream compressor's input:
// one "role: content" line per row. Purely mechanical — the compressor never
// sees message structure, only text.
func renderForSummary(older []contract.Message) string {
	var b strings.Builder
	for _, m := range older {
		if m.Content == "" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// compressPass is the DOWNSTREAM compressor: ONE pass over text of any size.
// Fits the compress window → one call. Bigger → sliding-window cut at logical
// boundaries, one call per segment, join — and when the join fits the window,
// one more call merges it (cross-segment dedup with the whole in view, the
// "一并再压" step). A join still over the window returns as-is: it lands as
// the summary row (the work marker) and the NEXT loop-top's pass continues
// from there. Per pass: ≤ segments+1 calls, no loop, no recursion. "" on any
// failed call (the caller abandons this round; logged at summarizeText).
func (c *core) compressPass(ctx context.Context, text string) string {
	segCap := c.compressSegCap()
	total := c.weighChunked(ctx, text)
	if total <= segCap {
		return c.summarizeSegment(ctx, compactSummarySystem, text)
	}
	segs := cutBySlidingWindow(text, total, segCap)
	parts := make([]string, 0, len(segs))
	for _, seg := range segs {
		out := c.summarizeSegment(ctx, compactSummarySystem, seg)
		if out == "" {
			return ""
		}
		parts = append(parts, out)
	}
	joined := strings.Join(parts, "\n\n")
	if c.weighChunked(ctx, joined) <= segCap {
		if merged := c.summarizeSegment(ctx, compactMergeSystem, joined); merged != "" {
			return merged
		}
		// The merge is an optional polish over N ALREADY-PAID segment calls —
		// a transient failure here must not discard them (review: abandoning
		// re-runs every segment from scratch next loop-top). The plain join is
		// a valid, strictly-smaller pass output; ship it.
		slog.Warn("turn: compression merge call failed — landing the plain join", "segments", len(parts))
	} else {
		slog.Info("turn: compression pass output still oversized — next loop-top continues",
			"segments", len(parts), "out_bytes", len(joined))
	}
	return joined
}

// summarizeSegment runs one compression call, HALVING on a context-overflow
// reject: that 400 is the serving mouth itself saying THIS text overflows its
// window — an identified real error, not a prediction (hector:
// 识别真实错误再反应). Every recursion is licensed by one such 400 and halves
// the input, so termination is geometric with no depth constant; it is also
// the net for a mis-measured cut (estimator-fallback undercount, density
// skew), which would otherwise 400 the identical segment every loop-top,
// permanently. Any OTHER failure returns "" (transient — retry next loop-top).
func (c *core) summarizeSegment(ctx context.Context, system, text string) string {
	out, err := c.summarizeText(ctx, system, text)
	if err == nil {
		return out
	}
	if !isContextExceededErr(err) || len(text) < 2 {
		return ""
	}
	mid := boundaryCut(text, len(text)/2)
	if mid >= len(text) {
		return "" // degenerate tiny input — nothing left to halve
	}
	slog.Info("turn: compress call overflowed the serving window — halving", "bytes", len(text))
	a := c.summarizeSegment(ctx, system, text[:mid])
	if a == "" {
		return ""
	}
	b := c.summarizeSegment(ctx, system, text[mid:])
	if b == "" {
		return ""
	}
	return a + "\n\n" + b
}

// weighChunkBytes bounds a single tokenize payload: one huge POST (a rendered
// older part can reach hundreds of KB) can outlive the backend's tokenize
// timeout and trip the 60s weigh breaker — silently degrading ALL sizing to
// the estimator. Fixed-size chunks keep every payload comfortably inside the
// timeout; the seam error is ±1 token per chunk, noise at this scale.
const weighChunkBytes = 64 << 10

// weighChunked weighs text on the compress scale in ≤weighChunkBytes rune-safe
// chunks and sums.
func (c *core) weighChunked(ctx context.Context, text string) int {
	total := 0
	for len(text) > weighChunkBytes {
		cut := weighChunkBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			break // degenerate (invalid UTF-8 head) — weigh the rest in one piece
		}
		total += c.weigh(ctx, compressReq(), text[:cut])
		text = text[cut:]
	}
	return total + c.weigh(ctx, compressReq(), text)
}

// cutBySlidingWindow cuts text into segments of ≤~capTokens on the compress
// scale. The token→byte conversion is proportional from ONE real weigh of the
// whole text (totalTokens), with a 10% margin for density variance inside the
// text — no per-segment re-weigh loop; a segment that still overshoots simply
// fails its summarize call (recoverable, next loop-top retries).
func cutBySlidingWindow(text string, totalTokens, capTokens int) []string {
	bytesPerTok := float64(len(text)) / float64(totalTokens)
	if bytesPerTok < 1 {
		bytesPerTok = 1 // sub-byte tokens don't exist; guards totalTokens > len(text)
	}
	capBytes := int(float64(capTokens) * bytesPerTok * 0.9)
	if capBytes < 1 {
		capBytes = 1
	}
	var segs []string
	for len(text) > capBytes {
		cut := boundaryCut(text, capBytes)
		segs = append(segs, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		segs = append(segs, text)
	}
	return segs
}

// boundaryCut picks one window's cut position: the LAST logical boundary
// (newline, CJK/ASCII sentence end, closing brace/bracket) within the final
// tenth of the window — a boundary further back would waste most of the
// window (e.g. a base64 run whose only newline is near the start). No
// boundary there → a hard rune-safe cut at the cap.
func boundaryCut(text string, capBytes int) int {
	floor := capBytes - capBytes/10
	if floor < 0 {
		floor = 0
	}
	// A rune truncated at either edge of the slice decodes as RuneError and
	// matches no boundary char, so the raw byte slice is rune-safe here.
	if i := strings.LastIndexAny(text[floor:capBytes], compactBoundaryChars); i >= 0 {
		_, size := utf8.DecodeRuneInString(text[floor+i:])
		cut := floor + i + size
		if cut > len(text) {
			cut = len(text)
		}
		return cut
	}
	// Hard cut at the cap, walked back to a rune start (same clip as
	// histTruncateRunes). A cap smaller than one rune would walk to 0 and
	// loop the caller forever — take the whole first rune instead.
	cut := capBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut <= 0 {
		_, size := utf8.DecodeRuneInString(text)
		cut = size
	}
	return cut
}

// compactBoundaryChars are the logical cut points boundaryCut accepts, ordered
// nearest-to-cap-wins by the LastIndexAny scan: newline, CJK/ASCII sentence
// ends, closing braces/brackets.
const compactBoundaryChars = "\n。！？.!?}])；;"

// summarizeText runs one compression call over text with the given system
// instruction. The error is surfaced (not swallowed) so summarizeSegment can
// distinguish a context-overflow reject — halve and retry — from a plain
// failure; empty input returns ("", nil) and reads as failure at call sites.
func (c *core) summarizeText(ctx context.Context, system, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	msgs := []contract.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: text},
	}
	resp, err := c.pool.Chat(ctx, compressReq(), msgs)
	if err != nil {
		slog.Warn("turn: compaction summarize failed", "err", err)
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// compactSummarySystem: English (the compress-tag models follow English
// instructions more reliably, and compacted histories are often English-dominant
// source text). The framing is the load-bearing part — it must summarize what the
// conversation ESTABLISHED, not restate what quoted sources SAY: a scraped page's
// self-description or marketing ("the official store") is evidence, not a fact,
// and naively summarizing it inverts the assistant's actual finding (// 实弹: a fraud-assessment turn compacted its raw scraped pages into "hoyo.global
// is the official Genshin store" — the opposite of its own report).
const compactSummarySystem = "You are compressing an earlier part of a conversation so the model can continue it. " +
	"Summarize what was ESTABLISHED, as concise topic-led bullet points: the assistant's findings and conclusions, " +
	"decisions made, unfinished tasks, the user's goals and constraints, and any identifiers/values/paths needed later. " +
	"Critically: text quoted from tools or web pages is SOURCE MATERIAL, not fact — capture the assistant's conclusion ABOUT a source, " +
	"and never restate a source's own self-description or marketing claims as if they were true. " +
	"Output only the summary body — no preamble, no explanation. Brevity is fine."

// compactMergeSystem is the final merge call's instruction: its input is the
// JOIN of per-segment summaries, not raw dialogue — the merge exists to
// de-duplicate across segments, which the segment prompt never asks for
// (review: reusing compactSummarySystem here preserved the cross-segment
// repetition the merge step is for).
const compactMergeSystem = "Below are per-segment summaries of one earlier stretch of a conversation, in chronological order. " +
	"Merge them into a single topic-led bullet summary: one line per topic, drop cross-segment repetition, " +
	"and preserve key facts, decisions, unfinished tasks, the user's goals and constraints, and any identifiers/values/paths needed later. " +
	"Treat any quoted source material as evidence, not fact. Output only the summary body — no preamble, no explanation. Brevity is fine."

// attachmentManifest renders the session's files as a compact "[本会话文件]" block to
// append to a compaction summary — the space-relative paths a tool reads, so a file the
// user sent in an earlier turn stays usable after its `[本轮附件]` text is summarised
// away. Best-effort: a store error or no files → "" (the summary is just the LLM text).
func attachmentManifest(ctx context.Context, store contract.MessageStore, sid string) string {
	atts, err := store.RecentAttachments(ctx, sid, 0)
	if err != nil || len(atts) == 0 {
		return ""
	}
	var lines []string
	for _, a := range atts {
		if a.Path != "" {
			lines = append(lines, "- "+a.Path)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n[本会话文件]\n" + strings.Join(lines, "\n")
}

// compressReq is the compression side-call's model request: HARD Requires so
// the pick is scoped to the dedicated compress entries (a soft Prefer would
// route past them to the primary big entries). A compress outage falls back
// via the pool's tag-fallback ruleset (models.yaml defaults.fallback
// compress→smart).
func compressReq() contract.ModelRequest {
	return contract.ModelRequest{Kind: contract.KindLLM, Requires: []string{"compress"}}
}

// compactSegTokens is the fallback segment size when the pool can't report the
// compress entries' window (compressSegCap) — the 32768 slot minus headroom for
// the system line and the generated summary. Var so tests can shrink it.
var compactSegTokens = 28000

// compressSegCap sizes a compression segment from ground truth: the window of
// the compress entry that would serve right now (models.yaml context_window
// declaration — 声明即真相, hector; the window-blind picker's
// winner) × 0.8, minus headroom (system line + generated summary
// ≈2K). The cap is consumed in weigh units — exact when the pool has a
// tokenize path. Pool without the sizing seam (tests) or no compress entry →
// the compactSegTokens default.
func (c *core) compressSegCap() int {
	if w := c.budgetFor(compressReq()); w > 0 {
		segCap := int(float64(w)*0.8) - 2000
		if segCap < 2000 {
			segCap = 2000 // floor: absurdly small segments would explode the call count
		}
		return segCap
	}
	return compactSegTokens
}
