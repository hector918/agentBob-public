// Package neterror centralises detection of "you can't reach this URL"
// failures across the fetch tools (browser_navigate, web_extract,
// scrapling), and provides the canned hint we attach to those results so
// small models stop guessing variant domains and try a search engine
// instead.
//
// Why a shared package: a guessing-loop on URLs is the single most common
// dead-end behaviour for local models when web_search is unavailable. The
// fix needs to be uniform across every tool that fetches a URL — keeping
// the patterns + hint text in one place means we can tune them once.
package neterror

import "strings"

// Hint is the canned redirection text. It is *prescriptive*: it names the
// concrete next step (a real URL the model can navigate to) instead of
// abstract "search instead". Small models follow concrete instructions far
// better than abstract ones.
//
// Brave is chosen because it returns a server-rendered result page that
// browser_snapshot/web_extract can read without JS. (It replaced DuckDuckGo's
// /html/ endpoint on: that one now redirects to a JS shell with no
// results in it — measured, see docs/web-fleet.md §4.1.)
const Hint = "If you were guessing this URL from memory, stop. " +
	"Search first: call browser_navigate with " +
	"\"https://search.brave.com/search?q=YOUR+QUERY+HERE\" then browser_snapshot " +
	"to read the result list and pick a real URL. " +
	"Do NOT try other variations of the same brand name — DNS errors mean " +
	"the domain doesn't exist, not that you should retry with a different path."

// dnsPatterns are substrings we look for in error messages from chromedp,
// Go's net/http, and the scrapling subprocess. Each indicates "the URL is
// not reachable" — actionable advice is the same for all of them.
var dnsPatterns = []string{
	// chromedp / chromium net error codes:
	"ERR_NAME_NOT_RESOLVED",
	"ERR_NAME_RESOLUTION_FAILED",
	"ERR_INTERNET_DISCONNECTED",
	"ERR_CONNECTION_REFUSED",
	"ERR_ADDRESS_UNREACHABLE",

	// Go net/http / DNS:
	"no such host",
	"server misbehaving",
	"connection refused",
	"network is unreachable",

	// Scrapling / Python httpx / curl_cffi:
	"ConnectError",
	"Cannot connect to host",
	"NameResolutionError",
	"getaddrinfo failed",
}

// IsDNSClass reports whether the message indicates a DNS / connection
// failure that the user should fix by searching for a real URL rather
// than retrying.
func IsDNSClass(msg string) bool {
	if msg == "" {
		return false
	}
	for _, p := range dnsPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
