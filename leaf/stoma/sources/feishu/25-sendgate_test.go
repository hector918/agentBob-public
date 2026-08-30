package feishu

import (
	"net/http"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// Gate semantics (serialisation / floor / relay / ctx) are owned by the shared
// sources/sendgate package tests; this file keeps only feishu's own classifier.

// TestFeishuRespRateLimited pins the HTTP-429 detection: a 429 status is a rate-limit
// with the Retry-After header (seconds) as its cooldown, defaulted + clamped; any other
// status (or a nil resp from a transport error) is not.
func TestFeishuRespRateLimited(t *testing.T) {
	resp := func(status int, retryAfter string) *larkcore.ApiResp {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &larkcore.ApiResp{StatusCode: status, Header: h}
	}
	cases := []struct {
		name   string
		r      *larkcore.ApiResp
		wantD  time.Duration
		wantOK bool
	}{
		{"429 with retry-after", resp(429, "3"), 3 * time.Second, true},
		{"429 no header → default", resp(429, ""), feishuDefaultCooldown, true},
		{"429 huge → clamped", resp(429, "9999"), feishuMaxCooldown, true},
		{"200 → not limited", resp(200, "3"), 0, false},
		{"nil resp → not limited", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, ok := feishuRespRateLimited(c.r)
			if ok != c.wantOK || d != c.wantD {
				t.Fatalf("got (%v, %v), want (%v, %v)", d, ok, c.wantD, c.wantOK)
			}
		})
	}
}
