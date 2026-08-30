package tools

import (
	"context"
	"strings"
	"testing"
)

// TestDialChecked_BlocksPrivate: the egress-proxy dialer refuses blocked targets
// (loopback / RFC1918 / link-local / cloud metadata) before dialing. IP literals
// resolve to themselves (no DNS), so this is deterministic and network-free.
func TestDialChecked_BlocksPrivate(t *testing.T) {
	ctx := context.Background()
	blocked := []string{
		"127.0.0.1:80",       // loopback
		"10.1.2.3:443",       // RFC1918
		"192.168.0.5:80",     // RFC1918
		"169.254.169.254:80", // link-local / cloud metadata
		"172.16.5.5:80",      // RFC1918
		"100.64.0.1:80",      // CGNAT (extra-blocked)
	}
	for _, addr := range blocked {
		conn, err := dialChecked(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			t.Errorf("dialChecked(%q) should be blocked, got nil error", addr)
			continue
		}
		if !strings.Contains(err.Error(), "blocked egress") {
			t.Errorf("dialChecked(%q) err = %v, want a 'blocked egress' rejection", addr, err)
		}
	}
}
