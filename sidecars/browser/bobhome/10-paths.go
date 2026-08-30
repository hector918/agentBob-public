// Package bobhome resolves the agentbob home directory ($BOB_HOME, default ~/.bob).
// Everything that reads/writes runtime state must go through here — never hardcode ~/.bob.
package bobhome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// homeOverride is set by SetHome (typically from the `bob --home <path>` flag)
// and takes precedence over $BOB_HOME and ~/.bob. Must be set BEFORE Init().
var homeOverride string

// StandardDirs is the canonical list of subdirectories under $BOB_HOME
// that EnsureDirs creates at startup. Every dir bob may write to should
// be listed here so that operators get a complete tree at boot rather
// than a tree that depends on which subsystem ran first. Entries are
// relative paths; "" (empty) means $BOB_HOME itself.
// NOTE: "agora" is deliberately ABSENT (review #4 [21]):
// $BOB_HOME/agora existing is the agora OPT-IN signal — openAgoraStore
// gates the whole agora schema bootstrap (incl. wipe-on-bump) on it, so
// creating it here unconditionally had silently opted every deployment
// in. Operators opt in by mkdir-ing it (and dropping permissions.yaml).
var StandardDirs = []string{
	"",
	"logs",
	"skills",
	"memory",
	"sandbox",
	"commands",
	"cron",
	"sqlite-store",
	"i18n",
	"sources",
	"credentials",
}

// SetHome forces Home() to return path, regardless of $BOB_HOME or default.
// Intended for the `bob --home <path>` CLI flag — convenient when a host
// runs multiple bots out of different home directories without juggling env
// vars in every shell. Setting it to "" clears the override. Must be called
// before Init().
func SetHome(path string) {
	homeOverride = path
}

// Init resolves the home directory once and caches it. main is expected to
// call this AFTER --home flag parsing but BEFORE any code reads Home() /
// Path(). Returns an error when the home can't be resolved (no --home, no
// $BOB_HOME, and $HOME unset) — the bot is a daemon, not a per-user app,
// so a missing home should fail loudly at startup rather than silently
// degrade to a cwd-relative path (R4Y-2).
//
// Idempotent: subsequent calls are no-ops (the first call's result wins).
func Init() error {
	initOnce.Do(func() {
		cachedHome, homeErr = resolveHome()
		// Export the resolved home as $BOB_HOME so every os.ExpandEnv consumer
		// agrees on it — most importantly tools/policy, whose defaultBannedPaths
		// (logs/memory/sqlite-store/permissions/...) are written as "$BOB_HOME/..."
		// and expanded at check time. With --home or the default ~/.bob, the env
		// var would otherwise be unset and "$BOB_HOME/logs" would expand to
		// "/logs", silently disabling those denies. Only set on success.
		if homeErr == nil && cachedHome != "" {
			_ = os.Setenv("BOB_HOME", cachedHome)
		}
	})
	return homeErr
}

var (
	cachedHome string
	homeErr    error
	initOnce   sync.Once
)

func resolveHome() (string, error) {
	if homeOverride != "" {
		return homeOverride, nil
	}
	if v := os.Getenv("BOB_HOME"); v != "" {
		return v, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("bobhome: cannot resolve home directory — set $BOB_HOME (canonical for docker / systemd deploys) or pass --home <path>: %w", err)
	}
	return filepath.Join(h, ".bob"), nil
}

// Home returns the absolute path to the agentbob home directory.
//
// Panics if Init() has not been called — this is a programmer error in
// any startup path that touches home before main's PersistentPreRunE
// resolves it. Was previously silently returning a cwd-relative ".bob"
// on UserHomeDir failure, which moved bot state between cwds invisibly
// (R4Y-2 fix).
func Home() string {
	if err := Init(); err != nil {
		panic(err)
	}
	return cachedHome
}

// Path joins parts onto the home directory.
func Path(parts ...string) string {
	return filepath.Join(append([]string{Home()}, parts...)...)
}

// Display returns a ~-collapsed form of the home directory for user-facing messages.
func Display() string {
	h := Home()
	if ud, err := os.UserHomeDir(); err == nil {
		return displayCollapse(h, ud)
	}
	return h
}

// displayCollapse collapses the user-home prefix ud in h to "~", but only
// on a real path boundary: h must equal ud or sit directly under ud. A bare
// strings.HasPrefix would mis-fold sibling dirs that share a string prefix
// (ud="/home/bob", h="/home/bobby" → wrong "~by").
func displayCollapse(h, ud string) string {
	if h == ud || strings.HasPrefix(h, ud+string(os.PathSeparator)) {
		return "~" + strings.TrimPrefix(h, ud)
	}
	return h
}

// restrictedDirs are standard subdirs that must be owner-only (0o700) rather
// than the default 0o755 — they hold per-uid secrets the credentials/sandbox
// subsystems explicitly create at 0o700 (credentials/40-bootstrap.go,
// credentials/30-yaml.go). MkdirAll does NOT tighten an existing dir's mode, so
// if EnsureDirs (which runs first at startup) created them at 0o755 the 0o700
// intent would be permanently defeated — leaving the credential dir listable by
// other uids on the same host. Chmod after MkdirAll closes that gap.
var restrictedDirs = map[string]os.FileMode{
	"credentials": 0o700,
	"sandbox":     0o700,
}

// EnsureDirs creates the standard subdirectories under the home dir.
func EnsureDirs() error {
	for _, d := range StandardDirs {
		p := Path(d)
		mode := os.FileMode(0o755)
		if m, ok := restrictedDirs[d]; ok {
			mode = m
		}
		if err := os.MkdirAll(p, mode); err != nil {
			return err
		}
		// MkdirAll leaves an already-existing dir's mode untouched; force the
		// restricted mode so a prior 0o755 creation is corrected in place.
		if m, ok := restrictedDirs[d]; ok {
			if err := os.Chmod(p, m); err != nil {
				return err
			}
		}
	}
	return nil
}
