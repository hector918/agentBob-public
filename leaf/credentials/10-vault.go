// Package credentials is warrant's secret backend: per-credential .env files
// under $BOB_HOME/credentials/ (the on-disk vault, NOT the store — a locked
// decision, docs/credentials.md §11). It builds configured clients (an ssh
// client now) without the secret reaching the caller. It is consumed ONLY by
// warrant (which gates credential:use BEFORE asking for a build); tools never
// see it.
//
// SLICE 3: the .env parser (ported, zero-dep) + the ssh builder. bearer/dsn/http
// builders (for web tools) land with those tools.
package credentials

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// load reads + parses $BOB_HOME/credentials/<name>.env into (kind, data).
func load(home, name string) (kind string, data map[string]string, err error) {
	raw, err := readCred(home, name)
	if err != nil {
		return "", nil, err
	}
	return parseEnv(raw)
}

// readCred reads $BOB_HOME/credentials/<name>.env bytes (name-guarded). Shared by
// load (full parse) and kindOf (kind-only) so the read + path + not-found error
// shape is defined once.
func readCred(home, name string) ([]byte, error) {
	if !credNameRe(name) {
		return nil, fmt.Errorf("credentials: invalid name %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(home, "credentials", name+".env"))
	if err != nil {
		// Don't echo the on-disk vault path out to the caller (it flows up through
		// warrant to the model) — name it by the credential, not the filesystem (S-35).
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("credentials: %q not found", name)
		}
		// The raw os error (EACCES vs EIO vs symlink loop) goes to the server log
		// only — without it a mis-chowned vault dir is undiagnosable.
		slog.Warn("credentials: read failed", "name", name, "err", err)
		return nil, fmt.Errorf("credentials: cannot read %q", name)
	}
	return raw, nil
}

// kindOf returns ONLY the kind= field of a credential, parsing just the first
// field instead of the whole map. namesByKind classifies every vault file on each
// resolve, so it must not parse + hold every secret value just to learn a file's
// kind (less work + fewer secret bytes lingering in maps).
func kindOf(home, name string) (string, error) {
	raw, err := readCred(home, name)
	if err != nil {
		return "", err
	}
	return parseKind(raw)
}

// namesByKind scans $home/credentials/*.env and returns the names whose parsed
// kind= equals kind. A file that fails to parse (or has an invalid name) is
// skipped — one malformed vault file must not break resolution for the good
// ones. Missing dir → empty (not an error: a deploy may have no credentials yet).
func namesByKind(home, kind string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(home, "credentials"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		slog.Warn("credentials: vault dir read failed", "err", err)
		return nil, fmt.Errorf("credentials: cannot read vault dir")
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".env")
		k, err := kindOf(home, name)
		if err != nil {
			// WARN-skip (not silent): a malformed/badly-named granted credential
			// would otherwise vanish from its kind's candidate set, mis-resolving as
			// "none authorized" and sending the operator to fix permissions instead
			// of the file. Don't echo the secret bytes — just name + parse error.
			slog.Warn("credentials: skipping unreadable vault file in kind scan", "name", name, "err", err)
			continue
		}
		if k == kind {
			out = append(out, name)
		}
	}
	return out, nil
}

// parseKind returns ONLY the kind= of a credential file, validating the WHOLE file
// exactly as parseEnv does but without building the secret-value map. It MUST agree
// with parseEnv on accept/reject for every input (namesByKind classifies with this,
// broker.Build loads with parseEnv) — so it shares parseEnv's scanner with a nil
// sink rather than stopping early (an early stop would accept a file parseEnv later
// rejects on a line past kind=, mis-classifying a malformed file as a live
// candidate). See TestParseKindMatchesParseEnv.
func parseKind(b []byte) (string, error) {
	return scanEnv(b, nil)
}

func credNameRe(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return name != "." && name != ".."
}

// parseEnv parses .env-format bytes into kind + data (ported from skeleton — a
// zero-dep parser, no yaml). First non-comment line MUST be `kind=<...>`; values
// split on the FIRST `=` (DSNs / base64 keep their `=`); value is left-trimmed only
// (trailing base64/PEM bytes preserved).
func parseEnv(b []byte) (kind string, data map[string]string, err error) {
	data = map[string]string{}
	kind, err = scanEnv(b, func(key, val string) { data[key] = val })
	if err != nil {
		return "", nil, err
	}
	return kind, data, nil
}

// scanEnv is the single .env field scanner behind both parseEnv (sink fills the
// map) and parseKind (nil sink — validate + read kind only). Enforcing the shared
// contract in ONE place is what guarantees the two callers never disagree on a
// file's accept/reject or kind: first non-comment field must be `kind=<non-empty>`,
// every field is `key=value` with a non-empty key. sink, when non-nil, receives
// each (key, val) in file order.
func scanEnv(b []byte, sink func(key, val string)) (kind string, err error) {
	lineno := 0
	seenKind := false
	for _, raw := range bytes.Split(b, []byte("\n")) {
		lineno++
		line := strings.TrimRight(string(raw), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return "", fmt.Errorf("credentials .env line %d: missing '='", lineno)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return "", fmt.Errorf("credentials .env line %d: empty key", lineno)
		}
		val := strings.TrimLeft(line[eq+1:], " \t")
		if !seenKind {
			if key != "kind" {
				return "", fmt.Errorf("credentials .env line %d: first field must be kind=", lineno)
			}
			// Fully trim ONLY the kind discriminator (a trailing space/CR would otherwise
			// produce a kind no factory matches); sink/secret data values stay untrimmed so
			// base64/PEM trailing bytes survive.
			k := strings.TrimSpace(val)
			if k == "" {
				// Reject at parse, where the root cause is obvious — not later at
				// Build with a confusing "unsupported kind \"\"" (S-34).
				return "", fmt.Errorf("credentials .env line %d: kind= must not be empty", lineno)
			}
			kind, seenKind = k, true
			// The kind= line is the discriminator, not secret data: don't feed it to the
			// sink, or parseEnv's data map carries a stray data["kind"] (and a later
			// duplicate kind= line would overwrite it).
			continue
		}
		if sink != nil {
			sink(key, val)
		}
	}
	if !seenKind {
		return "", fmt.Errorf("credentials .env: no kind= field")
	}
	return kind, nil
}
