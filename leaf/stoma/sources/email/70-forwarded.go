package email

import "strings"

// resolveForwardedAlias returns the ORIGINAL recipient alias this message
// was delivered for — an address whose domain is one of ownedDomains — or
// "" when the feature is off (no ownedDomains) or no owned recipient is
// found (e.g. the mailbox was BCC'd by a non-forwarder).
//
// When Cloudflare Email Routing forwards agent@/sales@/… of your own domain
// into one Gmail, every message lands in the same inbox. The alias is what
// lets bob tell them apart (route per-alias / reply-to the alias). The
// signal lives in the forwarded headers.
//
// Priority — most-reliable first:
//  1. X-Forwarded-For: Cloudflare stamps "<original-recipient> <forwarded-to>".
//     The first owned token is the alias, and it is present even for BCC.
//  2. To, then Cc: the original recipient survives forwarding in these
//     headers for ordinary directly-addressed mail.
//
// The ownedDomains gate keeps us from mistaking the forwarding mailbox
// itself or an unrelated Cc for the alias. ownedDomains must already be
// lowercased/trimmed (applyDefaults does this).
func resolveForwardedAlias(pe *parsedEmail, ownedDomains []string) string {
	if pe == nil || len(ownedDomains) == 0 {
		return ""
	}
	// 1. X-Forwarded-For tokens (whitespace-separated address list).
	for _, tok := range strings.Fields(pe.XForwardedFor) {
		if a := CanonicalAddress(tok); addrInOwnedDomain(a, ownedDomains) {
			return a
		}
	}
	// 2. To, then Cc.
	for _, raw := range pe.ToAddrs {
		if a := CanonicalAddress(raw); addrInOwnedDomain(a, ownedDomains) {
			return a
		}
	}
	for _, raw := range pe.CcAddrs {
		if a := CanonicalAddress(raw); addrInOwnedDomain(a, ownedDomains) {
			return a
		}
	}
	return ""
}

// addrInOwnedDomain reports whether addr's domain is one of ownedDomains
// (already lowercased/trimmed by applyDefaults). addr should already be a
// canonical address. Shared by resolveForwardedAlias (which alias is this?)
// and replyAllCc (which reply-all recipients are bob's own and must be
// dropped to prevent a self-CC mail loop across multiple forwarded aliases).
func addrInOwnedDomain(addr string, ownedDomains []string) bool {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return false
	}
	dom := addr[at+1:]
	for _, d := range ownedDomains {
		if dom == d {
			return true
		}
	}
	return false
}
