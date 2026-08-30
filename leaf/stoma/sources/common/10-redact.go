// Package common holds the cross-source helpers shared by bob's chat-platform
// adapters, so a feature implemented once isn't re-implemented per platform.
// It depends only on contract + shared infra (scrub) — never a platform SDK.
package common

import (
	"strings"

	"agentbob/heartwood/scrub"
)

// RedactErr scrubs secrets from an error before it reaches a log: it replaces
// each non-empty secret (the source's bot token / app secret) with "<redacted>"
// and runs the generic scrubber over the rest. A nil error, or one carrying no
// secret, is returned unchanged (type identity preserved, hot path cheap); only
// a changed message is re-wrapped. Every source's Wire.RedactErr delegates here.
func RedactErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	out := msg
	for _, sec := range secrets {
		if sec != "" && strings.Contains(out, sec) {
			out = strings.ReplaceAll(out, sec, "<redacted>")
		}
	}
	out = scrub.Scrub(out)
	if out == msg {
		return err // nothing scrubbed — keep the original error's type
	}
	return errString(out)
}

// errString is a minimal error wrapping a scrubbed message (type identity is
// not preserved — the source already classified the raw error before redacting).
type errString string

func (e errString) Error() string { return string(e) }
