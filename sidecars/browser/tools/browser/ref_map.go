package browser

// Ref-ID protocol — Hermes-compatible.
//
// Hermes's browser_snapshot returns a text representation of the page's
// interactive elements, each tagged with a ref ID like @e1, @e2. Click
// and Type then reference those ref IDs instead of CSS selectors. This
// frees the model from having to construct or guess CSS selectors —
// which is especially helpful for small / local models.
//
// Implementation: a single JS injection that
//   1) finds every interactive / typeable element under the page (or
//      under a given root selector),
//   2) tags each one with a fresh data-bob-ref="@e<n>" attribute (so a
//      later click/type can locate it via the same attribute selector),
//   3) returns a structured list (tag, type, text, refId, visible) the
//      Go side renders into Hermes-style text.
//
// Re-snapshotting overwrites the prior tagging from @e1 again — refs are
// per-snapshot, not stable across snapshots. This matches Hermes; the
// model is taught (in the system prompt) to re-snapshot after any
// interaction that may have re-rendered the page.

const interactiveTagJS = `
(() => {
  const root = %s;
  // Strip any prior tagging so refs always start at @e1.
  for (const old of root.querySelectorAll('[data-bob-ref]')) old.removeAttribute('data-bob-ref');

  // Selector matches what the model can actually interact with.
  // role-based catches custom widgets (React etc. that aren't <button> tags).
  const sel = [
    'a[href]', 'button', 'input:not([type=hidden])', 'select', 'textarea',
    '[role=button]', '[role=link]', '[role=textbox]', '[role=checkbox]',
    '[role=combobox]', '[role=tab]', '[contenteditable=""]',
    '[contenteditable=true]', '[onclick]'
  ].join(',');

  const elements = [...root.querySelectorAll(sel)];
  const out = [];
  let n = 0;
  for (const el of elements) {
    // Skip totally invisible elements (display:none or detached). Off-screen
    // (scrolled away) is still listed — the model can scroll them in.
    if (el.offsetParent === null && getComputedStyle(el).position !== 'fixed' && el.tagName !== 'BODY') continue;
    n++;
    const ref = '@e' + n;
    el.setAttribute('data-bob-ref', ref);

    // Best-effort label extraction. Priority: aria-label > placeholder >
    // value > innerText > alt > title.
    const tag = el.tagName.toLowerCase();
    let text = (el.getAttribute('aria-label') || '').trim();
    if (!text) text = (el.placeholder || '').trim();
    if (!text && el.tagName === 'INPUT' && el.value) text = el.value.trim();
    if (!text) text = (el.innerText || '').trim().replace(/\s+/g, ' ');
    if (!text) text = (el.alt || el.title || '').trim();
    if (text.length > 80) text = text.slice(0, 77) + '...';

    out.push({
      ref: ref,
      tag: tag,
      type: el.getAttribute('type') || '',
      role: el.getAttribute('role') || '',
      text: text,
      href: tag === 'a' ? (el.getAttribute('href') || '') : '',
    });
  }
  return out;
})()
`

// interactive is the shape returned by interactiveTagJS, decoded by Go.
type interactive struct {
	Ref  string `json:"ref"`
	Tag  string `json:"tag"`
	Type string `json:"type"`
	Role string `json:"role"`
	Text string `json:"text"`
	Href string `json:"href"`
}

// renderRefList turns the list of tagged interactives into the
// Hermes-style text snapshot:
//
//	[@e1] <button> "Login"
//	[@e2] <input type=email> placeholder="Email"
//	[@e3] <a href="/about"> "About us"
//
// One line per element. Compact, easy for the model to scan, refs
// directly usable in browser_click / browser_type.
func renderRefList(items []interactive) string {
	var b strBuilder
	for _, e := range items {
		b.WriteString("[")
		b.WriteString(e.Ref)
		b.WriteString("] <")
		b.WriteString(e.Tag)
		if e.Type != "" {
			b.WriteString(" type=")
			b.WriteString(e.Type)
		}
		if e.Role != "" {
			b.WriteString(" role=")
			b.WriteString(e.Role)
		}
		b.WriteString(">")
		if e.Text != "" {
			b.WriteString(" \"")
			b.WriteString(e.Text)
			b.WriteString("\"")
		}
		if e.Href != "" {
			b.WriteString(" → ")
			b.WriteString(truncHref(e.Href))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// maxHrefChars caps a link's href in the snapshot. The model clicks by @e ref,
// so a link's URL is only "roughly where it goes" context — long ad/redirect/
// tracking URLs (e.g. Taobao's simba `?p=…&s=…&k=…` query strings, hundreds of
// chars each × hundreds of links) are pure bytes that blow up the snapshot.
// Capping to a recognizable prefix is universal DOM-distillation (browser-use /
// Playwright ariaSnapshot drop hrefs entirely) — no per-site logic, refs/clicks
// unchanged.
const maxHrefChars = 64

// browserTreeMaxChars caps the a11y-tree snapshot (navigate + snapshot tree
// mode). Raised above the generic textcap.DefaultMaxChars (16 KB) because a
// content-heavy page's tree (Taobao search ≈ 110 KB) was cut to ~14 %, leaving
// the model unable to see most results; with href pruning above + the
// stale-snapshot elision in the agent loop (only the latest snapshot is kept
// full), a larger latest tree is affordable. 32 KB ≈ 3× more visible elements.
const browserTreeMaxChars = 32 * 1024

func truncHref(h string) string {
	r := []rune(h)
	if len(r) <= maxHrefChars {
		return h
	}
	return string(r[:maxHrefChars]) + "…"
}

// strBuilder is a tiny strings.Builder alias to avoid an import bloat in
// this file (which is otherwise free of stdlib dependencies aside from
// the JS template).
type strBuilder struct{ buf []byte }

func (s *strBuilder) WriteString(x string) {
	s.buf = append(s.buf, x...)
}
func (s *strBuilder) String() string { return string(s.buf) }
