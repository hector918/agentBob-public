package warrant

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"agentbob/contract"

	"golang.org/x/crypto/ssh"
)

// spaceNameCharset is the ONE source of truth for the flattened-space-name character
// class. Both the fold (spaceFoldRe, the live traversal guard) and the post-fold
// validate (spaceNameRe, defense-in-depth) derive from it, so the two can never drift
// out of being exact inverses — a drift is exactly what would open a hole (a char the
// validate admits but the fold no longer removes, or vice versa).
const spaceNameCharset = `A-Za-z0-9_.-`

// spaceNameRe guards a space name before it is joined into a path: no separators, so a
// name can't escape $BOB_HOME/spaces. (':' etc. are folded to '_' by sanitizeSpace
// before this check, so they are intentionally not admitted here.) Today this is a
// DEFENSE-IN-DEPTH backstop — sanitizeSpace already folds every out-of-charset char, so
// the match can't fail on a folded name — but it re-arms automatically the moment the
// fold charset changes, since both share spaceNameCharset.
var spaceNameRe = regexp.MustCompile(`^[` + spaceNameCharset + `]+$`)

// File vends a file channel to a space. Local → a rooted, escape-confined fs
// channel (no credential). Remote (ssh) → DEFERRED (sftp dep not vendored).
func (m *Manager) File(_ context.Context, identity, space string) (contract.FileChannel, error) {
	if m.spacesBroken {
		return nil, fmt.Errorf("warrant: spaces.yaml 解析失败，space 路由已停用——请管理员修复文件并重启")
	}
	// Remote spaces are TERMINAL-ONLY by design (拍板): the shell is
	// the whole file surface there; fs never speaks ssh/sftp.
	if m.spaces.backend(space).Backend == "ssh" {
		return nil, fmt.Errorf("warrant: 远程 space %q 仅支持 terminal——文件操作请在 shell 里完成", space)
	}
	root, err := m.spaceDir(space)
	if err != nil {
		return nil, err
	}
	return newLocalFileChannel(space, root)
}

// Exec vends a shell session for a space (POOLED — reused across calls). Local →
// one-shot bash in the space dir. Remote (ssh) → the credential:use:<cred> gate
// (level-2), then a pooled ssh connection. file + exec on a local space share the
// dir (write-then-run).
func (m *Manager) Exec(ctx context.Context, identity, space string) (contract.ExecChannel, error) {
	if m.spacesBroken {
		return nil, fmt.Errorf("warrant: spaces.yaml 解析失败，space 路由已停用——请管理员修复文件并重启")
	}
	be := m.spaces.backend(space)
	if be.Backend == "ssh" {
		return m.sshExec(ctx, identity, space, be)
	}
	dir, err := m.spaceDir(space)
	if err != nil {
		return nil, err
	}
	// exec-home is a PRIVATE HOME/TMPDIR sibling of spaces/ — keeps a program's
	// dotdirs/temp out of the user-visible space dir.
	execHome := filepath.Join(m.home, "exec-home", sanitizeSpace(space))
	return m.acquireExec(identity, space, func() (contract.Channel, error) {
		return newLocalExecChannel(space, dir, execHome)
	})
}

// sshExec gates the space's credential then vends a pooled ssh exec channel.
func (m *Manager) sshExec(ctx context.Context, identity, space string, be spaceBackend) (contract.ExecChannel, error) {
	if be.Credential == "" {
		return nil, fmt.Errorf("warrant: ssh space %q has no credential configured", space)
	}
	if ok, reason := m.Check(ctx, identity, "credential", be.Credential); !ok {
		return nil, fmt.Errorf("warrant: %s", reason)
	}
	if m.broker == nil {
		return nil, fmt.Errorf("warrant: no credential broker — remote spaces unavailable")
	}
	return m.acquireExec(identity, space, func() (contract.Channel, error) {
		cl, err := m.broker.Build(ctx, be.Credential)
		if err != nil {
			return nil, fmt.Errorf("dial %q: %w", be.Credential, err)
		}
		sshc, ok := cl.(*ssh.Client)
		if !ok {
			return nil, fmt.Errorf("credential %q is not an ssh credential", be.Credential)
		}
		return newSSHExecChannel(sshc), nil
	})
}

// acquireExec gets a pooled exec channel (or a fresh one when no pool is wired).
func (m *Manager) acquireExec(identity, space string, build func() (contract.Channel, error)) (contract.ExecChannel, error) {
	if m.pool == nil {
		ch, err := build()
		if err != nil {
			return nil, err
		}
		ec, ok := ch.(contract.ExecChannel)
		if !ok {
			return nil, fmt.Errorf("warrant: built channel is not an exec channel")
		}
		return ec, nil
	}
	ch, err := m.pool.Acquire(contract.ChannelKey{Identity: strings.TrimSpace(identity), Space: strings.TrimSpace(space), Kind: "exec"}, build)
	if err != nil {
		return nil, err
	}
	ec, ok := ch.(contract.ExecChannel)
	if !ok {
		return nil, fmt.Errorf("warrant: pooled channel is not an exec channel")
	}
	return ec, nil
}

// Cut revokes a principal's live channels (a permission change) — the pool holds
// them, so it does the closing.
func (m *Manager) Cut(identity string) {
	if m.pool != nil {
		// Trim to match the key Acquire pooled under, or a stray space makes the
		// revoke miss and a permission change leaves live channels open.
		m.pool.Recycle(strings.TrimSpace(identity))
	}
}

// spaceDir resolves a space name to its local root under $BOB_HOME. It folds
// separators FIRST, then validates the flattened form — so a structured scope name
// (e.g. "member:alice/inbox:1") collapses to one safe path component instead of
// being rejected, and a '/' can never survive into filepath.Join as a real
// separator (the fold is itself the traversal guard).
func (m *Manager) spaceDir(space string) (string, error) {
	return SpaceLocalDir(m.home, space)
}

// SpaceLocalDir resolves a space name to its local root under home/spaces, applying
// the SAME fold+validate as Manager.spaceDir so a target computed outside a started
// Manager (the inbound attachment-placement step, wired in cmd/bob) and the
// FileChannel that later serves that space agree byte-for-byte. Exported because
// placement runs at session submit, before warrant's Manager is reachable.
func SpaceLocalDir(home, space string) (string, error) {
	space = strings.TrimSpace(space)
	if space == "" {
		return "", fmt.Errorf("warrant: empty space")
	}
	clean := sanitizeSpace(space)
	if !spaceNameRe.MatchString(clean) {
		return "", fmt.Errorf("warrant: invalid space name %q", space)
	}
	// spaceNameRe admits "." — reject pure-dot names ("."/".."/"...") so a folded
	// name can't traverse out of spaces/ (".." → $BOB_HOME, exposing
	// credentials/permissions). Any name with a non-dot char survives unchanged.
	if strings.Trim(clean, ".") == "" {
		return "", fmt.Errorf("warrant: invalid space name %q", space)
	}
	return filepath.Join(home, "spaces", clean), nil
}

// sanitizeSpace flattens a space name to ONE safe path component: EVERY character
// outside the space-name charset folds to "_". This covers all scope separators ("/"
// member/inbox, ":" platform:group:id, "#" agora member sub-scope) plus anything else a
// chat-id / member-handle / company-name might carry, so a structured scope is always a
// valid space dir, never rejected. Run BEFORE the charset check (spaceDir). Folding is
// inherently lossy (":" and "/" both → "_"), which the codebase already accepts.
func sanitizeSpace(s string) string {
	return spaceFoldRe.ReplaceAllString(s, "_")
}

// spaceFoldRe matches any char NOT allowed in a flattened space name (the inverse of
// spaceNameRe's class, both derived from spaceNameCharset) so sanitizeSpace folds them
// all to "_". This is the live path-traversal guard — a '/' can never survive into
// filepath.Join.
var spaceFoldRe = regexp.MustCompile(`[^` + spaceNameCharset + `]`)
