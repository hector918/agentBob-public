package epub

// 20-htmlwalk.go — DOM helpers over golang.org/x/net/html. The read slice only
// needs nav-link extraction (chapter titles from the epub3 nav doc); the
// translate/pack slice will add the render/split helpers. Ported from skeleton's
// tools/epub/40-htmlwalk.go.

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// skipTextParent reports whether an element's text must never be translated.
// script/style are not prose at all; pre/code are prose-shaped but must survive
// verbatim — a translated identifier or string literal makes the listing in a
// technical book uncompilable, and an inline <code>func</code> is a term, not a
// word. The cost is accepted: comments inside a listing stay untranslated, and so
// does the text of a book that uses <code> as plain emphasis.
func skipTextParent(tag string) bool {
	switch strings.ToLower(tag) {
	case "script", "style", "pre", "code":
		return true
	default:
		return false
	}
}

// collectTextNodes returns every visible text node under n in document order
// (whitespace-only and script/style text skipped). The pointers are into the live
// tree, so writing node.Data + re-rendering the root round-trips edits.
func collectTextNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(node *html.Node)
	walk = func(node *html.Node) {
		switch node.Type {
		case html.TextNode:
			if node.Parent != nil && node.Parent.Type == html.ElementNode && skipTextParent(node.Parent.Data) {
				return
			}
			if strings.TrimSpace(node.Data) == "" {
				return
			}
			out = append(out, node)
			return
		case html.ElementNode:
			if skipTextParent(node.Data) {
				return
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// renderHTML serializes a parsed DOM back to bytes.
func renderHTML(doc *html.Node) ([]byte, error) {
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// repairXMLDeclaration restores the XHTML `<?xml ... ?>` prolog that html.Render
// (an HTML5, not XML, serializer) mangles into a `<!--?xml ... ?-->` comment. When
// the original had no declaration, rendered is returned unchanged. Ported verbatim.
func repairXMLDeclaration(original, rendered []byte) []byte {
	decl, ok := leadingXMLDeclaration(original)
	if !ok {
		return rendered
	}
	interior := decl[len("<?xml") : len(decl)-len("?>")]
	bogus := []byte("<!--?xml" + interior + "?-->")
	out := rendered
	if i := bytes.Index(out, bogus); i >= 0 {
		stripped := make([]byte, 0, len(out)-len(bogus))
		stripped = append(stripped, out[:i]...)
		stripped = append(stripped, out[i+len(bogus):]...)
		out = stripped
	}
	repaired := make([]byte, 0, len(decl)+1+len(out))
	repaired = append(repaired, decl...)
	repaired = append(repaired, '\n')
	repaired = append(repaired, out...)
	return repaired
}

// leadingXMLDeclaration returns the verbatim `<?xml ... ?>` at the very start of b
// (after an optional BOM + whitespace), or ok=false.
func leadingXMLDeclaration(b []byte) (string, bool) {
	s := bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	s = bytes.TrimLeft(s, " \t\r\n")
	if !bytes.HasPrefix(s, []byte("<?xml")) {
		return "", false
	}
	end := bytes.Index(s, []byte("?>"))
	if end < 0 {
		return "", false
	}
	return string(s[:end+len("?>")]), true
}

// splitOversizedNodes rewrites the DOM so no single text node exceeds
// chunkTargetBytes — an over-budget node becomes several adjacent sibling text
// nodes (which render concatenated, so the split is invisible). Ported verbatim.
func splitOversizedNodes(root *html.Node) {
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTextParent(n.Data) {
			return
		}
		var kids []*html.Node
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			kids = append(kids, c)
		}
		for _, c := range kids {
			if c.Type == html.TextNode && len(c.Data) > chunkTargetBytes {
				for _, piece := range splitText(c.Data, chunkTargetBytes) {
					n.InsertBefore(&html.Node{Type: html.TextNode, Data: piece}, c)
				}
				n.RemoveChild(c)
				continue
			}
			walk(c)
		}
	}
	walk(root)
}

// splitText splits s into consecutive pieces each at most max bytes, preferring a
// sentence end, then whitespace, then a whole-rune boundary. Join == s always holds.
func splitText(s string, max int) []string {
	var out []string
	for len(s) > max {
		cut := splitPoint(s, max)
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// splitPoint returns the byte index in (0, max] at which to cut s.
func splitPoint(s string, max int) int {
	lastSentence, lastSpace, lastRune := 0, 0, 0
	for i, r := range s {
		end := i + utf8.RuneLen(r)
		if end > max {
			break
		}
		lastRune = end
		switch r {
		case ' ', '\t', '\r', '　':
			lastSpace = end
		case '.', '!', '?', '\n', '。', '！', '？', '…':
			lastSentence = end
		}
	}
	switch {
	case lastSentence > 0:
		return lastSentence
	case lastSpace > 0:
		return lastSpace
	case lastRune > 0:
		return lastRune
	default:
		_, size := utf8.DecodeRuneInString(s)
		return size
	}
}

// navLink is one <a href> from a nav document, kept as a pair so the list can
// stay ordered.
type navLink struct {
	Href string
	Text string
}

// extractNavLinks returns every <a href> in a nav document as an ordered slice,
// in document order. Order is the contract, not an accident: nested epub3 TOCs
// routinely point several links (chapter + its sections) at one file with
// different #fragments, and the caller strips the fragment — so the caller can
// only make "first occurrence wins" mean "the outermost/earliest heading" if the
// links arrive in the order the book wrote them. A map keyed by href would hand
// back a random winner on every read of the same book. Links with an empty href
// or empty text are skipped; nil on parse failure.
func extractNavLinks(htmlBytes []byte) []navLink {
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return nil
	}
	var out []navLink
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "a") {
			var href string
			for _, a := range n.Attr {
				if strings.EqualFold(a.Key, "href") {
					href = a.Val
					break
				}
			}
			if href != "" {
				if text := nodeText(n); text != "" {
					out = append(out, navLink{Href: href, Text: text})
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// nodeText returns the concatenated, whitespace-collapsed text of n and its
// descendants.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(node *html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}
