package agora

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/agora/store"

	"gopkg.in/yaml.v3"
)

// panel is agora's self-description for the webui — Phase 2: the org rendered as a
// real node/edge GRAPH via the generic "graph" field (company/member/inbox bands +
// employment + inbox-ownership edges), drawn by the same drawGraph geometry as the
// home graph. Each node click opens a read-only glance with the entity's detail +
// per-entity action buttons (prefill /agora commands, picker, or yaml editor).
// AdminOnly (org data is sensitive); Tier "flow" (the flow band).
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "agora",
		Title:     "Agora",
		AdminOnly: true,
		Tier:      "flow",
		Settings: []contract.SettingSpec{
			{ID: "bootstrap", Kind: "action", Label: "bootstrap", Command: "/agora bootstrap --company= --founder="},
			{ID: "new-company", Kind: "action", Label: "new company", Command: "/agora company create --name="},
			{ID: "new-member", Kind: "action", Label: "new member", Command: "/agora member create --name="},
			{ID: "reload", Kind: "action", Label: "reload", Command: "/agora reload"},
		},
		State: func(ctx context.Context) []contract.StateField {
			snap := m.org.Snapshot(ctx, nil) // nil sessionCounter → ActiveSessions 0 (Phase 2)
			if len(snap.Companies) == 0 {
				return []contract.StateField{{Kind: "stat", Label: "agora", Value: "not bootstrapped", Status: "warn"}}
			}

			companyName := make(map[string]string, len(snap.Companies))
			for _, c := range snap.Companies {
				companyName[c.ID] = c.Name
			}
			memberName := make(map[string]string, len(snap.Members))
			for _, mb := range snap.Members {
				memberName[mb.ID] = mb.Name
			}
			// member pick list for the company "hire" picker: each member + whether
			// they are already employed elsewhere ("hired by: …" or "free").
			empByMember := map[string][]string{}
			for _, e := range snap.Employments {
				empByMember[e.MemberID] = append(empByMember[e.MemberID], companyName[e.CompanyID])
			}
			memberPick := make([]contract.PickItem, 0, len(snap.Members))
			for _, mb := range snap.Members {
				sub := "free"
				if cos := empByMember[mb.ID]; len(cos) > 0 {
					sub = "hired by: " + strings.Join(cos, ", ")
				}
				memberPick = append(memberPick, contract.PickItem{Value: mb.Name, Label: mb.Name, Sub: sub})
			}

			nodes := make([]contract.GraphNode, 0, len(snap.Companies)+len(snap.Members)+len(snap.Inboxes))
			edges := make([]contract.GraphEdge, 0, len(snap.Employments)+len(snap.Inboxes))

			// companies (detail = static fields + a self-described employee table with
			// per-row [change role][terminate] — the webui renders it generically; it
			// holds no agora knowledge).
			for _, c := range snap.Companies {
				var d strings.Builder
				fmt.Fprintf(&d, "company %s\nstatus: %s\n", c.ID, c.Status)
				if c.ReportInboxID != "" {
					fmt.Fprintf(&d, "report inbox: %s\n", c.ReportInboxID)
				}
				cact := contract.GraphAction{Label: "disable", Command: "/agora company disable " + c.ID}
				if c.Status == "disabled" {
					cact = contract.GraphAction{Label: "enable", Command: "/agora company enable " + c.ID}
				}
				// pause/resume is an independent in-memory toggle (stops the message
				// link without rerouting to the normal flow; cleared on restart).
				pact := contract.GraphAction{Label: "pause", Command: "/agora company pause " + c.ID}
				if m.impl.IsPausedCompany(c.ID) {
					pact = contract.GraphAction{Label: "resume", Command: "/agora company resume " + c.ID}
				}
				// hire belongs to the company — a picker lists members (with their
				// employment status); clicking one fills %s and prefills the command.
				hire := contract.GraphAction{Label: "hire", Command: "/agora hire --company=" + c.Name + " --member=%s --role=", Pick: memberPick}
				// employee roster as a self-described table — each row carries its own
				// [change role][terminate] commands (the webui just renders + prefills).
				var empRows [][]contract.Cell
				for _, e := range snap.Employments {
					if e.CompanyID != c.ID {
						continue
					}
					nm := memberName[e.MemberID]
					empRows = append(empRows, []contract.Cell{
						// the member name is a drill-down link → the member's glance.
						{Text: nm, Open: &contract.ViewTarget{Panel: "agora", Node: e.MemberID, Kind: "glance", Title: nm}},
						{Text: e.RoleName, Actions: []contract.CellAction{
							{Label: "change role", Command: "/agora change-role --company=" + c.ID + " --member=" + nm + " --role="},
							{Label: "terminate", Command: "/agora terminate --company=" + c.ID + " --member=" + nm},
						}},
					})
				}
				var empFields []contract.StateField
				if len(empRows) > 0 {
					empFields = []contract.StateField{{Kind: "table", Label: "employees", Columns: []string{"member", "role"}, Rows: empRows}}
				}
				coActs := []contract.GraphAction{
					cact, pact, hire,
					{Label: "edit permissions", Edit: &contract.EditTarget{Panel: "agora", Setting: "perms:" + c.ID, Lang: "yaml"}},
					{Label: "edit playbook", Edit: &contract.EditTarget{Panel: "agora", Setting: "playbook:" + c.ID, Lang: "markdown"}},
				}
				// "delete" surfaces ONLY on an already-disabled company (DeleteCompany's
				// hard precondition) — clicking prefills the confirm command into the dock
				// for the admin to press enter (confirm-by-prefill, like terminate/unwire).
				if c.Status == "disabled" {
					coActs = append(coActs, contract.GraphAction{Label: "delete", Command: "/agora company delete " + c.ID + " confirm"})
				}
				nodes = append(nodes, contract.GraphNode{
					ID: c.ID, Band: "company", Title: c.Name, Sub: "company",
					Status: companyStatus(c.Status), Detail: d.String(), Fields: empFields, Actions: coActs,
				})
			}

			// member → their owned inboxes, as a clickable bottom list (each row's name
			// drills into the inbox's glance — the member→inbox path). Bridges are
			// company-owned, not listed under a member.
			inboxRowsByMember := map[string][][]contract.Cell{}
			for _, ib := range snap.Inboxes {
				if ib.MemberID == "" || ib.Kind == "bridge" {
					continue
				}
				inboxRowsByMember[ib.MemberID] = append(inboxRowsByMember[ib.MemberID], []contract.Cell{
					{Text: ib.Name, Open: &contract.ViewTarget{Panel: "agora", Node: ib.ID, Kind: "glance", Title: ib.Name}},
				})
			}

			// members (detail lists their cross-company employments).
			for _, mb := range snap.Members {
				var d strings.Builder
				fmt.Fprintf(&d, "member %s\nmodel pref: %s\nprompt style: %s\n\nemployments:\n", mb.ID, mb.ModelPrefTag, mb.PromptStyle)
				for _, e := range snap.Employments {
					if e.MemberID == mb.ID {
						fmt.Fprintf(&d, "  %s — %s\n", companyName[e.CompanyID], e.RoleName)
					}
				}
				// the radial ring position already says "member"; show only the
				// model-pref tag (if any) so the node stays compact.
				sub := mb.ModelPrefTag
				// inboxes as a clickable bottom list (drill into each); "add inbox" stays a
				// top action.
				var memberFields []contract.StateField
				if rows := inboxRowsByMember[mb.ID]; len(rows) > 0 {
					// blank column label → the webui skips the (redundant) header row; the
					// "inboxes" section header already names the list.
					memberFields = []contract.StateField{{Kind: "table", Label: "inboxes", Columns: []string{""}, Rows: rows}}
				}
				macts := []contract.GraphAction{
					{Label: "edit prompt style", Edit: &contract.EditTarget{Panel: "agora", Setting: "promptstyle:" + mb.ID, Lang: "markdown"}},
					{Label: "add inbox", Command: "/agora inbox create --member=" + mb.Name + " --name="},
				}
				nodes = append(nodes, contract.GraphNode{ID: mb.ID, Band: "member", Title: mb.Name, Sub: sub, Detail: d.String(),
					Fields: memberFields, Actions: macts})
			}

			// Wired external targets per inbox: one query ("" → all sources). An
			// inbox_source binds an external chat; its scope (contract.ScopeFor over the
			// coords) is the "target scope" the inbox is wired to. Shown on each inbox node
			// so an admin sees at a glance how many — and which — real chats an inbox/bridge
			// reaches. Best-effort: a nil store / load error just omits wiring (graph still renders).
			wiredByInbox := map[string][]string{}
			if m.store != nil {
				if srcs, err := m.store.ListInboxSources(ctx, ""); err == nil {
					seen := map[string]bool{} // dedup rows that flatten to one scope (group topics)
					for _, s := range srcs {
						sc := contract.ScopeFor(s.SourceID, s.ChatType, s.ChatID, s.ThreadID)
						k := s.InboxID + "\x00" + sc
						if seen[k] {
							continue
						}
						seen[k] = true
						wiredByInbox[s.InboxID] = append(wiredByInbox[s.InboxID], sc)
					}
				}
			}

			// inboxes (detail = static fields).
			for _, ib := range snap.Inboxes {
				owner := memberName[ib.MemberID]
				if ib.Kind == "bridge" {
					owner = companyName[ib.OwnerCompanyID]
				}
				// A company inbox is named with the company's ID (opaque); show it
				// readably as "<company>#inbox". Member-created inboxes keep their own
				// name. Display-only — node ID + scope/routing are unchanged.
				title := ib.Name
				if co, ok := companyName[ib.Name]; ok {
					title = co + "#inbox"
				}
				wired := wiredByInbox[ib.ID]
				d := fmt.Sprintf("inbox name: %s\nid: %s\nkind: %s\nowner: %s\nscope: %s\nstatus: %s\nwired: %d", title, ib.ID, ib.Kind, owner, ib.Scope, ib.Status, len(wired))
				// the wired external target scopes, each with an "unwire" button that prefills
				// /agora inbox unwire <inboxID> <scope> into the dock (confirm-by-prefill) —
				// only when there is at least one binding.
				var fields []contract.StateField
				if len(wired) > 0 {
					sort.Strings(wired)
					rows := make([][]contract.Cell, 0, len(wired))
					for _, sc := range wired {
						rows = append(rows, []contract.Cell{{
							Text:        sc,
							Action:      "/agora inbox unwire " + ib.ID + " " + sc,
							ActionLabel: "unwire",
						}})
					}
					fields = []contract.StateField{{Kind: "table", Label: "wired targets", Columns: []string{"target scope"}, Rows: rows}}
				}
				// disambiguate the sub from the "member" band ("member inbox" /
				// "bridge inbox"); an active bridge gets its own color (degraded bridges
				// still show warn/down so health isn't hidden).
				st := inboxStatus(ib.Status)
				if ib.Kind == "bridge" && ib.Status == "active" {
					st = "bridge"
				}
				// "chat log" opens the inbox's conversation (read-only, paged) — data
				// composed by flow/agora's "agora-chat" panel (this graph stays decoupled
				// from the message store; it only links by inbox id).
				iacts := []contract.GraphAction{
					{Label: "chat log", Open: &contract.ViewTarget{Panel: "agora-chat", View: "inbox:" + ib.ID, Kind: "glance", Title: title + " · chat"}},
					{Label: "wire", Command: "/agora inbox wire " + ib.ID},
				}
				if ib.Status == "paused" {
					iacts = append(iacts, contract.GraphAction{Label: "resume", Command: "/agora inbox resume " + ib.ID})
				} else if ib.Status == "active" {
					iacts = append(iacts, contract.GraphAction{Label: "pause", Command: "/agora inbox pause " + ib.ID})
				}
				// "edit visibility": the per-inbox HIDE denylist (member inboxes only —
				// a bridge has no member turn to narrow). docs §14.3.
				if ib.MemberID != "" {
					iacts = append(iacts, contract.GraphAction{Label: "edit visibility",
						Edit: &contract.EditTarget{Panel: "agora", Setting: "visibility:" + ib.ID, Lang: "yaml"}})
				}
				nodes = append(nodes, contract.GraphNode{
					ID: ib.ID, Band: "inbox", Title: title, Sub: ib.Kind + " inbox",
					Status: st, Detail: d, Fields: fields, Actions: iacts,
				})
				// ownership edge: inbox → its member (member inbox) or company (bridge).
				if ib.Kind == "bridge" && ib.OwnerCompanyID != "" {
					edges = append(edges, contract.GraphEdge{From: ib.ID, To: ib.OwnerCompanyID})
				} else if ib.MemberID != "" {
					edges = append(edges, contract.GraphEdge{From: ib.ID, To: ib.MemberID})
				}
			}

			// employment edges: member → company, labeled with the role.
			for _, e := range snap.Employments {
				edges = append(edges, contract.GraphEdge{From: e.MemberID, To: e.CompanyID, Label: e.RoleName})
			}

			return []contract.StateField{{
				Kind: "graph", Label: "org", Layout: "radial",
				Bands: []string{"company", "member", "inbox"},
				Nodes: nodes, Edges: edges,
			}}
		},
		// Per-ENTITY editors, opened contextually from a node action (sid = "<kind>:<id>"):
		// company "edit permissions" / "edit playbook" (perms:/playbook:<companyID>) and
		// member "edit prompt style" (promptstyle:<memberID>). Not listed in Settings — the
		// webui route addresses Read/Apply by sid directly.
		Read: func(_ context.Context, sid string) (string, error) {
			kind, id, ok := splitSID(sid)
			if !ok {
				return "", fmt.Errorf("agora: bad setting %q", sid)
			}
			if kind == "promptstyle" { // per-MEMBER editor (id is a member id)
				mb, exists := m.org.MemberByID(id)
				if !exists {
					return "", fmt.Errorf("agora: unknown member %q", id)
				}
				return mb.PromptStyle, nil
			}
			if kind == "visibility" { // per-INBOX denylist editor (id is an inbox id)
				ib, exists := m.org.InboxByID(id)
				if !exists {
					return "", fmt.Errorf("agora: unknown inbox %q", id)
				}
				return renderVisibility(ib.Visibility, toolNames(m.toolCat), skillNames(m.skillCat)), nil
			}
			co, exists := m.org.CompanyByID(id)
			if !exists {
				return "", fmt.Errorf("agora: unknown company %q", id)
			}
			switch kind {
			case "perms":
				// merge-on-open: fold the live tools/skills into the stored matrix
				// (missing ones default off) + annotate any entry that no longer exists.
				var mf matrixFile
				if err := yaml.Unmarshal([]byte(co.PermissionsYAML), &mf); err != nil {
					// Parse failure (corrupt / legacy yaml): do NOT default-ize — that
					// would hide the real config behind an all-off default matrix, and a
					// save would overwrite the real grants. Echo the stored body RAW with a
					// warning header so the admin fixes it in place first (Apply re-validates,
					// so a save can't commit until the body parses). Empty yaml is NOT an
					// error → still gets the normal default matrix below.
					return "# ⚠ 权限 yaml 解析失败：" + err.Error() + "\n" +
						"# 下面是存储的原始内容（未合并、未默认化）。请先修复格式再编辑保存——\n" +
						"# 在修好之前保存会被校验拒绝；不要清空本内容，否则真实授权会丢失。\n\n" +
						co.PermissionsYAML, nil
				}
				if len(mf.Roles) == 0 {
					mf.Roles = []string{"boss", "member", "intern"}
				}
				if mf.Grants == nil {
					mf.Grants = map[string]map[string]string{}
				}
				live, liveOK := m.liveCaps()
				// Only merge the live inventory in when a catalog actually resolved;
				// otherwise we'd fold in nothing but the two bundle rows and, worse,
				// annotate every real grant as removed (see renderPermsMatrix liveOK).
				if liveOK {
					for cap := range live {
						if _, ok := mf.Grants[cap]; !ok {
							cell := make(map[string]string, len(mf.Roles))
							for _, r := range mf.Roles {
								cell[r] = "off"
							}
							mf.Grants[cap] = cell
						}
					}
				}
				return renderPermsMatrix(mf, live, liveOK), nil
			case "playbook":
				return co.Playbook, nil
			}
			return "", fmt.Errorf("agora: bad setting %q", sid)
		},
		Apply: func(ctx context.Context, sid, body string) contract.WriteResult {
			kind, id, ok := splitSID(sid)
			if !ok {
				return contract.WriteResult{Phase: "validate", Error: "bad setting"}
			}
			if kind == "promptstyle" { // per-MEMBER editor (id is a member id)
				mb, exists := m.org.MemberByID(id)
				if !exists {
					return contract.WriteResult{Phase: "validate", Error: "unknown member"}
				}
				if err := m.store.SetMemberPromptStyle(ctx, id, body, clock.UnixEpoch()); err != nil {
					return contract.WriteResult{Phase: "write", Error: err.Error()}
				}
				mb.PromptStyle = body
				m.org.UpsertMember(mb)
				return contract.WriteResult{Success: true, Phase: "write"}
			}
			if kind == "visibility" { // per-INBOX denylist editor
				if _, exists := m.org.InboxByID(id); !exists {
					return contract.WriteResult{Phase: "validate", Error: "unknown inbox"}
				}
				vis, err := parseVisibility(body)
				if err != nil {
					return contract.WriteResult{Phase: "validate", Error: err.Error()}
				}
				// SetInboxVisibility writes store-first then cache; nil vis clears the
				// filter (nothing hidden) so an emptied editor leaves no stored row.
				if err := m.org.SetInboxVisibility(ctx, m.store, id, vis); err != nil {
					return contract.WriteResult{Phase: "write", Error: err.Error()}
				}
				return contract.WriteResult{Success: true, Phase: "write"}
			}
			co, exists := m.org.CompanyByID(id)
			if !exists {
				return contract.WriteResult{Phase: "validate", Error: "unknown company"}
			}
			now := clock.UnixEpoch()
			switch kind {
			case "perms":
				if _, err := parseRolesYAML([]byte(body)); err != nil { // reject before write
					return contract.WriteResult{Phase: "validate", Error: err.Error()}
				}
				// clean-on-save: drop grant rows whose tool/skill no longer exists
				// (guarded: only when the live catalogs actually resolved, so an absent
				// catalog can't wipe the whole matrix).
				var mf matrixFile
				if err := yaml.Unmarshal([]byte(body), &mf); err != nil {
					return contract.WriteResult{Phase: "validate", Error: err.Error()}
				}
				out := body
				// Prune only when a catalog actually RESOLVED (ok) — never on map size,
				// which is always ≥2 (the arrangement bundles). An unresolved catalog must
				// leave the body untouched, not wipe every real grant.
				if live, ok := m.liveCaps(); ok {
					for cap := range mf.Grants {
						// Only prune caps liveCaps actually inventories (tool/skill). A
						// credential:use:* (or any future kind) is never in live — deleting
						// it would silently drop a legitimate grant, not clean a dead one.
						if prunableCap(cap) && !live[cap] {
							delete(mf.Grants, cap)
						}
					}
					out = renderPermsMatrix(mf, live, ok)
				}
				// Write-through the canonical helper: store-first + cache roles + in-place
				// PermissionsYAML refresh, serialised under the per-company writeMu. Replaces
				// the hand-rolled store.SetPermissionsYAML + UpsertCompany + Seed (which raced
				// the playbook save on the unguarded whole-row `co`). Body is already validated
				// above, so the helper's parse error can't fire here.
				if err := m.org.SetCompanyRolesFromYAML(ctx, m.store, id, []byte(out), now); err != nil {
					return contract.WriteResult{Phase: "write", Error: err.Error()}
				}
				return contract.WriteResult{Success: true, Phase: "write"}
			case "playbook":
				if err := m.store.SetCompanyPlaybook(ctx, id, body, now); err != nil {
					return contract.WriteResult{Phase: "write", Error: err.Error()}
				}
				co.Playbook = body
				m.org.UpsertCompany(co)
				return contract.WriteResult{Success: true, Phase: "write"}
			}
			return contract.WriteResult{Phase: "validate", Error: "bad setting"}
		},
	}
}

// toolNames / skillNames list the full catalog's bare names (sorted) for the
// visibility editor's "pick what to hide" reference. nil thunk/catalog → nil.
func toolNames(thunk func() contract.ToolCatalog) []string {
	if thunk == nil {
		return nil
	}
	cat := thunk()
	if cat == nil {
		return nil
	}
	var out []string
	for _, s := range cat.Specs() {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func skillNames(thunk func() contract.SkillCatalog) []string {
	if thunk == nil {
		return nil
	}
	cat := thunk()
	if cat == nil {
		return nil
	}
	var out []string
	for _, si := range cat.List() {
		out = append(out, si.Name)
	}
	sort.Strings(out)
	return out
}

// renderVisibility renders the per-inbox DENYLIST editor body: a header explaining
// the semantics + the full catalog names to pick from, then the current hidden lists
// (yaml). Empty lists → editable `[]` so the admin sees the keys.
func renderVisibility(vis *store.InboxVisibility, allTools, allSkills []string) string {
	var ht, hs []string
	if vis != nil {
		ht, hs = vis.HiddenTools, vis.HiddenSkills
	}
	var b strings.Builder
	b.WriteString("# 可见性 = 黑名单:列出要对本 inbox 隐藏的 tools / skills(不在名单里=可见)。\n")
	b.WriteString("# 留空 = 不隐藏(已授权的全部可见)。可见性只收窄、不扩大——隐藏只能减少能用的。\n")
	b.WriteString("# 可隐藏的全部名字(从中挑):\n")
	b.WriteString("#   tools:  " + strings.Join(allTools, ", ") + "\n")
	b.WriteString("#   skills: " + strings.Join(allSkills, ", ") + "\n\n")
	b.WriteString(renderHideList("hidden_tools", ht))
	b.WriteString(renderHideList("hidden_skills", hs))
	return b.String()
}

// renderHideList renders one `key:` block as yaml, with `[]` when empty so the key is
// visible/editable.
func renderHideList(key string, names []string) string {
	if len(names) == 0 {
		return key + ": []\n"
	}
	var b strings.Builder
	b.WriteString(key + ":\n")
	for _, n := range names {
		b.WriteString("  - " + n + "\n")
	}
	return b.String()
}

// parseVisibility decodes the editor body into a DENYLIST. Comments/blanks ignored
// (yaml). Both lists empty → nil (clear the filter, no stored row). Names are trimmed;
// blanks dropped.
func parseVisibility(body string) (*store.InboxVisibility, error) {
	var v store.InboxVisibility
	if err := yaml.Unmarshal([]byte(body), &v); err != nil {
		return nil, err
	}
	v.HiddenTools = cleanNames(v.HiddenTools)
	v.HiddenSkills = cleanNames(v.HiddenSkills)
	if len(v.HiddenTools) == 0 && len(v.HiddenSkills) == 0 {
		return nil, nil // nothing hidden → clear
	}
	return &v, nil
}

func cleanNames(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range in {
		if n = strings.TrimSpace(n); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// splitSID splits a per-company editor sid "<kind>:<companyID>" into its parts.
func splitSID(sid string) (kind, id string, ok bool) {
	i := strings.IndexByte(sid, ':')
	if i < 0 {
		return "", "", false
	}
	return sid[:i], sid[i+1:], true
}

// liveCaps is the set of currently-registered capabilities as grant strings
// ("tool:use:<name>" / "skill:use:<name>") — the live tool + skill catalogs, the
// SAME projection AuthorizeTools/AuthorizeSkills use. The second return, ok, is true
// only when at least one catalog thunk RESOLVED non-nil: callers must test ok (NOT
// len(set)) before pruning/annotating, because the set always carries the two
// arrangement:role bundles below and so is never empty — an unresolved catalog would
// otherwise look like "no live caps" and wipe every real grant on save.
// prunableCap reports whether clean-on-save has a live inventory to judge cap
// against — i.e. it's a tool/skill grant (liveCaps enumerates exactly those). Other
// kinds (credential:use:* and any future kind) have no catalog, so they are NEVER in
// live and MUST be preserved on save, not mistaken for "removed". Without this guard
// the matrix editor silently drops every credential grant.
func prunableCap(cap string) bool {
	return strings.HasPrefix(cap, "tool:use:") || strings.HasPrefix(cap, "skill:use:")
}

func (m *Module) liveCaps() (set map[string]bool, ok bool) {
	set = map[string]bool{}
	if m.toolCat != nil {
		if tc := m.toolCat(); tc != nil {
			ok = true
			for _, sp := range tc.Specs() {
				if rawArrangementToolNames[sp.Name] {
					continue // arrangement tools are granted via the bundles below, not individually
				}
				set["tool:use:"+sp.Name] = true
			}
		}
	}
	if m.skillCat != nil {
		if sc := m.skillCat(); sc != nil {
			ok = true
			for _, si := range sc.List() {
				set["skill:use:"+si.Name] = true
			}
		}
	}
	// The arrangement role bundles replace the four individual arrangement tool grants in
	// the matrix (so the admin toggles 2 rows, not 4; the raw 4 are rejected on save). They
	// expand to the tool grants at authorization — see leaf/agora/10-roles.go. Present for
	// rendering the two bundle rows when a catalog resolved (ok); harmless when not.
	set["arrangement:role:creator"] = true
	set["arrangement:role:collaborator"] = true
	return set, ok
}

// rawArrangementToolNames are the arrangement tool names hidden from the permissions matrix —
// granted via the creator/collaborator bundles, never individually.
var rawArrangementToolNames = map[string]bool{
	"arrangement_define": true, "arrangement_inject": true,
	"arrangement_pull": true, "arrangement_submit": true,
}

// renderPermsMatrix serializes a grant matrix back to the editor yaml: a fixed
// header, the roles list, then one aligned grant row per capability (sorted). A
// capability absent from live (a removed tool/skill) gets a trailing "已不可用"
// note so the admin sees it before it's cleaned on save. liveOK must be true (a
// catalog actually resolved) for the annotation to fire — otherwise "not in live"
// only means "catalog unresolved", not "removed", and every real grant would be
// mislabeled.
func renderPermsMatrix(mf matrixFile, live map[string]bool, liveOK bool) string {
	caps := make([]string, 0, len(mf.Grants))
	for c := range mf.Grants {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	width := 0
	for _, c := range caps {
		if len(c)+1 > width {
			width = len(c) + 1
		}
	}
	var b strings.Builder
	b.WriteString("# agora 角色授权矩阵。打开时实时工具/skill 已并入(缺的默认 off);保存时清除已不可用项。\n")
	b.WriteString("# roles: 本公司角色;grants: 每个能力 per role 标 on/off。不支持通配,仅影响本公司。\n\n")
	b.WriteString("roles:\n")
	for _, r := range mf.Roles {
		b.WriteString("  - " + r + "\n")
	}
	b.WriteString("\ngrants:\n")
	for _, c := range caps {
		cell := mf.Grants[c]
		parts := make([]string, 0, len(mf.Roles))
		for _, r := range mf.Roles {
			st := "off"
			if cell != nil {
				if normaliseState(cell[r]) == "on" {
					st = "on"
				}
			}
			parts = append(parts, r+": "+st)
		}
		line := "  " + c + ":"
		for len(line) < 2+width+1 {
			line += " "
		}
		line += "{ " + strings.Join(parts, ", ") + " }"
		if liveOK && prunableCap(c) && !live[c] {
			line += "   # 已不可用（保存时清除）"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// companyStatus maps a Company.Status to a webui node color. A disabled company is
// intentionally off (not failed), so it greys out (the "disabled" muted tone), NOT
// the red "down" reserved for genuine failure.
func companyStatus(s string) string {
	if s == "disabled" {
		return "disabled"
	}
	return "ok"
}

func inboxStatus(s string) string {
	switch s {
	case "active":
		return "ok"
	case "paused":
		return "warn"
	case "archived":
		return "down"
	}
	return ""
}
