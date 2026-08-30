package main

import (
	"os"
	"path/filepath"
	"time"
)

// version is bob's build version, injected at build time via
// -ldflags "-X main.version=<value>" (the Dockerfile passes the BOB_VERSION build
// arg — the deploy sets it from the host's git short SHA). "dev" when built without.
var version = "dev"

// recordVersion writes the running build's version + boot time to $home/VERSION so a
// deploy can confirm what's actually live (`cat $BOB_HOME/VERSION`), and returns the
// one-line summary for the startup log. Best-effort: a write failure is ignored.
func recordVersion(home string) string {
	line := version + " (booted " + time.Now().Format(time.RFC3339) + ")"
	_ = os.WriteFile(filepath.Join(home, "VERSION"), []byte(line+"\n"), 0o644)
	return line
}
