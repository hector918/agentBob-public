package browserremote

import (
	"encoding/json"
	"strings"

	"agentbob/contract"
)

// Shared helpers for the single `browser` tool (browser.go). The 13 separate
// browser_* tool shells were collapsed into one action-dispatched tool; this file
// now holds only the cross-action helpers (success marshalling + the DNS-hint
// classifier ported from skeleton's neterror.Hint).

// jsonOK marshals v as the success envelope's data payload. Marshal failure
// returns an error envelope rather than panicking — keeps the loop unblockable
// (same contract as the sibling tools).
func jsonOK(v any) contract.ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return contract.ErrResult("marshal: " + err.Error())
	}
	return contract.OKResult(string(b))
}

// dnsHint is the canned redirection text for DNS / unreachable-host failures,
// ported from skeleton's neterror.Hint. Prescriptive: names a concrete next step
// so small models stop guessing variant domains.
const dnsHint = "If you were guessing this URL from memory, stop. " +
	"Search first: call browser with action=browser_navigate and " +
	"\"https://search.brave.com/search?q=YOUR+QUERY+HERE\" then action=browser_snapshot " +
	"to read the result list and pick a real URL. " +
	"Do NOT try other variations of the same brand name — DNS errors mean " +
	"the domain doesn't exist, not that you should retry with a different path."

var dnsPatterns = []string{
	"ERR_NAME_NOT_RESOLVED",
	"ERR_NAME_RESOLUTION_FAILED",
	"ERR_INTERNET_DISCONNECTED",
	"ERR_CONNECTION_REFUSED",
	"ERR_ADDRESS_UNREACHABLE",
	"no such host",
	"server misbehaving",
	"connection refused",
	"network is unreachable",
	"ConnectError",
	"Cannot connect to host",
	"NameResolutionError",
	"getaddrinfo failed",
}

func isDNSClass(msg string) bool {
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
