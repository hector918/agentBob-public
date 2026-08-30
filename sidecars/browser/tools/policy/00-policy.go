// Package policy is the path / command policy gate for sensitive tools
// (audit D-residual). Three categories:
//
//   - Banned   — auto-refuse, no approval prompt. The model sees an error
//     immediately and can pick a different approach. Used for
//     paths bob must never touch (/etc, /root, ~/.ssh, ...) and
//     commands with no safe form (sudo, su).
//   - Approval — needs admin /approve before running. Used for sensitive
//     user data trees (~/Documents, ~/Projects) and risky
//     command patterns (rm -rf, chmod, pipe-to-shell).
//   - Allow    — implicit default; just runs.
//
// The split solves "if not allowed, don't even ask" — banned paths get a
// fast structured refusal instead of pinning a turn on an approval that
// will never arrive.
//
// Used by terminal / execute_code / patch (write side) and by read_file /
// search_files (read side, so banned dirs aren't even readable). Browser
// and web tools don't go through here yet; their URL boundary is the DNS
// breaker + the URL library's allow-list.
package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentbob/sidecars/browser/config"
)

// Decision is what CheckPath / CheckCommand return.
type Decision int

const (
	DecisionAllow Decision = iota
	DecisionApproval
	DecisionRefuse
)

// Result carries the decision plus a human-readable reason (for the
// refuse error message or the approval prompt). What matched, in plain
// English — never the regex / glob — so the model sees a clean signal.
type Result struct {
	Decision Decision
	Reason   string
}

// Policy is the compiled, ready-to-use rule set. Build with NewFromConfig.
type Policy struct {
	bannedPaths      []string
	approvalPaths    []string
	bannedCommands   []*regexp.Regexp
	approvalCommands []*regexp.Regexp
}

// NewFromConfig builds a Policy from the merged policy-config defaults
// plus any operator overrides. Defaults are intentionally conservative —
// every dir that ssh / cloud / system tooling stores credentials in lands
// under banned; the user-data trees commonly mounted into a container go
// under approval.
func NewFromConfig(cfg config.PolicyConfig) *Policy {
	p := &Policy{}
	for _, raw := range mergeWithDefaults(cfg.BannedPaths, defaultBannedPaths) {
		if cleaned := normalizePath(raw); cleaned != "" {
			p.bannedPaths = append(p.bannedPaths, cleaned)
		}
	}
	for _, raw := range mergeWithDefaults(cfg.ApprovalPaths, defaultApprovalPaths) {
		if cleaned := normalizePath(raw); cleaned != "" {
			p.approvalPaths = append(p.approvalPaths, cleaned)
		}
	}
	for _, raw := range mergeWithDefaults(cfg.BannedCommands, defaultBannedCommands) {
		if re := compileCmdPattern(raw); re != nil {
			p.bannedCommands = append(p.bannedCommands, re)
		}
	}
	for _, raw := range mergeWithDefaults(cfg.ApprovalCommands, defaultApprovalCommands) {
		if re := compileCmdPattern(raw); re != nil {
			p.approvalCommands = append(p.approvalCommands, re)
		}
	}
	return p
}

// CheckPath evaluates a filesystem path. Banned beats approval — an
// approval-prefixed path that's also banned (e.g. ~/Documents/.ssh on a
// macOS dev box) refuses outright.
//
// S4 fix: the path is resolved through filepath.EvalSymlinks
// BEFORE comparison. Without that, a symlink in an allowed dir pointing
// at a banned root (e.g. `~/notes/secrets → /etc/ssl/private`) defeated
// the banned-path check entirely. We compare BOTH the lexical-normalised
// path and the symlink-resolved path against bans so an exact match on
// either fails. EvalSymlinks returns the lexical path unchanged when no
// symlink is present (or when the path doesn't exist yet — in which case
// the symlink-walk falls back to the lexical form).
func (p *Policy) CheckPath(path string) Result {
	norm := normalizePath(path)
	if norm == "" {
		return Result{Decision: DecisionAllow}
	}
	// Compute the real path through any symlinks. If the path doesn't
	// exist, EvalSymlinks errors — that's fine; we fall back to the
	// lexical form (a non-existent path can't be following a symlink
	// to a banned root). Real != norm means a symlink was followed;
	// we check BOTH forms below.
	real, err := filepath.EvalSymlinks(norm)
	if err != nil {
		real = norm
	}
	// v1 leak hardening — substring-based credential segment defence.
	// Catches `/foo/../$BOB_HOME/credentials/x` style strings that survive
	// normalisation, plus admin envs where $BOB_HOME isn't where the
	// default rules expect. Runs BEFORE the configured prefix rules so
	// concat-style bypass loses even when neither norm nor real lives
	// under a banned root prefix the operator happens to have configured.
	//
	// Hand-traced cases:
	//   norm="/data/.bob/credentials/small01.env"
	//     → contains "/credentials/"            → refuse
	//   norm="/data/.bob/.env"
	//     → contains "/.bob/.env"               → refuse
	//   norm="/data/work/credentials/notes.md"  (user dir literally "credentials")
	//     → contains "/credentials/"            → refuse (acceptable false positive;
	//                                              user should rename their dir)
	//   norm="/home/user/cred-utils/script.py"
	//     → no banned segment                   → fall through to prefix rules
	for _, seg := range bannedPathSegments {
		if strings.Contains(norm, seg) || strings.Contains(real, seg) {
			return Result{Decision: DecisionRefuse, Reason: "path " + path + " contains banned segment " + seg}
		}
	}
	for _, banned := range p.bannedPaths {
		if pathUnder(norm, banned) || pathUnder(real, banned) {
			return Result{Decision: DecisionRefuse, Reason: "path " + path + " is under banned root " + banned}
		}
	}
	for _, ap := range p.approvalPaths {
		if pathUnder(norm, ap) || pathUnder(real, ap) {
			return Result{Decision: DecisionApproval, Reason: "path " + path + " is under approval-required root " + ap}
		}
	}
	return Result{Decision: DecisionAllow}
}

// CheckCommand evaluates a shell command line (or python source). Same
// banned-beats-approval rule.
//
// Two-layer check (T12 fix):
//  1. Regex match against banned_commands / approval_commands patterns.
//  2. Path-argument cross-check: scan the command text for tokens
//     that look like absolute paths (start with `/`) and check each
//     against banned_paths. The previous version skipped this so
//     `cat /etc/passwd` slipped through (acknowledged BUG #9).
//
// The path-arg check is intentionally conservative: only banned (not
// approval) — a `echo "/etc/banner"` triggering approval would be a
// significant false-positive rate. For real path access via approval-
// gated roots, callers should reach the path through the FILE tools
// (read_file / patch) which go through CheckPath proper.
func (p *Policy) CheckCommand(text string) Result {
	for _, re := range p.bannedCommands {
		if loc := re.FindStringIndex(text); loc != nil {
			return Result{Decision: DecisionRefuse, Reason: "command matched banned pattern " + re.String()}
		}
	}
	// Path-argument cross-check (T12). Tokenise on whitespace and look at
	// any token that starts with `/` or `~`. Match against banned
	// prefixes via the same EvalSymlinks-aware logic as CheckPath. We
	// stop at the first hit.
	for _, tok := range tokenisePathArgs(text) {
		if r := p.CheckPath(tok); r.Decision == DecisionRefuse {
			return Result{Decision: DecisionRefuse, Reason: "command argument " + tok + " is under banned root"}
		}
	}
	for _, re := range p.approvalCommands {
		if loc := re.FindStringIndex(text); loc != nil {
			return Result{Decision: DecisionApproval, Reason: "command matched approval pattern " + re.String()}
		}
	}
	return Result{Decision: DecisionAllow}
}

// embeddedPathRe — R5A-2 second-pass regex catches paths that don't
// begin a token. `cat </etc/passwd` and `cat </etc"/passwd"` both fail
// strings.Fields-based tokenising because the `/etc/passwd` chunk is
// glued to a redirection or quote char. Look for any `/...` that
// follows shell-significant punctuation (whitespace, `=`, `'`, `"`,
// `<`, `>`, `(`, `|`). The captured run greedily extends through path-
// legal chars.
//
// Limitations (documented per R5A-2 contract): shell metaprogramming
// like `$(printf /etc/passwd)`, backtick command substitution, and
// embedded `\x2f` escapes are NOT detected. Those require a real shell
// parser; we accept the gap as out-of-scope for v1 policy and rely on
// the BANNED-commands regex layer to catch the obvious wrappers
// (`bash -c`, `eval`, etc).
var embeddedPathRe = regexp.MustCompile(`[\s='"<>(|]/[A-Za-z0-9._/~-]+`)

// pythonInlineCDQ / pythonInlineCSQ match `python -c "..."` / `python -c '...'`
// argument groups. Two separate regexes because Go's RE2 has no
// backreferences (R5A-2 / R5A-2-followup). We feed each captured body
// to inlinePathRe to find path-shaped substrings. Doesn't try to parse
// python (the model could use `__import__("os").open("/etc/passwd")`
// and we'd miss it) — but `python3 -c "open('/etc/passwd')"` and the
// single-quoted variant are the common bypass and this catches them.
var pythonInlineCDQ = regexp.MustCompile(`(?:python|python3)\s+(?:[^|;&"]+\s+)?-c\s+"([^"]*)"`)
var pythonInlineCSQ = regexp.MustCompile(`(?:python|python3)\s+(?:[^|;&']+\s+)?-c\s+'([^']*)'`)

// inlinePathDQ / inlinePathSQ — path-shaped substring inside double- /
// single-quoted strings (RE2 backref-free split, same reason as above).
// Same conservative char class as embeddedPathRe.
var inlinePathDQ = regexp.MustCompile(`"([^"]*[/~][^"]*)"`)
var inlinePathSQ = regexp.MustCompile(`'([^']*[/~][^']*)'`)

// tokenisePathArgs extracts whitespace-separated tokens that look like
// absolute paths from a shell command line. Strips surrounding quotes,
// rejects URL-style `http://`. Not a shell parser — it WILL catch
// path-like substrings inside quoted strings (which would be a false
// positive for `echo "/etc/banner"`) but T12's contract accepts that
// trade-off in exchange for catching `cat /etc/passwd` and similar.
//
//: env-var prefix support (`$BOB_HOME/.env`, `$HOME/.ssh/...`).
// Without this, `cat $BOB_HOME/.env` slipped through because the literal
// `$BOB_HOME/...` doesn't start with `/` or `~`. We now ExpandEnv the
// token first so subsequent CheckPath sees the resolved absolute path.
// Inside the bob container the env is fixed at process start so
// expansion is deterministic; an attacker who can mutate the env has
// already won bigger battles than path-policy bypass.
//
// R5A-2: added a second-pass regex scan for `embeddedPathRe`
// (paths glued to redirections / quotes like `cat </etc/passwd`) plus a
// python-c inline-string scan via `pythonInlineCRe` + `inlinePathRe`.
// Both passes feed back through the same banned-path check.
func tokenisePathArgs(text string) []string {
	var out []string
	for _, raw := range strings.Fields(text) {
		// Strip matched leading/trailing quotes (handles "..." / '...').
		tok := strings.TrimFunc(raw, func(r rune) bool { return r == '"' || r == '\'' })
		if tok == "" {
			continue
		}
		// Skip URLs (http://, https://, file://) — they aren't FS paths
		// in the CheckPath sense.
		if strings.Contains(tok, "://") {
			continue
		}
		// Resolve env-var prefix BEFORE the leading-char check so
		// `$BOB_HOME/.env` → `/data/.bob/.env` gets caught.
		if strings.ContainsRune(tok, '$') {
			tok = os.ExpandEnv(tok)
		}
		if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") {
			out = append(out, tok)
		}
	}
	// R5A-2 pass 2: regex sweep for paths glued to shell punctuation.
	for _, m := range embeddedPathRe.FindAllString(text, -1) {
		// m starts with the punct char; drop it.
		if len(m) > 1 {
			tok := m[1:]
			tok = strings.TrimFunc(tok, func(r rune) bool { return r == '"' || r == '\'' })
			if tok == "" {
				continue
			}
			out = append(out, tok)
		}
	}
	// R5A-2 pass 3: python -c "...path..." inline scan. Match each
	// python invocation in both quote styles, then scan the inline-c
	// body for path-shaped quoted substrings (also both quote styles
	// since the bypass may use either e.g. `python3 -c "open('/etc/passwd')"`).
	for _, body := range collectPyBodies(text) {
		for _, scanner := range []*regexp.Regexp{inlinePathDQ, inlinePathSQ} {
			for _, inner := range scanner.FindAllStringSubmatch(body, -1) {
				if len(inner) < 2 {
					continue
				}
				cand := inner[1]
				if strings.ContainsRune(cand, '$') {
					cand = os.ExpandEnv(cand)
				}
				if strings.HasPrefix(cand, "/") || strings.HasPrefix(cand, "~") {
					out = append(out, cand)
				}
			}
		}
	}
	return out
}

// collectPyBodies returns the bodies of every `python -c "..."` /
// `python -c '...'` invocation in text. Helper for tokenisePathArgs.
func collectPyBodies(text string) []string {
	var bodies []string
	for _, m := range pythonInlineCDQ.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 {
			bodies = append(bodies, m[1])
		}
	}
	for _, m := range pythonInlineCSQ.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 {
			bodies = append(bodies, m[1])
		}
	}
	return bodies
}

// pathUnder reports whether `target` is exactly `root` or a descendant.
// Both inputs must be cleaned absolute paths (callers go through
// normalizePath first).
func pathUnder(target, root string) bool {
	if target == root {
		return true
	}
	if !strings.HasSuffix(root, string(os.PathSeparator)) {
		root += string(os.PathSeparator)
	}
	return strings.HasPrefix(target, root)
}

// normalizePath resolves ~ / env vars, cleans `..`, and returns an absolute
// path. Empty result on unresolvable input — the caller skips silently
// (over-cautious config never causes a tool to crash).
func normalizePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = os.ExpandEnv(raw)
	if strings.HasPrefix(raw, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			raw = strings.Replace(raw, "~", home, 1)
		}
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

// compileCmdPattern turns the YAML's human-friendly pattern (with *) into
// a regex. The * meta-character matches any chars (greedy) inside one line;
// the two-char sequence \b passes through as a regex word-boundary anchor
// (defaultBannedCommands depends on it — see the sudo/su rationale there);
// everything else is escaped. A trailing space in the pattern forces a
// word boundary (so "rm " matches the command `rm` but not `rmdir`).
//
// Only LEADING whitespace is trimmed (a YAML-indentation artifact); the
// trailing space is load-bearing per the word-boundary contract above and
// must be preserved — escaped literally so the pattern matches an actual
// space after the command word.
func compileCmdPattern(pat string) *regexp.Regexp {
	pat = strings.TrimLeft(pat, " \t")
	if strings.TrimSpace(pat) == "" {
		return nil
	}
	var b strings.Builder
	runes := []rune(pat)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '*':
			b.WriteString(".*?")
		case runes[i] == '\\' && i+1 < len(runes) && runes[i+1] == 'b':
			b.WriteString(`\b`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		}
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// mergeWithDefaults unions defaults with operator additions. Caller
// passes an empty (but non-nil) list to mean "use defaults only", and
// passes nil-by-omission to also mean "use defaults". To strip a
// default (e.g. allow access to /etc) the operator must add it to the
// explicit allow-list, not by sneaking it out of the default banned
// list.
//
// S7 fix: the prior semantics was "replace defaults with
// operator list". That meant `banned_commands: ["rm -rf /home/me"]`
// silently dropped `sudo`, `su`, etc — a catastrophic foot-gun for any
// admin adding one entry. Union is safer: admin can ADD bans but
// cannot accidentally lose the defaults. A truly motivated admin who
// wants to drop a default can comment-out a line in defaultBanned*
// arrays in source, or use a future cfg.PolicyDisableDefaults knob
// (not in v1).
func mergeWithDefaults(operator []string, defaults []string) []string {
	if len(operator) == 0 {
		return defaults
	}
	// Union: defaults first (so they show up first in policy listings),
	// then operator additions, dedupe via map.
	seen := make(map[string]bool, len(defaults)+len(operator))
	out := make([]string, 0, len(defaults)+len(operator))
	for _, d := range defaults {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, o := range operator {
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

// bannedPathSegments is the credential-territory denylist applied as a
// post-filepath.Clean substring scan in CheckPath. Substring (not just
// prefix) so a concat-built or `..`-laundered path that still resolves
// into credentials/ gets caught even when $BOB_HOME isn't the
// deployment's default. Conservative — only segments where any read is
// unambiguously a credential exfil attempt. Adding entries tightens
// security; removing them is a regression.
var bannedPathSegments = []string{
	"/credentials/",
	"/.bob/credentials/",
	"/.bob/.env",
}

// defaultBannedPaths is the conservative starting set. Tweakable per
// install via ExecutionConfig.BannedPaths.
//
// $BOB_HOME subtrees added (TD2 follow-up): the model can
// otherwise `cat $BOB_HOME/.env` via the terminal tool and dump every
// API key / DSN / bot token in one read. See docs/security-design.md
// §"Container hardening" for the full attack surface (chromium routes
// are closed separately in browser_navigate / browser_cdp).
var defaultBannedPaths = []string{
	"/etc",
	"/var",
	"/proc",
	"/sys",
	"/root",
	"~/.ssh",
	"~/.gnupg",
	"~/.aws",
	"~/.docker",
	"~/.kube",
	"~/.config/gh",
	// Credential containers — must not be read by tools.
	"$BOB_HOME/.env",
	"$BOB_HOME/credentials",
	// Operational state with sensitive content (chat history, model
	// API call payloads, conversation summaries).
	"$BOB_HOME/logs",
	"$BOB_HOME/memory",
	"$BOB_HOME/sqlite-store",
	// Permissions config (templates may carry sensitive role names;
	// approvals table has audit content). Future: bob_home/permissions/
	// when broker-style identity yamls land.
	"$BOB_HOME/permissions",
}

// defaultApprovalPaths covers user-data trees commonly mounted into the
// container under /work or kept on the host home. Adjust per deployment.
var defaultApprovalPaths = []string{
	"~/Documents",
	"~/Projects",
	"/workspace",
}

// defaultBannedCommands — no safe form. `sudo` / `su` always need a
// human; in a container with no sudo binary they'd fail anyway, but
// catching upstream gives a cleaner error.
//
// Word-boundary regex: previous literal "su " / "su\t"
// substring-matched ANY occurrence of "su" + whitespace in the command
// text, false-positiving on python scripts containing things like
// `import subprocess as sp ` or `print("status: ...")`. The \b anchors
// match real command-word boundaries — "subprocess" / "BeautifulSoup"
// / "issue" / "pseudonym" all safely pass; literal `su` / `sudo`
// invocations still trip.
var defaultBannedCommands = []string{
	`\bsudo\b`,
	`\bsu\b`,
}

// defaultApprovalCommands — destructive or system-altering patterns that
// have legitimate uses but warrant a confirmation.
var defaultApprovalCommands = []string{
	"rm -rf",
	"rm -fr",
	"rm -r ",
	"chmod ",
	"chown ",
	"curl * | sh",
	"curl * | bash",
	"wget * | sh",
	"wget * | bash",
	"dd ",
	"mkfs",
}

// DefaultCounts reports the lengths of the built-in policy lists so
// callers (e.g. the doctor summary line) can show "default" entry counts
// without hardcoding numbers that drift from the lists above.
type DefaultCounts struct {
	BannedPaths      int
	ApprovalPaths    int
	BannedCommands   int
	ApprovalCommands int
}

// Defaults returns the entry counts of the built-in policy lists.
func Defaults() DefaultCounts {
	return DefaultCounts{
		BannedPaths:      len(defaultBannedPaths),
		ApprovalPaths:    len(defaultApprovalPaths),
		BannedCommands:   len(defaultBannedCommands),
		ApprovalCommands: len(defaultApprovalCommands),
	}
}
