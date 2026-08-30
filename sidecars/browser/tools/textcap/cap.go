// Package textcap is a tiny shared helper used by browser_*, web_extract,
// scrapling, and any future tool that returns large text content. It caps the
// output and appends a clear truncation marker so the model knows there was
// more — and how much.
//
// Why a cap at all: a single fat response (a Wikipedia article or a JS-heavy
// SPA's full innerText) easily runs to 100K+ tokens, which exceeds the
// context window of every self-hosted model we ship with by default. Once
// that lands in the conversation history, even auto-compression can't save
// us — the compress prompt itself OOMs the server. So the right place to
// stop the bleed is at the tool boundary, before the result ever enters
// the chat history.
package textcap

import (
	"fmt"
	"unicode/utf8"
)

// DefaultMaxChars is the floor we use when a tool didn't pass anything else.
// 16 KiB ≈ 4K tokens — comfortable headroom for several tool calls per turn
// even on a 32K-context model. Tune via per-tool config if you need more.
const DefaultMaxChars = 16 * 1024

// Apply returns (capped, truncated, totalChars). When the input fits, it's
// returned unchanged with truncated=false. When it doesn't, the prefix that
// fits inside (max - markerLen) chars is kept and a "... [truncated, X chars
// total. Use selector= or split URLs to narrow the snapshot.]" marker is
// appended so the model sees both that there's more and *how to ask for it*.
//
// The marker phrasing nudges the model toward narrowing strategies (selector
// for browser_snapshot, more specific URL for web_extract, etc.) instead of
// just shrugging.
func Apply(s string, max int) (out string, truncated bool, total int) {
	total = len(s)
	if max <= 0 {
		max = DefaultMaxChars
	}
	if total <= max {
		return s, false, total
	}
	marker := fmt.Sprintf(
		"\n\n... [truncated by tool — kept the first %d of %d chars. Use a CSS selector, a more specific URL, or paginate to get the rest.]",
		0, total,
	)
	keep := max - len(marker)
	if keep < 0 {
		// TD8 fix: caller's max is so small that the
		// marker itself won't fit. Honour the cap by trimming the
		// marker to max bytes rather than returning a longer-than-
		// requested string. Caller almost certainly mis-configured
		// (`max_output_bytes: 8`) and will see the cropped marker
		// which is signal enough to investigate.
		keep = 0
		if max < len(marker) {
			return marker[:max], true, total
		}
	}
	marker = fmt.Sprintf(
		"\n\n... [truncated by tool — kept the first %d of %d chars. Use a CSS selector, a more specific URL, or paginate to get the rest.]",
		keep, total,
	)
	// Rebuilding the marker with the real keep can render more digits
	// than the placeholder keep=0 did, making the marker longer than the
	// one keep was sized against — s[:keep]+marker would then overshoot
	// max. Recompute keep once more; the digit count is stable now that
	// keep is roughly right, so this second pass converges.
	keep = max - len(marker)
	if keep < 0 {
		keep = 0
	}
	// Snap keep down to a rune boundary so s[:keep] never chops a
	// multi-byte UTF-8 sequence mid-rune.
	for keep > 0 && keep < len(s) && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + marker, true, total
}
