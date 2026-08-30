package prompt

import "agentbob/contract"

// Prompt token sizing — a character-class heuristic shared by everyone who must judge
// how big an assembled prompt is: the turn core's §6.9 compaction trigger AND the model
// pool's context-window pre-routing (F75). They MUST use the SAME estimate or the two
// disagree — the turn compresses history to a budget the pool would then reject, or vice
// versa. A per-consumer copy silently drifts, so it lives here in the shared base (like
// SanitizeSpeaker), direct-imported.

// EstTokens estimates tokens by character class (spec §6.2.4): a wide char
// (CJK/kana/hangul/fullwidth) ≈ 1 token, everything else ≈ 0.25 (len/4 under-counts CJK
// ~2.7×, so compression would never trigger on Chinese).
func EstTokens(s string) int {
	total := 0.0
	for _, r := range s {
		if isWideChar(r) {
			total += 1
		} else {
			total += 0.25
		}
	}
	return int(total)
}

// EstTokensMsgs sums the token estimate over a message slice — content PLUS each
// assistant row's tool_call arguments (often the bulk of a large-args call). Stored
// history is text-only (no image bytes to add). The compaction split-walk and the model
// pool's pre-routing both call this so the accounting can't drift between them.
func EstTokensMsgs(msgs []contract.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstTokensMsg(m)
	}
	return total
}

// EstTokensMsg estimates one message's tokens — content PLUS each assistant row's
// tool_call arguments. The split walk and the compaction trigger MUST use this same
// per-message accounting or the recent tail is under-counted exactly when large tool
// args are present.
func EstTokensMsg(m contract.Message) int {
	total := EstTokens(m.Content)
	for _, tc := range m.ToolCalls {
		total += EstTokens(tc.Arguments)
	}
	return total
}

// isWideChar reports whether r is a wide (roughly one-token) character.
func isWideChar(r rune) bool {
	switch {
	case r >= 0x3000 && r <= 0x303F, // CJK symbols & punctuation
		r >= 0x3040 && r <= 0x30FF, // hiragana + katakana
		r >= 0x3400 && r <= 0x4DBF, // CJK ext A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xAC00 && r <= 0xD7AF, // hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFF00 && r <= 0xFFEF: // fullwidth forms
		return true
	}
	return false
}
