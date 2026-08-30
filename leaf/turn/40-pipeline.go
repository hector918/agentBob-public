package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
	"agentbob/heartwood/scrub"
)

// This file is the tool-result pipeline (spec §4.2 steps 17-21, §6.1/§6.2) — the
// shape + safety invariants every tool round applies, in fixed order (I6):
//
//	dedupe(+synth-id) → [persist calls] → [dispatch] → belt cap → scrub → summarize
//	→ empty→"{}" → appendToolResult(lock retry)
//
// These are MECHANISM: universal, un-skippable, no per-flow policy. The only knobs
// are constants + the tool-authored NoAutoCompress opt-out.

const (
	// beltCapBytes is the hard per-result byte ceiling before a result reaches the
	// history (spec §6.2). A runaway result is replayed every round, so the belt is
	// the always-on floor — independent of summarize, which may be off.
	beltCapBytes = 256 * 1024
	// summarizeThresholdTokens routes an oversized result through a small model to
	// summarize before it lands in history (spec §6.2.3). Below it, verbatim.
	summarizeThresholdTokens = 2000
	// storeLockedRetryDelay is the single back-off before the one I15 retry.
	storeLockedRetryDelay = 50 * time.Millisecond
)

// synthSeq disambiguates backfilled tool-call ids minted within the same nanosecond
// (see dedupeCalls — the timestamp component carries the reboot-safety).
var synthSeq atomic.Int64

// dedupeCalls is the single pre-persist/dispatch normalization pass on a round's
// tool_calls (spec §6.1): it backfills empty ids, drops same-id duplicates, and
// neutralizes malformed arguments. It runs BEFORE both AppendAssistantCalls (I2)
// and dispatch, and the same returned slice feeds both, so history and the tool
// see one normalized form.
//
// Empty ids: some chat templates emit parallel calls with no id, which collide
// first-match on replay. Each empty-id call gets its own synth id and survives.
// Synthetic ids must be unique across the WHOLE replay window, not just this round:
// repairOrphanToolCalls / dropOrphanToolRows key on the id alone, so a repeating
// "synth-0" would let an earlier round's result row mask a later round's orphaned
// call (the exact I2/I3 crash gap the repair closes). The wall-clock component also
// keeps a post-crash turn's ids distinct from the persisted orphan it must repair
// (a bare counter would restart at 0 on reboot — precisely when it matters); the
// process-wide sequence keeps same-nanosecond rounds apart.
//
// Malformed arguments: a degenerate model can emit a tool_call whose arguments
// JSON is truncated / not valid (e.g. cut off mid-object at max_tokens). Left
// alone it is persisted verbatim (20-round.go AppendAssistantCalls) and REPLAYED
// to every model on each later round (providers.toWire sends Arguments raw), and a
// strict server rejects the whole request ("unexpected end of input; expected
// '}'") — so ONE bad call fails every failover candidate and poisons the pool for
// the rest of the session. Neutralize it to a valid empty object here: the tool's
// own required-field check then returns a clean error the model can retry from,
// and history stays replay-safe. An empty ("") arguments string is normalized too
// (json.Valid rejects it) — a no-arg tool reads "{}" fine.
func dedupeCalls(calls []contract.ToolCall) []contract.ToolCall {
	seen := make(map[string]bool, len(calls))
	out := make([]contract.ToolCall, 0, len(calls))
	synthBase := time.Now().UnixNano()
	for _, call := range calls {
		if call.ID == "" {
			call.ID = fmt.Sprintf("synth-%d-%d", synthBase, synthSeq.Add(1))
		}
		if seen[call.ID] {
			slog.Warn("turn: dropping duplicate tool_call id", "id", call.ID, "tool", call.Name)
			continue
		}
		seen[call.ID] = true
		if a := strings.TrimSpace(call.Arguments); a == "" || !json.Valid([]byte(a)) {
			slog.Warn("turn: neutralized malformed tool_call arguments (kept call, reset to {})",
				"id", call.ID, "tool", call.Name, "raw_len", len(call.Arguments))
			call.Arguments = "{}"
		}
		out = append(out, call)
	}
	return out
}

// pipelineAndPersist applies the result pipeline to ONE result and persists it
// (I3). belt + scrub + empty-guard always run; summarize runs only for an oversized
// OK result whose tool didn't opt out. Operates on the result's payload fields, then
// serializes the envelope — so truncation never splits the envelope JSON.
func (c *core) pipelineAndPersist(ctx context.Context, spec contract.TurnSpec, call contract.ToolCall, res contract.ToolResult) {
	// scrub BEFORE belt: redact credentials on the FULL payload (the summary model
	// may be remote — raw secret bytes must not reach that wire). scrub.Scrub is
	// idempotent and replaces whole tokens with a fixed marker, so scrubbing first
	// then capping is behavior-preserving EXCEPT it closes a leak: a credential
	// straddling the belt boundary would otherwise keep its un-redacted head
	// fragment (scrub's min-length anchors can't match the truncated tail).
	res.Data = scrub.Scrub(res.Data)
	// belt: cap the (potentially huge) success payload; Error/Hint are short.
	res.Data = beltTruncate(res.Data)
	res.Error = scrub.Scrub(res.Error)
	res.Hint = scrub.Scrub(res.Hint)
	// summarize: oversized OK payload, tool not opted out, not an exit result.
	if res.OK && res.ExitRequest == nil && res.Data != "" && !c.noAutoCompress(spec, call.Name) {
		if prompt.EstTokens(res.Data) > summarizeThresholdTokens {
			res.Data = c.summarize(ctx, res.Data)
		}
	}
	content := res.Envelope()
	if content == "" {
		content = "{}" // defensive: Envelope never returns "" (template-crash guard)
	}
	if err := c.appendToolResultRetry(ctx, spec.Sid, call.ID, call.Name, content); err != nil {
		// The retry already ran; the assistant-calls row exists but this result row
		// doesn't — next boot's orphan repair splices a placeholder (I3 closure).
		slog.Error("turn: tool result not saved after retry — orphan repair next turn",
			"sid", spec.Sid, "tool", call.Name, "call_id", call.ID, "err", err)
	}
}

// noAutoCompress reports whether the tool opted out of summarize (verbatim tools:
// read_file, patch). An unknown tool defaults to compressible.
func (c *core) noAutoCompress(spec contract.TurnSpec, name string) bool {
	if spec.Tools == nil {
		return false
	}
	t, ok := spec.Tools.Lookup(name)
	return ok && t.Spec().NoAutoCompress
}

// summarize routes an oversized payload through a small model and returns the
// summary prefixed with a token note. Fail-safe: any error / empty summary →
// verbatim (the belt-capped payload), never an empty or lost result.
func (c *core) summarize(ctx context.Context, data string) string {
	n := prompt.EstTokens(data)
	msgs := []contract.Message{
		{Role: "system", Content: summarizeSystem},
		{Role: "user", Content: data},
	}
	resp, err := c.pool.Chat(ctx, compressReq(), msgs)
	if err != nil {
		slog.Warn("turn: tool-result summarize failed — keeping verbatim", "err", err)
		return data
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return data
	}
	return fmt.Sprintf("[工具返回约 %d token；以下为摘要]\n%s", n, out)
}

const summarizeSystem = "你是工具结果摘要器。把下面的工具返回内容压成简洁摘要，保留关键事实、数字、" +
	"标识符、可点引用，去掉冗余与噪音。直接输出摘要正文，不要前言、不要解释。"

// appendToolResultRetry persists one result row, retrying ONCE after a short
// back-off on a lock-busy error (I15) — and ONLY that class (other store errors
// must not be retried). The retry honors ctx cancellation.
func (c *core) appendToolResultRetry(ctx context.Context, sid, callID, name, content string) error {
	err := c.store.AppendToolResult(ctx, sid, callID, name, content)
	if err == nil || !errors.Is(err, contract.ErrStoreLocked) {
		return err
	}
	select {
	case <-time.After(storeLockedRetryDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.store.AppendToolResult(ctx, sid, callID, name, content)
}

// beltTruncate caps s at beltCapBytes, appending a rune-safe marker that tells the
// model to narrow its output. Under the cap → unchanged.
func beltTruncate(s string) string {
	if len(s) <= beltCapBytes {
		return s
	}
	marker := fmt.Sprintf("\n\n... [已截断：%d/%d 字节超过上限，请让工具收窄输出（选择器/分页）]", beltCapBytes, len(s))
	keep := beltCapBytes - len(marker)
	if keep < 0 {
		return truncateBytes(marker, beltCapBytes)
	}
	return truncateBytes(s, keep) + marker
}

// truncateBytes returns the largest rune-aligned prefix of s that fits in max
// bytes (never splits a multi-byte rune at the boundary).
func truncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	b := []byte(s[:max])
	for len(b) > 0 {
		// A genuine malformed trailing byte decodes to RuneError with size 1; a
		// well-formed U+FFFD decodes with size 3 — keep the latter.
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}
