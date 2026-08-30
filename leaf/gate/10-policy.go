// Package gate is the single inbound sender screen + admin authority. It owns
// each source's access policy (allow / deny / admins / per-group overrides),
// runs the access-axis triage on every inbound event (denylist > bare-code
// redeem > allowlist, docs/accounts.md §6.1), and answers admin questions from
// the same policy. It registers a contract.Screener on the trunk; the central
// inbound flow is its only caller — no source screens earlier in its own
// protocol phase (email's IMAP header peek is pure dedup and defers access
// control here).
package gate

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// IDList is a flat list of platform-native sender ids (numeric Telegram ids
// stringified, string Slack ids pass-through). Matching is exact-string, with
// one carve-out: entries carrying "@" (email addresses) compare
// case-insensitively — the email source canonicalizes From to lowercase while
// operators naturally type mixed-case addresses into the yaml, so an exact
// compare would silently miss (allowlist fails closed, denylist fails OPEN).
// Platform ids never contain "@", so their case-sensitive exact match is
// untouched.
type IDList []string

// idEqual is the single id-comparison rule shared by list matching and the
// granter's already-present check: exact, except @-carrying ids (email
// addresses) compare case-insensitively — see the IDList doc.
func idEqual(a, b string) bool {
	return a == b || (strings.Contains(a, "@") && strings.EqualFold(a, b))
}

// Contains reports whether id is in the list (per the idEqual rule).
func (l IDList) Contains(id string) bool {
	for _, x := range l {
		if idEqual(x, id) {
			return true
		}
	}
	return false
}

// Group overrides allow/deny/admin for one group chat, keyed by chat id in
// Policy.Groups. DM scoping uses the source-level allow/denylist, not this map.
//
//   - AllowAll: per-chat OPEN — anyone may talk in this chat, bypassing the
//     source-level allowlist (so a globally-closed source can still have one
//     wide-open group). Denylist still wins; the @-mention group-noise gate is
//     separate (it lives at the source leaf).
//   - Allowlist: per-chat AND — when populated, only listed users may talk here.
//   - AllowlistAdd: per-chat UNION — listed users are granted access here ON TOP
//     of the source-level allowlist.
//   - Denylist: per-chat OR — listed users are denied here.
//   - Admins: per-chat UNION with source-level Admins.
type Group struct {
	AllowAll     bool   `yaml:"allow_all,omitempty"`
	Allowlist    IDList `yaml:"allowlist,omitempty"`
	AllowlistAdd IDList `yaml:"allowlist_add,omitempty"`
	Denylist     IDList `yaml:"denylist,omitempty"`
	Admins       IDList `yaml:"admins,omitempty"`
}

// Policy is one source's access policy — "who is allowed to talk to bob on this
// source". One flat file per source name under <sourcesDir>/<name>.yaml. The
// zero value is CLOSED (allow_all:false, empty allowlist → nobody authorized),
// so a Gated source whose file is missing or failed to load fails shut.
//
// Connection config (enabled / token env) is NOT here — that lives in the
// central config and .env, owned by the source bus. This file carries only
// access policy and hot-reloads.
type Policy struct {
	AllowAll  bool             `yaml:"allow_all"`
	Allowlist IDList           `yaml:"allowlist,omitempty"`
	Denylist  IDList           `yaml:"denylist,omitempty"`
	Admins    IDList           `yaml:"admins,omitempty"`
	Groups    map[string]Group `yaml:"groups,omitempty"`
	// Onboarding OPENS this source to onboarding: an un-allowlisted (non-denied) sender is
	// PASSED through to the router (→ intro creates a bare pending account) instead of being
	// bounced. Access is then a granted entitlement (account flow), gated downstream — the
	// allowlist becomes a roster/record, the denylist stays the hard block. Default FALSE:
	// an unconfigured source keeps the closed bounce-on-un-allowlisted behavior, so opening
	// onboarding is a deliberate per-source opt-in (and a no-accounts deployment is never
	// silently thrown open).
	Onboarding bool `yaml:"onboarding,omitempty"`
}

// IsAdmin reports whether userID is an admin in chatID on this source.
// Source-level admins apply to every chat; per-chat admins overlay (additive).
func (p Policy) IsAdmin(chatID, userID string) bool {
	if p.Admins.Contains(userID) {
		return true
	}
	if g, ok := p.Groups[chatID]; ok && g.Admins.Contains(userID) {
		return true
	}
	return false
}

// Denied reports whether userID is on the source-level OR the chat-level
// denylist — the "denylist wins over everything" check run BEFORE the bare-code
// redeem (a denylisted sender must not be able to bind via a code).
func (p Policy) Denied(chatID, userID string) bool {
	if p.Denylist.Contains(userID) {
		return true
	}
	if g, ok := p.Groups[chatID]; ok && g.Denylist.Contains(userID) {
		return true
	}
	return false
}

// Authorized applies the two-tier allow/deny model:
//
//	denied if on the source-level denylist OR the chat's denylist;
//	else allowed if on the chat's allowlist_add OR the chat is allow_all;
//	else, if the chat has an allowlist and the user isn't on it → denied;
//	else allowed iff the source is allow_all OR on the source-level allowlist.
func (p Policy) Authorized(chatID, userID string) bool {
	if p.Denylist.Contains(userID) {
		return false
	}
	if g, ok := p.Groups[chatID]; ok {
		if g.Denylist.Contains(userID) {
			return false
		}
		// Per-chat UNION grant: accept the user in THIS chat even if absent
		// from the source-level allowlist. Checked BEFORE the restrictive
		// per-chat allowlist gate — allowlist_add is an unconditional grant, so
		// a populated per-chat allowlist must not shadow it.
		if g.AllowlistAdd.Contains(userID) {
			return true
		}
		// Per-chat OPEN: this group is open to everyone, bypassing the
		// source-level allowlist. Checked before the restrictive per-chat
		// allowlist gate — allow_all means "anyone". Denylist (above) wins.
		if g.AllowAll {
			return true
		}
		if len(g.Allowlist) > 0 && !g.Allowlist.Contains(userID) {
			return false
		}
	}
	if p.AllowAll {
		return true
	}
	return p.Allowlist.Contains(userID)
}

// LoadAll scans <dir>/*.yaml and returns one Policy per file, keyed by the
// filename without extension (= the source name). A missing directory yields an
// empty map (no policies configured) without error. Any single file that fails
// to parse is fatal — operators get a loud "this file is broken" signal instead
// of a source silently falling to the closed-default.
func LoadAll(dir string) (map[string]Policy, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("gate: scan %s: %w", dir, err)
	}
	out := make(map[string]Policy, len(matches))
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("gate: read %s: %w", path, err)
		}
		var p Policy
		if err := yaml.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("gate: parse %s: %w", path, err)
		}
		// A typo'd top-level key ('allowlst:') parses into a zero Policy with no
		// error → the source silently falls to the closed default. Not fatal (one
		// typo shouldn't brick every source), but WARN loud so the operator sees it
		// (S-24; mirrors the write path's guard).
		if gerr := guardTopKeys(b); gerr != nil {
			slog.Warn("gate: policy file has an unmodeled top-level field (typo?) — it is ignored, so this source may be unexpectedly closed", "file", path, "err", gerr)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		out[name] = p
	}
	return out, nil
}
