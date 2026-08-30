package browser

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"agentbob/sidecars/browser/bobhome"
	"agentbob/sidecars/browser/core"
)

// clearStaleProfileLocks removes chromium's user-data-dir singleton files
// (SingletonLock — a symlink — plus SingletonSocket / SingletonCookie) from dir.
// Chromium writes them on launch and deletes them on a CLEAN exit; an unclean
// exit (crash / OOM / a bob restart that SIGKILLs the subprocess) leaves them
// behind, and the next launch then fails with "the profile appears to be in use
// by another Chromium process" and chromium won't start.
//
// The caller MUST hold this profile's EXCLUSIVE custodian checkout, so no live
// chromium bob is tracking owns these files. The cross-restart case is safe too:
// on Linux (the deploy target) chromedp launches chromium with Pdeathsig=SIGKILL,
// so a bob restart/crash kills every chromium child — locks found after a restart
// are owned by a dead process and are stale by construction. Clearing them lets a
// restarted bob re-open a preserved login instead of dead-locking on it.
// Best-effort: a missing file is the normal (clean-exit) case; any other remove
// error is logged and ignored so the launch surfaces the real failure.
func clearStaleProfileLocks(dir string) {
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("browser profile: stale lock cleanup failed", "path", p, "err", err)
		}
	}
}

// ProfileCapability is the credential NAME the browser-profile gate checks:
// the grant `credential:use:browser-profile` on a role/user means "may use a
// browser login profile". It's a FIXED capability, NOT a per-identity name —
// because the permission matrix + boot reconcile only keep grants whose
// credential is in the live registry, and a per-member `browser_<id>` name is
// dynamic (lazy, unregistered) so reconcile would drop it as an orphan. One
// fixed capability is registry-friendly (added to the reconcile alive set) and
// survives reconcile. WHICH profile a turn uses is still per-identity (the dir
// + pool/custodian key = ProfileName(MemberID|AccountID)); the capability only
// authorizes "you may use YOUR profile". The acting identity is server-resolved
// in agora (MemberID, unspoofable), so capability + self-key is safe there.
// Unlike the client-building credential kinds (ssh / bearer / dsn), the grant
// builds NO client: the asset is a live chromium user-data-dir (cookies /
// localStorage), and the backing dir materialises lazily on the first
// authorized use.
const ProfileCapability = "browser-profile"

// ProfileIdentity is the single gate decision for the per-person browser
// profile: it returns the identity whose profile this turn is authorized to
// use, or "" for the legacy per-scope path. The identity is the one THE TURN
// ACTS AS — agora's server-resolved MemberID first, the personal AccountID
// otherwise (docs/remote-browser-handoff.md). A missing identity, a nil gate
// (no permissions plugin), or a non-allow decision on the fixed
// ProfileCapability all mean "legacy" — the feature stays opt-in.
//
// Shared by Pool.profileRoute (in-process tools) and the browserd thin client
// (tools/browserremote), so the security decision lives in exactly one place
// and ALWAYS runs inside bob — browserd itself never gates (docs/browserd.md).
func ProfileIdentity(ctx context.Context, gate core.Gate, actx core.AgentCtx) string {
	id := actx.MemberID
	if id == "" {
		id = actx.AccountID
	}
	if id == "" || gate == nil {
		return ""
	}
	// Gate on the FIXED capability (credential:use:browser-profile) — a dynamic
	// per-identity grant can't survive boot reconcile (orphan drop). WHICH
	// profile is used is still per-identity (ProfileName/ProfileDir of id).
	if dec := gate.Check(ctx, "credential", ProfileCapability); dec.Kind != core.PermAllow {
		return ""
	}
	return id
}

// profilePoolKeyPrefix is the prefix ProfileName puts in front of an identity
// to form the pool/custodian key. Single constant so the forward (ProfileName)
// and reverse (ProfileVaultKeyFromPoolKey) mappings cannot drift.
const profilePoolKeyPrefix = "browser_"

// ProfileName is the credential name for identity's browser profile, e.g.
// ProfileName("ac_hector") == "browser_ac_hector". The gate the browser tools
// check is therefore credential:use:browser_<identity>.
//
// The separator is an UNDERSCORE, not a colon: a credential grant lives in the
// permission matrix as a 3-segment `kind:verb:name` string (permissions
// grantRe), so the name segment must contain no colon. `browser:<id>` produced
// `credential:use:browser:<id>` — four segments — which the matrix rejected,
// silently dropping the whole agora roles file to a parse error. Keep this the
// single source of truth so the gate string and the admin grant agree.
func ProfileName(identity string) string { return profilePoolKeyPrefix + identity }

// ProfileVaultRoot returns the vault directory that holds every per-identity
// browser profile dir (ProfileDir creates children directly under it).
// Exported for browserd's profile export/import face (docs/browserd.md P3),
// which lists / packs profile dirs without knowing any identity up front.
func ProfileVaultRoot() string { return bobhome.Path("credentials", "browser-profiles") }

// ValidProfileVaultKey reports whether key is exactly a sanitized vault dir
// segment — i.e. the same mapping ProfileDir applies to an identity would
// leave it unchanged. Anything sanitization would have altered (path
// separators, "..", colons, leading/trailing dots) is rejected, so a
// caller-supplied key can never address outside the vault root. Used by the
// browserd export/import endpoints to validate URL-supplied keys.
func ValidProfileVaultKey(key string) bool { return key != "" && key == sanitizeProfileID(key) }

// ProfileVaultKeyFromPoolKey maps a pool/custodian profile key (the
// ProfileName(identity) form) back to the vault dir segment ProfileDir uses
// for the same identity. Single source of truth for the pool-key ↔ on-disk
// dir relation — browserd's export face needs it to decide whether a vault
// dir is currently checked out.
func ProfileVaultKeyFromPoolKey(poolKey string) string {
	return sanitizeProfileID(strings.TrimPrefix(poolKey, profilePoolKeyPrefix))
}

// ProfileDir returns (creating on demand, 0o700) the chromium user-data-dir for
// identity's browser profile, under the credentials vault root:
//
//	$BOB_HOME/credentials/browser-profiles/<sanitized identity>/
//
// The profile is a credential-grade asset, but its backing is a multi-file live
// directory, so it sits in the vault root AS a dir (not inline in a yaml like an
// SSH key's PEM). The 0o700 dir under the already-0o700 credentials root gives
// it the same at-rest protection as every other credential — bob does not
// encrypt secrets at rest (credentials/00-doc.go), so a profile dir needs no
// decrypt/encrypt cycle and never fights chromium's live-mutable-dir model.
//
// Callers MUST gate (credential:use:ProfileName(identity)) BEFORE calling this —
// creating the dir is an authorized-use side effect, not a public lookup.
func ProfileDir(identity string) (string, error) {
	clean := sanitizeProfileID(identity)
	if clean == "" {
		return "", fmt.Errorf("browser profile: empty / invalid identity %q", identity)
	}
	dir := bobhome.Path("credentials", "browser-profiles", clean)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("browser profile: create dir %s: %w", dir, err)
	}
	return dir, nil
}

// sanitizeProfileID maps an identity to a filesystem-safe single path segment:
// keep [A-Za-z0-9_.-], replace anything else with '_', then reject results that
// are empty or path-traversal (".", ".."). Defense-in-depth — identities are
// normally AccountIDs ("ac_…") or SourceHandle fallbacks, but a hostile / odd
// handle must never escape the browser-profiles root.
func sanitizeProfileID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return ""
	}
	return out
}
