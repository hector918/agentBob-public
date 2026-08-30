package feishu

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// 25-sendgate.go — feishu's 429 classifiers for the per-bot outbound send gate
// (L-FEISHU-D1; the gate itself is the shared sources/sendgate.Gate held on
// Source.gate). EVERY REST send — Message.Create/Reply, reactions, the File.Create
// upload — routes through gate.Do, so all sends serialise and share one 429
// cooldown floor + bounded relay across every sink.

const (
	// feishuDefaultCooldown is the 429 backoff when feishu sends no parseable
	// Retry-After; feishuMaxCooldown clamps it so one throttled send can't park the
	// serialiser indefinitely.
	feishuDefaultCooldown = 1 * time.Second
	feishuMaxCooldown     = 15 * time.Second
)

// feishuRateLimit is the typed error a send returns on an HTTP 429, carrying the
// server's retry-after. Both the send gate (shared cooldown/relay) and feishuWire.RateLimited
// (the streamsink core's retry) classify it through feishuRateLimited.
type feishuRateLimit struct{ retryAfter time.Duration }

func (e *feishuRateLimit) Error() string {
	return fmt.Sprintf("feishu: rate limited (429), retry after %s", e.retryAfter)
}

// feishuRateLimited classifies an error as a feishu 429 and returns its retry-after.
func feishuRateLimited(err error) (time.Duration, bool) {
	var rl *feishuRateLimit
	if errors.As(err, &rl) {
		return rl.retryAfter, true
	}
	return 0, false
}

// feishuRespRateLimited reports whether a REST response is an HTTP 429 and the
// retry-after to honour (the Retry-After header in seconds, else feishuDefaultCooldown,
// clamped to feishuMaxCooldown). A nil ApiResp (transport error → no HTTP status) is
// not a rate-limit.
func feishuRespRateLimited(r *larkcore.ApiResp) (time.Duration, bool) {
	if r == nil || r.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	d := feishuDefaultCooldown
	if ra := strings.TrimSpace(r.Header.Get("Retry-After")); ra != "" {
		if secs, e := strconv.Atoi(ra); e == nil && secs > 0 {
			d = time.Duration(secs) * time.Second
		}
	}
	if d > feishuMaxCooldown {
		d = feishuMaxCooldown
	}
	return d, true
}
