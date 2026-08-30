// File 15-strings.go holds standalone string-handling helpers used
// across the codebase. They landed in core/56-urllib.go historically
// as a cycle workaround (urllib subpackages needed them but the urllib
// root needed to import the subpackages); today the callers are
// agora/, tools/, pipeline-gateway/, none of which care about URLs,
// so the helpers live in their own file where the name matches the
// symbol surface.
package core

import "unicode/utf8"

// TruncateRunes returns s clipped to maxBytes without slicing a UTF-8
// rune (BUG #8 from the audit). Shared by all URL-library
// impls so Record / RecordQuery store valid UTF-8 in TEXT columns
// regardless of backend.
func TruncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := 0
	for end < maxBytes {
		_, size := utf8.DecodeRuneInString(s[end:])
		if size == 0 || end+size > maxBytes {
			break
		}
		end += size
	}
	return s[:end]
}

// TruncateRunesEllipsis returns s clipped to at most maxRunes
// (rune-aware) with a "…" appended when truncation actually happened.
// Used by chat-facing / prompt-facing renderers that want to make
// clipping visible to a human reader (e.g. log lines, todo content
// previews, button text). For raw-byte storage (no ellipsis), use
// TruncateRunes instead.
//
// maxRunes <= 0 → empty string (degrades safely; callers can rely on
// "give nothing rather than something with no budget").
//
// Consolidates the previous gateway.truncate and coretools.truncateRunes
// helpers that had drifted into near-identical copies (D5 audit).
func TruncateRunesEllipsis(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
