package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestScrapeRunError_ToolSide pins the one judgement the engine breaker depends on:
// which subprocess failures are OUR side (the browser never came up) versus the
// target's (timeout, DNS, or an exit we can't attribute). Getting this wrong in either
// direction is silent — too broad and a genuinely dead engine never cools down, too
// narrow and a broken install cools every engine down for 10 minutes.
func TestScrapeRunError_ToolSide(t *testing.T) {
	for _, tc := range []struct {
		name, mode, out  string
		runErr           error
		wantToolSide     bool
		wantHintContains string
	}{
		{
			name: "missing chromium is ours", mode: "fetch",
			out:    "playwright._impl._errors.Error: BrowserType.launch: Executable doesn't exist at /opt/ms-playwright/chromium-1228/chrome",
			runErr: errors.New("exit status 1"), wantToolSide: true,
			wantHintContains: "playwright chromium",
		},
		{
			name: "launch failure is ours", mode: "stealthy-fetch",
			out:    "patchright._impl._errors.Error: BrowserType.launch_persistent_context: something went wrong",
			runErr: errors.New("exit status 1"), wantToolSide: true,
		},
		{
			// A page-level playwright error is NOT evidence about our install.
			name: "in-page playwright error is not ours", mode: "fetch",
			out:    "playwright._impl._api_types.Error: Page.goto: net::ERR_CONNECTION_RESET",
			runErr: errors.New("exit status 1"), wantToolSide: false,
			wantHintContains: "playwright chromium",
		},
		{
			name: "bare crash is not attributable", mode: "fetch",
			out:    "Traceback (most recent call last):\n  File \"/usr/local/bin/scrapling\", line 8, in <module>\nValueError: boom",
			runErr: errors.New("exit status 1"), wantToolSide: false,
		},
		{
			name: "timeout belongs to the target", mode: "stealthy-fetch",
			out: "", runErr: errors.New("signal: killed"), wantToolSide: false,
		},
		{
			name: "dns belongs to the target", mode: "get",
			out:    "curl_cffi.requests.exceptions.ConnectionError: Could not resolve host: nope.invalid",
			runErr: errors.New("exit status 1"), wantToolSide: false,
		},
		{
			// Browser-only branch: the same text from a plain GET proves nothing.
			name: "browser markers ignored outside browser modes", mode: "get",
			out:    "Executable doesn't exist at /opt/ms-playwright/chromium-1228/chrome",
			runErr: errors.New("exit status 1"), wantToolSide: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, hint, toolSide := scrapeRunError(context.Background(), tc.runErr,
				[]byte(tc.out), tc.mode, "https://example.com/x", 60, false, 0)
			if toolSide != tc.wantToolSide {
				t.Fatalf("toolSide = %v, want %v (hint=%q)", toolSide, tc.wantToolSide, hint)
			}
			if tc.wantHintContains != "" && !strings.Contains(hint, tc.wantHintContains) {
				t.Fatalf("hint = %q, want it to mention %q", hint, tc.wantHintContains)
			}
		})
	}
}

// TestToolFailMarksOutcome: the failure return carries the flag the search path reads.
func TestToolFailMarksOutcome(t *testing.T) {
	out, msg, hint := toolFail("sandbox: read-only file system", "")
	if !out.ToolFailure {
		t.Fatal("toolFail must mark the outcome as our own failure")
	}
	if msg == "" || hint != "" {
		t.Fatalf("toolFail should pass through (msg, hint) verbatim, got (%q, %q)", msg, hint)
	}
}
