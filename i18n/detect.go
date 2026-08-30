package i18n

import (
	"strings"
	"unicode"
)

// Detect classifies text by the dominant script and returns the lang
// string T should look up. MVP grammar:
//
//   - count Han (CJK) runes vs ASCII letters
//   - empty / pure-symbols / pure-digits → "" (no signal)
//   - han ≥ latin → "zh"
//   - otherwise → "default"
//
// No latin-language sub-detection — "default" covers English and
// every other latin-script reply for the MVP. Returns "" only when
// there is no script signal at all, so the caller (the inbound flow's lang
// cascade) falls through to the override pin / the sender's remembered language /
// "default".
func Detect(text string) string {
	han, latin := 0, 0
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			latin++
		}
	}
	if han == 0 && latin == 0 {
		return "" // no script signal — let the caller fall through
	}
	if han >= latin {
		return "zh"
	}
	return "default"
}

// chain returns the lookup order T walks to satisfy a (key, lang)
// request. "zh-funky" → ["zh-funky", "zh", "default"]; "zh" → ["zh",
// "default"]; "en" → ["default"] (alias); unknown / empty → ["default"].
//
// The MVP recognises "zh-funky" as the one variant of "zh"; other
// "<base>-<variant>" forms also fall through base→default by the same
// dash-split rule, so a future "zh-formal" or "es-mx" works without
// code changes.
func chain(lang string) []string {
	if lang == "" || lang == "en" || lang == "default" {
		return []string{"default"}
	}
	out := []string{lang}
	// "<base>-<variant>" → also try the bare base before default.
	if i := strings.IndexByte(lang, '-'); i > 0 {
		out = append(out, lang[:i])
	}
	out = append(out, "default")
	return out
}
