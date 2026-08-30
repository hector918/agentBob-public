// Package scrub redacts credential-shaped bytes from any string the agent
// is about to feed to a model (tool results, historical messages, etc.).
//
// History: an earlier session leaked real OpenSSH private key bytes into
// the conversation context. Root cause was never fully pinned down; this
// module is the belt-and-suspenders defence — any path that takes free-form
// text from disk / tool stdout / store rows runs through Scrub before it
// reaches the prompt assembler or the wire.
//
// Patterns are conservative on purpose: each one is anchored on a high-
// signal token (PEM armor, known service prefix, OpenSSH base64 magic)
// so we don't blast every long string. False negatives are fine
// (path-policy and broker boundary still defend) — false positives that
// scrub legitimate tool output would silently degrade the model's
// answers, which is worse.
package scrub

import (
	"regexp"
	"strings"
)

// redactionMarker is what every match is replaced with. Picked as a single
// token so the model sees a clear signal "something was here, it was
// dropped on purpose, don't try to reconstruct it".
const redactionMarker = "[REDACTED:credential]"

// pemBlock matches any RFC 7468 PEM block from BEGIN through matching END.
// `[A-Z ]*(KEY|CERTIFICATE)` covers OpenSSH/RSA/EC/DSA/PGP private keys
// (PRIVATE KEY etc.) AND a bare `-----BEGIN CERTIFICATE-----` — the `*`
// (not `+`) is load-bearing: the common single-word X.509 form has no
// word before "CERTIFICATE", and a `+` silently let it slip through
// un-redacted. `(?s)` lets `.` cross newlines; `.*?` keeps the match
// lazy so two adjacent PEM blocks don't merge into one giant match.
var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*(?:KEY|CERTIFICATE)-----.*?-----END [A-Z ]*(?:KEY|CERTIFICATE)-----`)

// pemOrphanBegin — fallback for a PEM block whose END marker never made it
// into the string: upstream caps HEAD-truncate long tool output (e.g. exec
// stdout is cut at a byte budget BEFORE scrub runs), which can leave a BEGIN
// armor header plus key bytes with no matching END. pemBlock's lazy pass
// above has already consumed every properly paired block, so any BEGIN
// header still standing is by construction unpaired — redact from it to the
// end of the string. Over-redaction surface is only text that follows a
// literal orphan armor header, which in practice is key material.
var pemOrphanBegin = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*(?:KEY|CERTIFICATE)-----.*$`)

// opensshArmorBase64 catches the well-known base64-encoded prefix of a
// PEM-armored OpenSSH private key seen in transit (e.g. when the
// surrounding text was JSON-escaped or otherwise mangled so pemBlock
// missed it). ALL THREE alignment rotations are matched (a key prefixed by
// 1-2 foreign bytes before encoding shifts every quantum — the standard
// gitleaks approach), each with its alignment-ambiguous lead chars dropped;
// rotation 0 is the one proven against the leak observed last session.
// Seeing any rotation standalone is almost certainly a key.
var opensshArmorBase64 = regexp.MustCompile(`(?:LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0|0tLS0tQkVHSU4gT1BFTlNTSCBQUklWQVRFIEtFWS0tLS0t|tLS0tLUJFR0lOIE9QRU5TU0ggUFJJVkFURSBLRVktLS0tLQ)[A-Za-z0-9+/=]*`)

// awsSecret — AWS_SECRET_ACCESS_KEY=... line. Stops at whitespace / quote /
// semicolon so we don't eat the rest of a config file. Both export-style and
// bare-assignment shells covered.
var awsSecret = regexp.MustCompile(`(?i)AWS_SECRET_ACCESS_KEY\s*[:=]\s*["']?[A-Za-z0-9/+=]{30,}["']?`)

// slackBot — Slack bot token; the `xoxb-` prefix is unique enough.
var slackBot = regexp.MustCompile(`xoxb-[A-Za-z0-9-]{10,}`)

// githubPAT — GitHub personal access token (classic `ghp_` and fine-grained
// `github_pat_` both covered).
var githubPAT = regexp.MustCompile(`(?:ghp_|github_pat_)[A-Za-z0-9_]{20,}`)

// dsnUserinfoPassword matches the password field in a URL userinfo block —
// `<scheme>://user:SECRET@host`. The most common secret in this pg-DSN-driven
// codebase: a pg / mysql / redis / amqp / mongodb error that echoes its
// connection URL would otherwise leak the password verbatim. We redact ONLY
// the password capture group and keep scheme / user / host so the log stays
// diagnosable ("connection to db at host failed"). Anchored on `://` then a
// `user:` segment (neither part containing `:`/`@`/`/`/whitespace) then `@`,
// so a plain URL with no userinfo (`https://host/path`) never matches.
// Replacement uses $1/$2 to preserve everything but the password.
var dsnUserinfoPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]*:)([^@/\s]+)(@)`)

// Provider API keys — unique high-signal prefix + 20+ char body so short
// `sk-foo` prose tokens are left alone. Go's RE2 has no lookbehind, so each
// pattern captures a `(^|[^A-Za-z0-9])` left boundary and the replacement
// restores it via ${1}: without it, hyphenated alnum IDs ending in the
// prefix letters (`task-…`, `disk-…`) match mid-word and get silently
// corrupted — exactly the false-positive class the package doc forbids.
//
// prefixedSKKey — `sk-<label>-…` shapes: Anthropic `sk-ant-…` and modern
// OpenAI `sk-proj-…` / `sk-svcacct-…` / `sk-admin-…`. Their bodies contain
// `-`/`_`, which breaks the alnum-only openaiKey body, and the label alone
// is too short to start a bare-`sk-` match — so without this pattern they'd
// pass through (or be only partially eaten). Applied before the bare `sk-`
// pattern; the explicit label list keeps the wider `[-_]` charset from
// over-matching hyphenated `sk-…` prose (bare `sk-` stays alnum-only on
// purpose).
var prefixedSKKey = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-(?:ant|proj|svcacct|admin)-[A-Za-z0-9_-]{20,}`)
var openaiKey = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-[A-Za-z0-9]{20,}`)
var xaiKey = regexp.MustCompile(`(^|[^A-Za-z0-9])xai-[A-Za-z0-9]{20,}`)

// bearerToken — `Authorization: Bearer <token>` headers and bare `Bearer <jwt>`
// spans. 20+ char body keeps it off the word "Bearer" in prose. Redacts the
// whole "Bearer ..." span (the literal word carries no value).
var bearerToken = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._+/=-]{20,}`)

// envAssignmentSecret — env-dump / config shapes where the KEY NAME ends in a
// secret-ish word (`*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`) and is
// assigned a non-trivial value. We keep the key name (so the log says WHICH
// secret was present) and redact only the value via the $1 capture group.
//
// Conservative anchor: the key must be an env-var-shaped identifier — either
// it carries an `<ident>_` prefix (`OPENAI_API_KEY`, `db_password`) OR the
// secret word itself is ALL-CAPS (`PASSWORD=`, `TOKEN=`). This is what keeps
// the very common prose phrase "token: <something>" from being treated as an
// assignment. The value is one of: a balanced double/single-quoted run (so a
// quoted config value `PASSWORD: "superSecret99"` redacts including its quotes),
// or an 8+-char DELIMITER-AWARE run (stops at whitespace / quote / comma /
// semicolon / closing brace) — matching the stop-at-delimiter convention.
// The optional closing quote after the key name admits the JSON shape
// (`"DB_PASSWORD": "..."` — docker inspect / kubectl -o json / config.json),
// the dominant env-dump form that the bare-identifier anchor used to miss.
// awsSecret/bearerToken use. The delimiter-aware unquoted arm is what keeps a
// secret embedded in JSON/CSV/compact config (`"API_KEY=abcdefgh","next":...`)
// from swallowing the closing quote/comma and corrupting the surrounding
// structure. `PASSWORD=` with an empty / tiny value is left alone.
var envAssignmentSecret = regexp.MustCompile(`((?:[A-Za-z0-9]+_(?i:API_KEY|TOKEN|SECRET|PASSWORD)|API_KEY|TOKEN|SECRET|PASSWORD)["']?\s*[:=]\s*)(?:"[^"]{8,}"|'[^']{8,}'|[^\s"',;}]{8,})`)

// longHighEntropyB64 is the safety net for armor-less raw base64 dumps.
// Conditions stacked at the call site (see scrubLongBase64) ensure we
// only redact when the run ALSO sits within 100 chars of "PRIVATE KEY"
// or contains the OpenSSH armor prefix — neither tested via regex
// alone because that would either over-match (every long base64 string)
// or under-match (anchors fight with each other in one expression).
var longHighEntropyB64 = regexp.MustCompile(`[A-Za-z0-9+/=]{200,}`)

// Scrub redacts every credential-shaped span in s and returns the
// cleaned string. Safe to call on empty / nil-equivalent input
// (returns s unchanged). Idempotent — running it twice produces the
// same output as once.
func Scrub(s string) string {
	if s == "" {
		return s
	}
	s = pemBlock.ReplaceAllString(s, redactionMarker)
	// Orphan-BEGIN fallback AFTER the paired pass: any BEGIN header that
	// survived pemBlock has no matching END (truncated key) — redact to EOS.
	s = pemOrphanBegin.ReplaceAllString(s, redactionMarker)
	s = opensshArmorBase64.ReplaceAllString(s, redactionMarker)
	s = awsSecret.ReplaceAllString(s, redactionMarker)
	s = slackBot.ReplaceAllString(s, redactionMarker)
	s = githubPAT.ReplaceAllString(s, redactionMarker)
	// DSN password before the generic key/env patterns so the URL form is
	// handled specifically (keep scheme/user/host, redact only the password).
	s = dsnUserinfoPassword.ReplaceAllString(s, "${1}"+redactionMarker+"${3}")
	// Prefixed `sk-<label>-…` shapes before the bare `sk-` pattern: a
	// `sk-proj-…` body with a long alnum run inside would otherwise be only
	// partially eaten by openaiKey, leaving the prefix + separators behind.
	// ${1} restores the captured left-boundary char (see pattern comment).
	s = prefixedSKKey.ReplaceAllString(s, "${1}"+redactionMarker)
	s = openaiKey.ReplaceAllString(s, "${1}"+redactionMarker)
	s = xaiKey.ReplaceAllString(s, "${1}"+redactionMarker)
	s = bearerToken.ReplaceAllString(s, redactionMarker)
	// Env-assignment: keep the key name (capture group 1), redact the value.
	s = envAssignmentSecret.ReplaceAllString(s, "${1}"+redactionMarker)
	s = scrubLongBase64(s)
	return s
}

// scrubLongBase64 implements the conditional long-run redaction:
// any 200+ char base64-like run is only redacted when it ALSO either
//
//   - contains the OpenSSH armor prefix bytes inside (defence-in-depth
//     for a base64 dump that swallowed the BEGIN marker mid-run), OR
//   - sits within 100 chars after the literal substring "PRIVATE KEY"
//     in the surrounding text (the body of an armor-less key block
//     pasted into stdout).
//
// Anything else is kept verbatim — long base64 alone (image data URIs,
// JWT payloads, signed URLs, ...) is not a credential signal.
//
// Each match is anchored against its OWN offset (FindAllStringIndex,
// not strings.Index): if the same base64 run appears more than once,
// strings.Index would resolve every occurrence to the first one's
// position — so a real key in the second copy would dodge the
// "near PRIVATE KEY" anchor while a benign first copy got redacted.
func scrubLongBase64(s string) string {
	// All three base64 alignment rotations of the armor header (see
	// opensshArmorBase64).
	armorMarkers := []string{
		"LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZ",
		"0tLS0tQkVHSU4gT1BFTlNTSCBQUklWQVRFIEtFWQ",
		"tLS0tLUJFR0lOIE9QRU5TU0ggUFJJVkFURSBLRVk",
	}
	const nearbyMarker = "PRIVATE KEY"
	const nearbyWindow = 100

	locs := longHighEntropyB64.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		match := s[start:end]
		redact := false
		for _, marker := range armorMarkers {
			if strings.Contains(match, marker) {
				redact = true
				break
			}
		}
		if !redact {
			// Look back up to nearbyWindow chars before THIS match's
			// real offset for "PRIVATE KEY".
			windowStart := start - nearbyWindow
			if windowStart < 0 {
				windowStart = 0
			}
			redact = strings.Contains(s[windowStart:start], nearbyMarker)
		}
		b.WriteString(s[prev:start])
		if redact {
			b.WriteString(redactionMarker)
		} else {
			b.WriteString(match)
		}
		prev = end
	}
	b.WriteString(s[prev:])
	return b.String()
}
