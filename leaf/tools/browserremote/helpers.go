package browserremote

import (
	"context"
	"encoding/base64"
	"log/slog"
	"time"
)

// nowNano is the screenshot filename nonce source (extracted so tests can read
// the call site without importing time at every use). Monotonic enough — two
// screenshots in the same nanosecond is not a real concern.
func nowNano() int64 { return time.Now().UnixNano() }

// decodeB64 decodes a standard-base64 string (browserd's /vision PNG payload).
func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// PurgeSession tells browserd to wipe the scope's browser state. Ported from
// skeleton's core.SessionPurger impl, but UNWIRED in trunk: there is no /new //
// /close-session purger chain to hand this to yet. Kept best-effort so wiring it
// later is a one-line registration. Failures are logged, never fatal.
func (c *Client) PurgeSession(ctx context.Context, scope string) {
	if scope == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.post(cctx, "/purge", purgeReq{Scope: scope}, nil); err != nil {
		slog.Warn("browser remote: purge failed (browserd state may linger until its idle sweep)",
			"scope", scope, "err", err)
	}
}
