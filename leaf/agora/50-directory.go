package agora

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentbob/leaf/agora/store"
)

// This file ports skeleton agora/96-directory.go (the reachability computation)
// + skeleton prompt/67-source-agora-directory.go (the string rendering) into one
// place: agora computes the caller's addressable directory from one OrgCache
// snapshot and renders it to the Chinese prompt text the identity layer drops in.
// The prompt layer never imports agora (boundary A) — TurnContext hands it the
// finished string.
//
// Reachability is the UNION across ALL of the caller's active companies: a member
// employed at two companies reaches every colleague in EITHER company + every
// company's bridge, and sees every company's playbook. (Grants stay the member→
// company intersection — that logic lives in 40-impl.go and is untouched here.)

// dirContact is one addressable contact: the model-facing essentials (name +
// role + which of the caller's companies the colleague is in + inbox scope). A
// colleague shared across two of the caller's companies yields one entry per
// company.
type dirContact struct {
	Name       string
	Role       string
	Company    string // which of the caller's companies this colleague is in
	InboxScope string
}

// dirMembership is one (company, role) the caller holds. A bridge session has a
// single membership with Role "bridge".
type dirMembership struct {
	Company string
	Role    string
}

// dirSelf is the caller's own agora identity within a directory.
type dirSelf struct {
	Name        string          // member name; "" for a bridge session
	Bridge      bool            // true = bridge session (no member)
	Memberships []dirMembership // caller's (company, role) across all active companies
}

// dirPlaybook is one company's operating prose.
type dirPlaybook struct {
	Company string
	Text    string
}

// directory is a caller's addressable directory computed from the org graph for
// one inbox: who they are + everyone they can currently reach across ALL their
// companies + every reachable bridge scope + every company's playbook.
type directory struct {
	Self         dirSelf
	Contacts     []dirContact  // union across ALL the caller's companies
	BridgeScopes []string      // each reachable company's bridge scope
	Playbooks    []dirPlaybook // per-company playbook
}

// renderDirectory builds the caller's directory for inboxID and returns the
// rendered (directoryText, combinedPlaybook). Both are "" when there is nothing
// to show (non-agora / unresolved / build declined). The playbook is returned
// separately so TurnContext can drop it into AgoraTurn.Playbook.
func (a *Impl) renderDirectory(ctx context.Context, inboxID string) (string, string) {
	dir, ok := a.buildDirectory(ctx, inboxID)
	if !ok {
		return "", ""
	}
	return renderDirectoryText(dir), combinePlaybooks(dir.Playbooks)
}

// combinePlaybooks joins the per-company playbooks: exactly one → just its text;
// several → each prefixed with a "## <company> 运作说明" header, separated by a
// blank line. Trimmed. "" when there are none.
func combinePlaybooks(pbs []dirPlaybook) string {
	switch len(pbs) {
	case 0:
		return ""
	case 1:
		return strings.TrimSpace(pbs[0].Text)
	}
	var b strings.Builder
	for i, pb := range pbs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s 运作说明\n", pb.Company)
		b.WriteString(strings.TrimSpace(pb.Text))
	}
	return strings.TrimSpace(b.String())
}

// buildDirectory computes the caller's addressable directory from one OrgCache
// snapshot + the caller's inboxID — the single source the prompt directory + the
// send-resolver reachability share. ok=false when the directory can't be built
// (non-agora inbox / no active employment / no live company).
//
// The caller's company set is the UNION of all companies it is actively employed
// at (a bridge inbox has the single owning company). Reachability, bridges and
// playbooks all span that whole set.
func (a *Impl) buildDirectory(ctx context.Context, inboxID string) (directory, bool) {
	// One Snapshot (deep-copied) + indexes built from it = the entire data set
	// this call needs, frozen against a concurrent OrgCache Load.
	snap := a.org.Snapshot(ctx, nil)
	companyByID := make(map[string]store.Company, len(snap.Companies))
	for _, co := range snap.Companies {
		companyByID[co.ID] = co
	}
	memberByID := make(map[string]store.Member, len(snap.Members))
	for _, m := range snap.Members {
		memberByID[m.ID] = m
	}
	inboxByID := make(map[string]store.Inbox, len(snap.Inboxes))
	inboxByScope := make(map[string]store.Inbox, len(snap.Inboxes))
	for _, ib := range snap.Inboxes {
		inboxByID[ib.Inbox.ID] = ib.Inbox
		inboxByScope[ib.Inbox.Scope] = ib.Inbox
	}

	callerInbox, found := inboxByID[inboxID]
	if !found {
		return directory{}, false
	}

	// Determine the caller's company set + identity.
	//   - bridge inbox (OwnerCompanyID set): the single owning company.
	//   - member inbox (MemberID set): EVERY company the member has an active
	//     employment at. This subsumes the old inbox-name derivation, the
	//     single-employment fallback AND the employment gate — a departed member
	//     has no active employment → empty set → no directory.
	var (
		callerMemberID string
		isBridge       bool
	)
	// companyIDs collects the caller's company IDs (deduped); roleByCompany maps
	// each member-company to the role at that company's active employment.
	companyIDs := make(map[string]struct{})
	roleByCompany := make(map[string]string)
	switch {
	case callerInbox.OwnerCompanyID != "":
		isBridge = true
		companyIDs[callerInbox.OwnerCompanyID] = struct{}{}
	case callerInbox.MemberID != "":
		callerMemberID = callerInbox.MemberID
		for _, e := range snap.Employments {
			if e.Status == "active" && e.MemberID == callerMemberID {
				companyIDs[e.CompanyID] = struct{}{}
				roleByCompany[e.CompanyID] = e.RoleName
			}
		}
	}

	// Keep only companies that exist and are not disabled, then sort by company
	// NAME (deterministic).
	companies := make([]store.Company, 0, len(companyIDs))
	for id := range companyIDs {
		co, ok := companyByID[id]
		if !ok || co.Status == "disabled" {
			continue
		}
		companies = append(companies, co)
	}
	if len(companies) == 0 {
		return directory{}, false
	}
	sort.Slice(companies, func(i, j int) bool { return companies[i].Name < companies[j].Name })

	// ---- self ----
	self := dirSelf{Bridge: isBridge}
	if isBridge {
		// A bridge session has no member; it has exactly one owning company.
		self.Memberships = []dirMembership{{Company: companies[0].Name, Role: "bridge"}}
	} else {
		if mem, ok := memberByID[callerMemberID]; ok {
			self.Name = mem.Name
		}
		for _, co := range companies {
			self.Memberships = append(self.Memberships, dirMembership{Company: co.Name, Role: roleByCompany[co.ID]})
		}
	}

	// ---- contacts (union across all the caller's companies) ----
	// For EACH company in the set, every member with an active employment there
	// (skip the caller), addressed by MemberOwnedInboxScope(name, companyID); kept
	// only when that inbox exists and is active. A colleague shared across two of
	// the caller's companies yields two entries (one per company).
	contacts := make([]dirContact, 0)
	for _, co := range companies {
		for _, e := range snap.Employments {
			if e.Status != "active" || e.CompanyID != co.ID {
				continue
			}
			if callerMemberID != "" && e.MemberID == callerMemberID {
				continue // skip self
			}
			mem, ok := memberByID[e.MemberID]
			if !ok {
				continue
			}
			colleagueScope := MemberOwnedInboxScope(mem.Name, co.ID)
			ib, ok := inboxByScope[colleagueScope]
			if !ok || ib.Status != "active" {
				continue // no live company inbox for this colleague
			}
			contacts = append(contacts, dirContact{
				Name:       mem.Name,
				Role:       e.RoleName,
				Company:    co.Name,
				InboxScope: ib.Scope,
			})
		}
	}
	sort.Slice(contacts, func(i, j int) bool {
		if contacts[i].Company != contacts[j].Company {
			return contacts[i].Company < contacts[j].Company
		}
		return contacts[i].Name < contacts[j].Name
	})

	// ---- bridges (each reachable company's bridge scope, deduped) ----
	bridgeScopes := make([]string, 0)
	seenBridge := make(map[string]struct{})
	for _, co := range companies {
		if co.ReportInboxID == "" {
			continue
		}
		bridge, ok := inboxByID[co.ReportInboxID]
		if !ok {
			continue
		}
		if _, dup := seenBridge[bridge.Scope]; dup {
			continue
		}
		seenBridge[bridge.Scope] = struct{}{}
		bridgeScopes = append(bridgeScopes, bridge.Scope)
	}

	// ---- playbooks (one per company with non-empty trimmed prose) ----
	playbooks := make([]dirPlaybook, 0)
	for _, co := range companies {
		if strings.TrimSpace(co.Playbook) == "" {
			continue
		}
		playbooks = append(playbooks, dirPlaybook{Company: co.Name, Text: co.Playbook})
	}

	return directory{
		Self:         self,
		Contacts:     contacts,
		BridgeScopes: bridgeScopes,
		Playbooks:    playbooks,
	}, true
}

// renderDirectoryText renders the caller's identity + reachable contacts to the
// Chinese prompt text the identity layer uses (ported from skeleton
// prompt/67-source-agora-directory.go::Render — the playbook section is rendered
// by the prompt layer from AgoraTurn.Playbook, so it is NOT appended here).
func renderDirectoryText(dir directory) string {
	var b strings.Builder

	// ---- self identity ----
	b.WriteString("agora 身份：")
	switch {
	case dir.Self.Bridge:
		company := ""
		if len(dir.Self.Memberships) > 0 {
			company = dir.Self.Memberships[0].Company
		}
		fmt.Fprintf(&b, "你在 %s 的桥接会话（bridge）", company)
	default:
		name := dir.Self.Name
		if name == "" {
			name = "(未命名)"
		}
		fmt.Fprintf(&b, "你是 %s", name)
		switch len(dir.Self.Memberships) {
		case 0:
			// no memberships — nothing more to say
		case 1:
			m := dir.Self.Memberships[0]
			if m.Role != "" {
				fmt.Fprintf(&b, "，在 %s 任 %s", m.Company, m.Role)
			} else {
				fmt.Fprintf(&b, "，在 %s", m.Company)
			}
		default:
			b.WriteString("，身兼数职：")
			for i, m := range dir.Self.Memberships {
				if i > 0 {
					b.WriteString("、")
				}
				if m.Role != "" {
					fmt.Fprintf(&b, "%s（%s）", m.Company, m.Role)
				} else {
					b.WriteString(m.Company)
				}
			}
		}
	}
	b.WriteString("。")

	// ---- bridges (报告/对上沟通) ----
	if len(dir.BridgeScopes) > 0 {
		fmt.Fprintf(&b, "汇报/对上沟通走 bridge inbox：%s。", strings.Join(dir.BridgeScopes, "、"))
	}

	// ---- reachable contacts (union across all the caller's companies) ----
	if len(dir.Contacts) > 0 {
		b.WriteString("\n你现在可以联系（用 send_message，to= 填对方成员名或 inbox_scope）：")
		for _, c := range dir.Contacts {
			b.WriteString("\n- ")
			b.WriteString(renderContact(c))
		}
	}
	return b.String()
}

// renderContact formats one contact as "name（role @ company）→ inbox_scope"
// (role omitted when empty; the company is always shown so a multi-company caller
// can tell which company a colleague belongs to).
func renderContact(c dirContact) string {
	name := c.Name
	if name == "" {
		name = "(未命名)"
	}
	line := name
	switch {
	case c.Role != "" && c.Company != "":
		line += "（" + c.Role + " @ " + c.Company + "）"
	case c.Company != "":
		line += "（@ " + c.Company + "）"
	case c.Role != "":
		line += "（" + c.Role + "）"
	}
	line += " → " + c.InboxScope
	return line
}
