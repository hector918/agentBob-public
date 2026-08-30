package browser

import "testing"

// TestCDPAllowlistExcludesGetAllCookies pins decision #7: Network.getAllCookies
// must NOT be on the browser_cdp allowlist — it returns every cookie for every
// origin in the profile (session tokens of every site the admin logged into),
// a one-call cross-site exfil vector far broader than any single task needs.
// Network.getCookies (current page only) stays. The dangerous escape-hatch
// methods the allowlist exists to keep out must also stay denied. Keys are
// stored lowercased and looked up via strings.ToLower in CDPExec.
func TestCDPAllowlistExcludesGetAllCookies(t *testing.T) {
	denied := []string{
		"network.getallcookies",   // the decision-#7 exfil vector
		"page.navigate",           // file:///…/credentials/*
		"runtime.evaluate",        // arbitrary JS in page context
		"network.setcookie",       // write, not read
		"target.createtarget",     // out-of-pool pages
		"page.handlejavascriptdialog",
	}
	for _, m := range denied {
		if cdpAllowMethods[m] {
			t.Errorf("CDP allowlist must NOT permit %q", m)
		}
	}

	allowed := []string{
		"network.getcookies", // current-page cookie read — legitimate, scoped
		"page.capturesnapshot",
		"dom.getdocument",
	}
	for _, m := range allowed {
		if !cdpAllowMethods[m] {
			t.Errorf("CDP allowlist should still permit %q", m)
		}
	}
}
