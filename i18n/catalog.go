// Package i18n holds the per-string lookup table for every user-facing
// reply the gateway (slash commands, queue-overflow notice, restart-
// missed banner, …) emits. The catalog is a JSON file embedded at
// compile time; admin can overlay per-key edits in $BOB_HOME/i18n/
// strings.json and per-chat variant pins in $BOB_HOME/i18n/overrides.yaml.
//
// Public API (only these are stable):
//
//	i18n.T(key, lang, args...) string
//	i18n.Detect(text string) string
//	i18n.Override(source, chatID string) string
//	i18n.Reload(bobHome string) error
//
// All four delegate to a single live *Catalog held in an atomic.Pointer
// so callers never block during a reload. Tests construct an explicit
// Catalog and call its methods directly.
//
// Design (locked):
//   - one global namespace; key prefix denotes call site:
//     "slash.<command>.<subcase>" for slash output,
//     "system.<purpose>" for non-slash gateway-level (queue overflow, …)
//   - fallback chain: variant ("zh-funky") → bare lang ("zh") → "default"
//   - "en" is an alias for "default"
//   - missing key + no default → return key (visible breakage signal)
//   - %s / %d placeholders pass through to fmt.Sprintf; positional
//     reorder is NOT supported (translator keeps argument order)
//   - per-chat variant pin (overrides.yaml) skips Detect; absent pin
//     falls through to per-reply detection.
package i18n

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

//go:embed strings.json
var embeddedStrings []byte

// Entry is one translatable string. default is mandatory (English);
// every other field is an optional language/variant rendering keyed by
// the lang string T receives. "_note" is a translator hint, ignored at
// runtime — it's preserved on disk so admin editing the overlay sees
// the same context.
//
// Decoded as map[string]string so unknown keys (a future "ja" or
// "es-formal") work without an embedded code change.
type Entry map[string]string

// Catalog is the live string table + chatID→variant override map.
// Construct via Load; swap in atomically via Reload. Fields are
// read-only after construction — concurrent T / Detect / Override
// calls don't need to hold a lock.
type Catalog struct {
	entries   map[string]Entry  // key → languages → text
	overrides map[string]string // "<source>:<chatID>" → variant
}

// overridesFile is the on-disk schema for $BOB_HOME/i18n/overrides.yaml.
// One section, "chat_variants", maps "source:chatID" → variant name
// (typically "zh", "zh-funky", "default"). Anything else is ignored.
type overridesFile struct {
	ChatVariants map[string]string `yaml:"chat_variants"`
}

// live is the process-wide Catalog readers reach via the package-level
// helpers. Set by Reload; nil until then (callers in pre-load tests
// pass an explicit Catalog).
var live atomic.Pointer[Catalog]

// Reload reads the embedded strings.json + overlays $BOB_HOME/i18n/
// strings.json (per-KEY) + loads $BOB_HOME/i18n/overrides.yaml, then
// atomically swaps the live catalog. Missing overlay/overrides files
// are OK; a parse error surfaces so admin sees the typo immediately.
//
// Idempotent — called once at startup (cmd/bob) before any slash dispatch
// fires; safe to re-call to hot-reload overlays if a trigger is ever wired.
func Reload(bobHome string) error {
	c, err := Load(bobHome)
	// Load degrades on a broken OVERLAY (returns the embedded-only catalog with
	// the error); only a broken EMBEDDED catalog yields c==nil. Store whatever
	// catalog we got BEFORE surfacing the error — a typo in an optional admin
	// overlay must cost only the overlay, never every user's strings.
	if c != nil {
		live.Store(c)
	}
	return err
}

// Load builds a Catalog from the embedded JSON + optional bobHome
// overlays. Exported so tests can construct a Catalog without touching
// the process-wide live pointer.
func Load(bobHome string) (*Catalog, error) {
	entries := map[string]Entry{}
	if err := json.Unmarshal(embeddedStrings, &entries); err != nil {
		return nil, fmt.Errorf("i18n: parse embedded strings.json: %w", err)
	}
	// Drop the documentation pseudo-entry so callers can't accidentally
	// resolve it.
	delete(entries, "_README")

	// loadErr accumulates optional-overlay failures. Each overlay
	// (strings.json, overrides.yaml) is independent: a typo in one must cost
	// only that overlay, never abort the assembly of the others. So we DON'T
	// early-return — we capture the error, skip the broken overlay's
	// contributions, fall through to the remaining sections, and surface the
	// joined error at the very end alongside the fully assembled Catalog.
	var loadErr error

	// Per-KEY overlay from $BOB_HOME/i18n/strings.json (if present).
	// Per-key, not per-file, so admin can ship a small file overriding
	// only the strings they actually want to tweak without copying the
	// whole catalog.
	if bobHome != "" {
		overlayPath := filepath.Join(bobHome, "i18n", "strings.json")
		if raw, err := os.ReadFile(overlayPath); err == nil {
			overlay := map[string]Entry{}
			if err := json.Unmarshal(raw, &overlay); err != nil {
				// Degrade, don't nuke: the embedded catalog is intact — serve it
				// and record the parse error (the caller logs it loudly). Fall
				// through so the overrides.yaml pins still load.
				loadErr = errors.Join(loadErr,
					fmt.Errorf("i18n: parse %s (serving embedded strings without the overlay): %w", overlayPath, err))
			} else {
				for k, v := range overlay {
					if k == "_README" {
						continue
					}
					if existing, ok := entries[k]; ok {
						// Per-lang merge: overlay wins on conflict, keeps
						// embedded langs the overlay didn't touch.
						for lang, text := range v {
							existing[lang] = text
						}
						entries[k] = existing
					} else {
						entries[k] = v
					}
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			loadErr = errors.Join(loadErr,
				fmt.Errorf("i18n: read %s (serving embedded strings without the overlay): %w", overlayPath, err))
		}
	}

	overrides := map[string]string{}
	if bobHome != "" {
		ovPath := filepath.Join(bobHome, "i18n", "overrides.yaml")
		if raw, err := os.ReadFile(ovPath); err == nil {
			var ov overridesFile
			if err := yaml.Unmarshal(raw, &ov); err != nil {
				loadErr = errors.Join(loadErr,
					fmt.Errorf("i18n: parse %s (serving without chat variants): %w", ovPath, err))
			} else {
				for k, v := range ov.ChatVariants {
					overrides[k] = v
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			loadErr = errors.Join(loadErr,
				fmt.Errorf("i18n: read %s (serving without chat variants): %w", ovPath, err))
		}
	}

	return &Catalog{entries: entries, overrides: overrides}, loadErr
}

// T resolves key in lang and Sprintfs args into the result. Fallback
// chain follows chain(lang); a key missing from every language in the
// chain returns the key itself so missing strings are visible in logs
// and in chat (vs silently empty).
func (c *Catalog) T(key, lang string, args ...any) string {
	entry, ok := c.entries[key]
	if !ok {
		return key
	}
	for _, l := range chain(lang) {
		text, ok := entry[l]
		if !ok || text == "" {
			continue
		}
		if len(args) == 0 {
			return text
		}
		out := fmt.Sprintf(text, args...)
		// A catalog string with a literal `%` (e.g. "50% off"), a stray verb,
		// or an arg-count mismatch makes fmt embed its error markers
		// (%!(NOVERB) / %!(EXTRA …)). Showing the raw string is strictly better
		// than leaking that garbage to the user, so fall back on detecting it —
		// unless an arg itself contains "%!" (user/provider-controlled data),
		// in which case the marker proves nothing and the formatted output
		// stands.
		if strings.Contains(out, "%!") && !argsContainMarker(args) {
			return text
		}
		return out
	}
	return key
}

// argsContainMarker reports whether any arg's own rendering contains the
// fmt error marker "%!". When it does, T's post-Sprintf sniff can't tell a
// template error from legitimately marker-bearing data, so the fallback is
// skipped rather than dropping the user's data for a false positive.
func argsContainMarker(args []any) bool {
	for _, a := range args {
		if strings.Contains(fmt.Sprint(a), "%!") {
			return true
		}
	}
	return false
}

// override returns the admin's per-chat variant pin (overrides.yaml), or "". The
// language RESOLUTION (override → Detect → per-sender memory → "default") lives in
// the inbound flow now — i18n stays a pure detect/translate/override-lookup library,
// holding no per-chat or per-sender state.
func (c *Catalog) override(source, chatID string) string { return c.overrides[source+":"+chatID] }

// T is the package-level shortcut. Safe to call after Reload; before
// Reload it returns the key itself (visible breakage signal — tests
// and the rare pre-Reload startup code path see this).
func T(key, lang string, args ...any) string {
	c := live.Load()
	if c == nil {
		return key
	}
	return c.T(key, lang, args...)
}

// Override returns the admin's per-chat variant pin (overrides.yaml) for this
// scope, or "" when none. The flow's lang cascade consults it FIRST (an admin pin
// outranks detection + the per-sender/session memory). "" when the catalog isn't
// loaded yet.
func Override(source, chatID string) string {
	c := live.Load()
	if c == nil {
		return ""
	}
	return c.override(source, chatID)
}
