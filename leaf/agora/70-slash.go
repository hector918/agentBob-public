// Package agora — /agora slash adapter. Thin chat front for the operation layer
// (60-operations.go): admin-gated subcommand dispatch (mirrors
// leaf/accounts/50-slash.go), i18n/Chinese reply strings, NO business logic.
//
// ESSENTIAL subset only (docs/agora-port.md §6.5 Stage 2c). DEFERRED skeleton
// command families (the other ~40 subcommands): roles list/reconcile, member
// create/list/delete, terminate, inbox create/visibility/
// add-source-by-matcher/unwire, bootstrap (the macro — Bootstrap() op is
// callable, just no slash front yet), session show/messages/kill/finalize,
// dispatch overview/resume/retry, inject, set-reports-to (retired). They land in
// later stages alongside the dispatcher (④b) + webui.
package agora

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/leaf/agora/store"
)

// wireKind is the claim-token kind label for an inbox wire token (facility treats kind as
// opaque — the gate's redeem step type-asserts contract.BatchPayload). wireTokenTTL bounds
// an unredeemed wire token.
const (
	wireKind     = "agora-wire"
	wireTokenTTL = 15 * time.Minute
)

// registerSlash wires /agora into the registry. Registered AdminOnly=false so
// the handler can render a Chinese refusal for non-admins on every subcommand
// (the registry's AdminOnly gate would otherwise reject with a generic message);
// each subcommand checks sc.IsAdmin explicitly.
func (m *Module) registerSlash(reg contract.SlashRegistry) {
	reg.Register(contract.SlashCommand{
		Name:    "agora",
		DescKey: "slash.agora.desc",
		Handler: m.slashAgora,
	})
}

// deps builds the OpDeps the slash handlers share (live cache + router reload +
// the in-flight probe for DeleteCompany). RunningAtScope is wired only when the
// SessionManager is live (lazy/optional); left nil otherwise so DeleteCompany
// fails closed.
func (m *Module) deps() OpDeps {
	d := OpDeps{
		Store:        m.store,
		Org:          m.org,
		RouterReload: func(ctx context.Context) error { return m.router.Reload(ctx) },
	}
	if m.sessionMgr != nil {
		if mgr := m.sessionMgr(); mgr != nil {
			d.RunningAtScope = mgr.RunningAtScope
		}
	}
	return d
}

func (m *Module) slashAgora(ctx context.Context, sc contract.SlashContext) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.agora.admin_only", sc.Lang))
		return errors.New("agora: admin only")
	}
	fields := strings.Fields(sc.Args)
	if len(fields) == 0 {
		// bare /agora → print the identity THIS chat resolves to, then the usage.
		return m.scWhoami(ctx, sc)
	}
	sub, rest := fields[0], fields[1:]
	switch sub {
	case "overview":
		return m.scOverview(ctx, sc)
	case "bootstrap":
		return m.scBootstrap(ctx, sc, rest)
	case "company":
		return m.scCompany(ctx, sc, rest)
	case "hire":
		return m.scHire(ctx, sc, rest)
	case "terminate":
		return m.scTerminate(ctx, sc, rest)
	case "change-role":
		return m.scChangeRole(ctx, sc, rest)
	case "member":
		return m.scMember(ctx, sc, rest)
	case "inbox":
		return m.scInbox(ctx, sc, rest)
	case "prompt":
		return m.scPrompt(ctx, sc, rest)
	case "reload":
		if err := Reload(ctx, m.deps()); err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.reload_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.reload_ok", sc.Lang))
	default:
		_ = sc.Sink.Finish(i18n.T("slash.agora.unknown_sub", sc.Lang, sub) + "\n\n" + i18n.T("slash.agora.usage", sc.Lang))
		return fmt.Errorf("agora: unknown subcommand %q", sub)
	}
}

// scPrompt dumps a NAMED member's LIVE agora context — the read-only diagnostic for "what
// does this worker actually see / why doesn't it see colleague X". For each of the member's
// active employments it resolves the company inbox straight off the OrgCache and renders the
// SAME pieces a turn assembles: identity + directory (renderDirectory), playbook, and the
// authorized tools/skills (AuthorizeTools/Skills). No scope/reply/session-keying games — the
// explicit member is the target. Admin-gated by slashAgora. (For the CURRENT chat's full
// prompt incl. the non-agora layers, use /prompt; this is the per-worker agora view.)
func (m *Module) scPrompt(ctx context.Context, sc contract.SlashContext, args []string) error {
	if len(args) < 1 {
		return sc.Sink.Finish(i18n.T("slash.agora.prompt_usage", sc.Lang))
	}
	name := args[0]
	snap := m.org.Snapshot(ctx, nil)
	memberID := ""
	for _, mb := range snap.Members {
		if mb.Name == name {
			memberID = mb.ID
			break
		}
	}
	if memberID == "" {
		return sc.Sink.Finish(i18n.T("slash.agora.prompt_member_not_found", sc.Lang, name))
	}
	emps := m.org.ActiveEmploymentsByMember(memberID)
	if len(emps) == 0 {
		return sc.Sink.Finish(i18n.T("slash.agora.prompt_no_active_employment", sc.Lang, name))
	}
	var b strings.Builder
	for _, e := range emps {
		scope := MemberOwnedInboxScope(name, e.CompanyID)
		inboxID, ok := m.impl.InboxForScope(ctx, scope)
		if !ok {
			fmt.Fprintf(&b, "【%s】inbox 未解析（scope=%s）—— 该成员公司 inbox 不在缓存或未路由\n\n", e.CompanyID, scope)
			continue
		}
		fmt.Fprintf(&b, "【%s】inbox=%s\n", e.CompanyID, inboxID)
		dir, playbook := m.impl.renderDirectory(ctx, inboxID)
		if strings.TrimSpace(dir) != "" {
			fmt.Fprintf(&b, "\n— 身份/通讯录 —\n%s\n", dir)
		}
		if strings.TrimSpace(playbook) != "" {
			fmt.Fprintf(&b, "\n— playbook —\n%s\n", playbook)
		}
		if m.toolCat != nil {
			if tc := m.toolCat(); tc != nil {
				names := authorizedToolNames(ctx, m.impl, inboxID, tc.Specs())
				fmt.Fprintf(&b, "\n— 工具（%d）— %s\n", len(names), strings.Join(names, ", "))
			}
		}
		if m.skillCat != nil {
			if scat := m.skillCat(); scat != nil {
				names := authorizedSkillNames(ctx, m.impl, inboxID, scat.List())
				fmt.Fprintf(&b, "— 技能（%d）— %s\n", len(names), strings.Join(names, ", "))
			}
		}
		b.WriteString("\n")
	}
	return sc.Sink.Finish(i18n.T("slash.agora.prompt_header", sc.Lang, name, b.String()))
}

// authorizedToolNames / authorizedSkillNames project the full catalog through the inbox's
// (company,role) grants — the same self-authorization a turn does — and return the names.
func authorizedToolNames(ctx context.Context, a *Impl, inboxID string, specs []contract.ToolSpec) []string {
	proj := a.MemberProjection(ctx, inboxID)
	var out []string
	for _, s := range specs {
		if proj.Granted("tool:use:" + s.Name) {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}

func authorizedSkillNames(ctx context.Context, a *Impl, inboxID string, infos []contract.SkillInfo) []string {
	proj := a.MemberProjection(ctx, inboxID)
	var out []string
	for _, si := range infos {
		if proj.Granted("skill:use:" + si.Name) {
			out = append(out, si.Name)
		}
	}
	sort.Strings(out)
	return out
}

// scWhoami prints the agora identity the CURRENT chat resolves to: inbox → member
// → active employments → intersection grants → principal. Bare `/agora` runs it,
// then appends the command usage. One-glance debug for "why are this worker's
// tools/skills wrong": no member ⇒ fail-closed/empty; multi-company ⇒ intersection
// narrows; otherwise it's the company's permissions_yaml for that role.
func (m *Module) scWhoami(ctx context.Context, sc contract.SlashContext) error {
	scope := sc.Event.RoutingScope()
	var b strings.Builder
	fmt.Fprintf(&b, "📍 agora identity\nscope: %s\n", scope)

	ib, ok := m.router.ResolveByScope(scope)
	if !ok {
		b.WriteString("inbox: (this chat is NOT routed to any agora inbox — plain/non-agora)\n")
		return sc.Sink.Finish(b.String() + "\n" + i18n.T("slash.agora.usage", sc.Lang))
	}
	title := ib.Name
	if co, ok := m.org.CompanyByID(ib.Name); ok { // company inbox is named with the company id
		title = co.Name + "#inbox"
	}
	fmt.Fprintf(&b, "inbox: %s %q  status=%s  kind=%s\n", ib.ID, title, ib.Status, ib.Kind)

	switch {
	case ib.MemberID == "":
		b.WriteString("member: (NONE — inbox has no MemberID → grants fail-closed / EMPTY)\n")
	default:
		if mb, ok := m.org.MemberByID(ib.MemberID); ok {
			fmt.Fprintf(&b, "member: %s (%s)\n", mb.Name, mb.ID)
		} else {
			fmt.Fprintf(&b, "member: <%s> (unknown — not in cache)\n", ib.MemberID)
		}
		emps := m.org.ActiveEmploymentsByMember(ib.MemberID)
		if len(emps) == 0 {
			b.WriteString("employments: (none active → grants EMPTY)\n")
		} else {
			b.WriteString("employments:\n")
			for _, e := range emps {
				coName := e.CompanyID
				if co, ok := m.org.CompanyByID(e.CompanyID); ok {
					coName = co.Name
				}
				fmt.Fprintf(&b, "  - %s / %s\n", coName, e.RoleName)
			}
			if len(emps) > 1 {
				fmt.Fprintf(&b, "  (grants = INTERSECTION of these %d companies — net-subtractive)\n", len(emps))
			}
		}
	}

	fmt.Fprintf(&b, "principal: %s\n", m.impl.principalForInbox(ib.ID))

	grants, vi, gok := m.impl.grantsAndVisibility(ib.ID)
	if !gok {
		b.WriteString("grants: (fail-closed — nothing authorized)\n")
	} else {
		gs := make([]string, 0, len(grants))
		for g := range grants {
			gs = append(gs, g)
		}
		sort.Strings(gs)
		if len(gs) == 0 {
			b.WriteString("grants: (EMPTY)\n")
		} else {
			fmt.Fprintf(&b, "grants (%d):\n", len(gs))
			for _, g := range gs {
				fmt.Fprintf(&b, "  - %s\n", g)
			}
		}
		if len(vi.HiddenTools) > 0 || len(vi.HiddenSkills) > 0 {
			fmt.Fprintf(&b, "visibility (hidden): tools=%v skills=%v\n", vi.HiddenTools, vi.HiddenSkills)
		} else {
			b.WriteString("visibility: (nothing hidden)\n")
		}
	}

	return sc.Sink.Finish(b.String() + "\n" + i18n.T("slash.agora.usage", sc.Lang))
}

// ---------- arg parsing ----------

// parseFlags splits "--key=value" tokens out of args into a map and returns the
// remaining positional tokens, so every /agora subcommand accepts either the flag
// form (--company=…) or the legacy positional form.
func parseFlags(args []string) (map[string]string, []string) {
	flags := map[string]string{}
	pos := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			kv := strings.SplitN(strings.TrimPrefix(a, "--"), "=", 2)
			if len(kv) == 2 {
				flags[kv[0]] = kv[1]
			} else {
				flags[kv[0]] = ""
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

// argOr returns the flag value if non-empty, else the i-th positional, else "".
func argOr(flag string, pos []string, i int) string {
	if strings.TrimSpace(flag) != "" {
		return flag
	}
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

// ---------- bootstrap ----------

// scBootstrap founds a company in one shot (company + bridge + founder + employment
// + founder inbox + optional bridge→chat wire), the operator path that makes the
// bridge OUTBOUND path exercisable. The company name is positional OR via the
// --company=<name> flag; other flags: --founder=<name>, --bridge-to-chat=<arg>
// (auto → bind to the current chat; <source>:<dm|group>:<chat> → explicit).
func (m *Module) scBootstrap(ctx context.Context, sc contract.SlashContext, args []string) error {
	var (
		company   string
		founder   string
		bridgeArg string
	)
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--company="):
			company = strings.TrimPrefix(a, "--company=") // named-flag alias for the positional company name
		case strings.HasPrefix(a, "--founder="):
			founder = strings.TrimPrefix(a, "--founder=")
		case strings.HasPrefix(a, "--bridge-to-chat="):
			bridgeArg = strings.TrimPrefix(a, "--bridge-to-chat=")
		case strings.HasPrefix(a, "--"):
			_ = sc.Sink.Finish(i18n.T("slash.agora.bootstrap_unknown_flag", sc.Lang, a) + "\n\n" + i18n.T("slash.agora.bootstrap_usage", sc.Lang))
			return fmt.Errorf("agora: unknown flag %q", a)
		default:
			// Single positional = company name (nameOK forbids spaces, so a
			// multi-token name can never be valid — reject extras up front).
			if company != "" {
				_ = sc.Sink.Finish(i18n.T("slash.agora.bootstrap_usage", sc.Lang))
				return fmt.Errorf("agora: unexpected extra argument %q (company name is a single token)", a)
			}
			company = a
		}
	}
	company = strings.TrimSpace(company)
	if company == "" {
		_ = sc.Sink.Finish(i18n.T("slash.agora.bootstrap_usage", sc.Lang))
		return errors.New("agora: company name required")
	}

	spec := BootstrapSpec{CompanyName: company, FounderName: strings.TrimSpace(founder)}
	if bridgeArg != "" {
		// The slash runs inside a chat, so "auto" can resolve from sc.Event. The
		// gateway is not held by the module → pass nil (reserved/format checks still
		// run; the registered-source check is skipped, like the CLI path).
		ev := sc.Event
		src, ct, chat, err := ParseBridgeToChat(bridgeArg, &ev, nil)
		if err != nil {
			_ = sc.Sink.Finish(err.Error())
			return err
		}
		spec.BridgeSource = src
		spec.BridgeChatType = ct
		spec.BridgeChatID = chat
	}

	res, err := Bootstrap(ctx, m.deps(), spec)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.agora.bootstrap_failed", sc.Lang, err.Error()))
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("slash.agora.bootstrap_ok", sc.Lang)+"\n", res.Company.Name, res.Company.ID)
	fmt.Fprintf(&b, i18n.T("slash.agora.bootstrap_bridge", sc.Lang)+"\n", res.Bridge.ID, res.Bridge.Scope)
	fmt.Fprintf(&b, i18n.T("slash.agora.bootstrap_founder", sc.Lang)+"\n", res.Founder.Name)
	fmt.Fprintf(&b, i18n.T("slash.agora.bootstrap_founder_inbox", sc.Lang)+"\n", res.FounderInbox.ID, res.FounderInbox.Scope)
	if res.InboxSourceID != "" {
		fmt.Fprintf(&b, i18n.T("slash.agora.bootstrap_chat_bound", sc.Lang), res.InboxSourceID)
	} else if bridgeArg != "" {
		b.WriteString(i18n.T("slash.agora.bootstrap_chat_bind_failed", sc.Lang))
	}
	return sc.Sink.Finish(strings.TrimRight(b.String(), "\n"))
}

// ---------- overview ----------

func (m *Module) scOverview(ctx context.Context, sc contract.SlashContext) error {
	snap := m.org.Snapshot(ctx, nil)
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("slash.agora.overview_companies", sc.Lang)+"\n", len(snap.Companies))
	for _, co := range snap.Companies {
		fmt.Fprintf(&b, "  - %s  %s  [%s]\n", co.ID, co.Name, co.Status)
	}
	fmt.Fprintf(&b, i18n.T("slash.agora.overview_members", sc.Lang)+"\n", len(snap.Members))
	for _, mb := range snap.Members {
		fmt.Fprintf(&b, "  - %s  %s\n", mb.ID, mb.Name)
	}
	fmt.Fprintf(&b, i18n.T("slash.agora.overview_inboxes", sc.Lang)+"\n", len(snap.Inboxes))
	for _, ib := range snap.Inboxes {
		fmt.Fprintf(&b, "  - %s  %s  [%s/%s]\n", ib.ID, ib.Scope, ib.Kind, ib.Status)
	}
	// Registered skills (the catalog — what exists to grant). Usability is still
	// per-(company,role) via each company's permissions_yaml; this is the org-wide
	// "what skills do we have" list. Optional/lazy catalog → omit when unavailable.
	if m.skillCat != nil {
		if cat := m.skillCat(); cat != nil {
			skills := cat.List()
			fmt.Fprintf(&b, i18n.T("slash.agora.overview_skills", sc.Lang)+"\n", len(skills))
			for _, si := range skills {
				fmt.Fprintf(&b, "  - %s\n", si.Name)
			}
		}
	}
	return sc.Sink.Finish(strings.TrimRight(b.String(), "\n"))
}

// ---------- company ----------

func (m *Module) scCompany(ctx context.Context, sc contract.SlashContext, args []string) error {
	if len(args) == 0 {
		_ = sc.Sink.Finish(i18n.T("slash.agora.company_usage", sc.Lang))
		return errors.New("agora: company subcommand required")
	}
	switch args[0] {
	case "create":
		flags, pos := parseFlags(args[1:])
		name := strings.TrimSpace(flags["name"])
		if name == "" {
			// Single positional = company name (nameOK forbids spaces, so a
			// multi-token name can never be valid — reject extras up front).
			if len(pos) > 1 {
				_ = sc.Sink.Finish(i18n.T("slash.agora.company_create_usage", sc.Lang))
				return fmt.Errorf("agora: unexpected extra argument %q (company name is a single token)", pos[1])
			}
			if len(pos) == 1 {
				name = pos[0]
			}
		}
		if name == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_create_usage", sc.Lang))
			return errors.New("agora: company name required")
		}
		co, err := CreateCompany(ctx, m.deps(), name)
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_create_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.company_create_ok", sc.Lang, co.ID, co.Name, co.ReportInboxID))
	case "list":
		cos, err := ListCompanies(ctx, m.deps())
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.read_failed", sc.Lang, err.Error()))
			return err
		}
		if len(cos) == 0 {
			return sc.Sink.Finish(i18n.T("slash.agora.company_list_empty", sc.Lang))
		}
		var b strings.Builder
		for _, co := range cos {
			fmt.Fprintf(&b, "%s  %s  [%s]\n", co.ID, co.Name, co.Status)
		}
		return sc.Sink.Finish(strings.TrimRight(b.String(), "\n"))
	case "disable":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_disable_usage", sc.Lang))
			return errors.New("agora: company id required")
		}
		if err := DisableCompany(ctx, m.deps(), args[1]); err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_disable_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.company_disable_ok", sc.Lang, args[1]))
	case "enable":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_enable_usage", sc.Lang))
			return errors.New("agora: company id required")
		}
		if err := EnableCompany(ctx, m.deps(), args[1]); err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_enable_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.company_enable_ok", sc.Lang, args[1]))
	case "delete":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_delete_usage", sc.Lang))
			return errors.New("agora: company id required")
		}
		coID := m.org.CompanyIDByIDOrName(args[1])
		if coID == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_unknown", sc.Lang, args[1]))
			return fmt.Errorf("agora: unknown company %q", args[1])
		}
		// Destructive + irreversible: require an explicit `confirm` token (the
		// disable precondition is enforced inside DeleteCompany, not here).
		if len(args) < 3 || strings.ToLower(args[2]) != "confirm" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_delete_confirm", sc.Lang, args[1], args[1]))
			return errors.New("agora: company delete needs confirm")
		}
		if err := DeleteCompany(ctx, m.deps(), coID); err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_delete_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.company_delete_ok", sc.Lang, args[1]))
	case "pause":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_pause_usage", sc.Lang))
			return errors.New("agora: company id required")
		}
		if _, ok := m.org.CompanyByID(args[1]); !ok {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_unknown", sc.Lang, args[1]))
			return fmt.Errorf("agora: unknown company %q", args[1])
		}
		m.impl.Pause(args[1]) // in-memory: stops the message link (non-destructive)
		return sc.Sink.Finish(i18n.T("slash.agora.company_pause_ok", sc.Lang, args[1]))
	case "resume":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_pause_usage", sc.Lang))
			return errors.New("agora: company id required")
		}
		if _, ok := m.org.CompanyByID(args[1]); !ok {
			_ = sc.Sink.Finish(i18n.T("slash.agora.company_unknown", sc.Lang, args[1]))
			return fmt.Errorf("agora: unknown company %q", args[1])
		}
		m.impl.Resume(args[1])
		return sc.Sink.Finish(i18n.T("slash.agora.company_resume_ok", sc.Lang, args[1]))
	default:
		_ = sc.Sink.Finish(i18n.T("slash.agora.company_unknown_sub", sc.Lang, args[0]))
		return fmt.Errorf("agora: unknown company subcommand %q", args[0])
	}
}

// ---------- hire ----------

func (m *Module) scHire(ctx context.Context, sc contract.SlashContext, args []string) error {
	flags, pos := parseFlags(args)
	company := argOr(flags["company"], pos, 0)
	memberName := argOr(flags["member"], pos, 1)
	role := argOr(flags["role"], pos, 2)
	if company == "" || memberName == "" || role == "" {
		_ = sc.Sink.Finish(i18n.T("slash.agora.hire_usage", sc.Lang))
		return errors.New("agora: hire requires company, member, role")
	}
	member, _, inbox, err := Hire(ctx, m.deps(), company, memberName, role)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.agora.hire_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.agora.hire_ok", sc.Lang,
		member.Name, company, role, inbox.ID, inbox.Scope))
}

// ---------- terminate ----------

// scTerminate ends a member's active employment at a company — the inverse of
// hire. Flags mirror the hire case (--company / --member, with positional
// fallback). It only ends the employment; no inbox is touched.
func (m *Module) scTerminate(ctx context.Context, sc contract.SlashContext, args []string) error {
	flags, pos := parseFlags(args)
	company := argOr(flags["company"], pos, 0)
	member := argOr(flags["member"], pos, 1)
	if company == "" || member == "" {
		_ = sc.Sink.Finish(i18n.T("slash.agora.terminate_usage", sc.Lang))
		return errors.New("agora: terminate requires company + member")
	}
	if err := TerminateEmployment(ctx, m.deps(), company, member); err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.agora.terminate_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.agora.terminate_ok", sc.Lang, member, company))
}

// ---------- change-role ----------

// scChangeRole changes the role of a member's active employment at a company.
// Flags mirror the hire case (--company / --member / --role, with positional
// fallback). It only changes the role; no inbox is touched (the member's grants
// recompute from the new role's permissions_yaml).
func (m *Module) scChangeRole(ctx context.Context, sc contract.SlashContext, args []string) error {
	flags, pos := parseFlags(args)
	company := argOr(flags["company"], pos, 0)
	member := argOr(flags["member"], pos, 1)
	role := argOr(flags["role"], pos, 2)
	if company == "" || member == "" || role == "" {
		_ = sc.Sink.Finish(i18n.T("slash.agora.change_role_usage", sc.Lang))
		return errors.New("agora: change-role requires company + member + role")
	}
	if err := ChangeRole(ctx, m.deps(), company, member, role); err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.agora.change_role_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.agora.change_role_ok", sc.Lang, member, role))
}

// ---------- member ----------

func (m *Module) scMember(ctx context.Context, sc contract.SlashContext, args []string) error {
	if len(args) == 0 || args[0] != "create" {
		_ = sc.Sink.Finish(i18n.T("slash.agora.member_create_usage", sc.Lang))
		return errors.New("agora: member create required")
	}
	flags, pos := parseFlags(args[1:])
	name := strings.TrimSpace(argOr(flags["name"], pos, 0))
	if name == "" {
		_ = sc.Sink.Finish(i18n.T("slash.agora.member_create_usage", sc.Lang))
		return errors.New("agora: member name required")
	}
	mb, err := CreateMember(ctx, m.deps(), name, flags["model-pref"], flags["prompt-style"])
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.agora.member_create_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.agora.member_create_ok", sc.Lang, mb.Name, mb.ID))
}

// ---------- inbox ----------

func (m *Module) scInbox(ctx context.Context, sc contract.SlashContext, args []string) error {
	if len(args) == 0 {
		_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_usage", sc.Lang))
		return errors.New("agora: inbox subcommand required")
	}
	switch args[0] {
	case "add-source":
		flags, pos := parseFlags(args[1:]) // positional order: inbox source chat-type chat-id [thread]
		inboxID := argOr(flags["inbox"], pos, 0)
		if inboxID == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_usage", sc.Lang))
			return errors.New("agora: inbox id required")
		}
		// concise form (the original completion mode): --chat=<source>:<dm|group>:<chat>
		// (or "auto" to bind the current chat) — one field instead of three.
		if chatArg := flags["chat"]; chatArg != "" {
			ev := sc.Event
			src, ct, chat, err := ParseBridgeToChat(chatArg, &ev, nil)
			if err != nil {
				_ = sc.Sink.Finish(err.Error())
				return err
			}
			out, err := AddInboxSource(ctx, m.deps(), inboxID, src, ct, chat, "")
			if err != nil {
				_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_failed", sc.Lang, err.Error()))
				return err
			}
			return sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_ok", sc.Lang, src, string(ct), chat, inboxID, out.ID))
		}
		// explicit form: --source= --chat-type= --chat-id= (or positional).
		sourceID := argOr(flags["source"], pos, 1)
		chatTypeS := argOr(flags["chat-type"], pos, 2)
		chatID := argOr(flags["chat-id"], pos, 3)
		threadID := argOr(flags["thread"], pos, 4)
		if sourceID == "" || chatTypeS == "" || chatID == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_usage", sc.Lang))
			return errors.New("agora: source, chat-type, chat-id required")
		}
		ct, ok := parseChatType(chatTypeS)
		if !ok {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_bad_chat_type", sc.Lang, chatTypeS))
			return fmt.Errorf("agora: bad chat type %q", chatTypeS)
		}
		src, err := AddInboxSource(ctx, m.deps(), inboxID, sourceID, ct, chatID, threadID)
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_ok", sc.Lang,
			sourceID, chatTypeS, chatID, inboxID, src.ID))
	case "list":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_list_usage", sc.Lang))
			return errors.New("agora: company id required")
		}
		ibs, err := ListInboxes(ctx, m.deps(), args[1])
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.read_failed", sc.Lang, err.Error()))
			return err
		}
		if len(ibs) == 0 {
			return sc.Sink.Finish(i18n.T("slash.agora.inbox_list_empty", sc.Lang))
		}
		var b strings.Builder
		for _, ib := range ibs {
			fmt.Fprintf(&b, "%s  %s  [%s/%s]\n", ib.ID, ib.Scope, ib.Kind, ib.Status)
		}
		return sc.Sink.Finish(strings.TrimRight(b.String(), "\n"))
	case "create":
		flags, pos := parseFlags(args[1:])
		member := argOr(flags["member"], pos, 0)
		name := argOr(flags["name"], pos, 1)
		if member == "" || name == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_create_usage", sc.Lang))
			return errors.New("agora: member and name required")
		}
		ib, err := CreateMemberOwnedInbox(ctx, m.deps(), member, name)
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_create_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.inbox_create_ok", sc.Lang, ib.Name, ib.ID, ib.Scope))
	case "wirehere":
		// The redeem action an /agora inbox wire token carries in its batch: bind THIS chat
		// (from the redeemer's event) to the inbox + allowlist the sender. Reached only under
		// slashAgora's top-level admin gate — a raw non-admin can't run it; the wire token
		// froze AsAdmin=true so its holder passes that gate.
		inboxID := ""
		if len(args) > 1 {
			inboxID = args[1]
		}
		if inboxID == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_wire_usage", sc.Lang))
			return errors.New("agora: inbox id required")
		}
		ev := sc.Event
		if _, err := AddInboxSource(ctx, m.deps(), inboxID, ev.Source, ev.ChatType, ev.ChatID, ev.ThreadID); err != nil {
			if errors.Is(err, store.ErrInboxGone) {
				_ = sc.Sink.Finish(i18n.T("agora.wire.gone", sc.Lang))
				return err
			}
			_ = sc.Sink.Finish(i18n.T("agora.wire.retry", sc.Lang))
			return err
		}
		// Auto-allowlist the wiring sender at the redeem chat's scope (group → that chat, DM
		// → source-wide). Best-effort — the chat is wired even if this blips.
		if m.granter != nil {
			if g := m.granter(); g != nil {
				if ev.ChatType.IsGroupChat() {
					_, _ = g.AllowInChat(ctx, ev.Source, ev.ChatID, ev.UserID)
				} else {
					_, _ = g.Allow(ctx, ev.Source, ev.UserID)
				}
			}
		}
		return sc.Sink.Finish(i18n.T("agora.wire.ok", sc.Lang))
	case "wire":
		flags, pos := parseFlags(args[1:])
		inboxID := argOr(flags["inbox"], pos, 0)
		if inboxID == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_wire_usage", sc.Lang))
			return errors.New("agora: inbox id required")
		}
		if _, ok := m.org.InboxByID(inboxID); !ok {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_unknown", sc.Lang, inboxID))
			return fmt.Errorf("agora: unknown inbox %q", inboxID)
		}
		// Optional second positional = a chat scope string: bind that chat to the
		// inbox directly (no token). NO scope arg → keep today's one-time-token flow.
		if scopeArg := argOr(flags["scope"], pos, 1); scopeArg != "" {
			src, ct, chatID, threadID, ok := parseWireScope(scopeArg)
			if !ok {
				_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_bad_scope", sc.Lang))
				return fmt.Errorf("agora: bad scope %q", scopeArg)
			}
			// Reuse the same op as add-source (AddInboxSource + router reload); do NOT
			// re-implement the store insert. wire-by-scope only adds the inbox_source —
			// the admin allowlists the chat separately at the gate.
			if _, err := AddInboxSource(ctx, m.deps(), inboxID, src, ct, chatID, threadID); err != nil {
				_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_add_source_failed", sc.Lang, err.Error()))
				return err
			}
			return sc.Sink.Finish(i18n.T("slash.agora.inbox_wired", sc.Lang, inboxID, scopeArg))
		}
		// The token is a batch carrying the wirehere command; redeemed in the target chat,
		// it binds THAT chat + allowlists the sender. AsAdmin=true: the /agora subcommands are
		// admin-gated, so the token's frozen authority is what lets an operator run it.
		tok := m.tokens.Mint(wireKind, contract.BatchPayload{
			Commands: []string{"/agora inbox wirehere " + inboxID},
			AsAdmin:  true,
		}, wireTokenTTL)
		return sc.Sink.Finish(i18n.T("slash.agora.inbox_wire_token", sc.Lang, tok))
	case "unwire":
		flags, pos := parseFlags(args[1:])
		inboxID := argOr(flags["inbox"], pos, 0)
		scope := argOr(flags["scope"], pos, 1)
		if inboxID == "" || scope == "" {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_unwire_usage", sc.Lang))
			return errors.New("agora: inbox id and scope required")
		}
		if _, ok := m.org.InboxByID(inboxID); !ok {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_unknown", sc.Lang, inboxID))
			return fmt.Errorf("agora: unknown inbox %q", inboxID)
		}
		if err := RemoveInboxSource(ctx, m.deps(), inboxID, scope); err != nil {
			if errors.Is(err, contract.ErrNoRows) {
				_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_no_binding", sc.Lang, scope, inboxID))
				return err
			}
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_unwire_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.inbox_unwired", sc.Lang, inboxID, scope))
	case "pause":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_pause_usage", sc.Lang))
			return errors.New("agora: inbox id required")
		}
		if err := PauseInbox(ctx, m.deps(), args[1]); err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_pause_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.inbox_pause_ok", sc.Lang, args[1]))
	case "resume":
		if len(args) < 2 {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_resume_usage", sc.Lang))
			return errors.New("agora: inbox id required")
		}
		if err := ResumeInbox(ctx, m.deps(), args[1]); err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_resume_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.agora.inbox_resume_ok", sc.Lang, args[1]))
	default:
		_ = sc.Sink.Finish(i18n.T("slash.agora.inbox_unknown_sub", sc.Lang, args[0]))
		return fmt.Errorf("agora: unknown inbox subcommand %q", args[0])
	}
}

// parseWireScope parses a chat scope string (the inverse of contract.ScopeFor)
// into its inbox_source coordinates. Format: "<source>:dm:<chatID>[#<thread>]" or
// "<source>:group:<chatID>". It locates the ":dm:" or ":group:" marker → the part
// before it is the source, the part after is the chatID (a trailing "#thread" is
// split off as ThreadID for the dm form). Returns ok=false if neither marker is
// present.
func parseWireScope(scope string) (source string, ct contract.ChatType, chatID, threadID string, ok bool) {
	if i := strings.Index(scope, ":dm:"); i >= 0 {
		source = scope[:i]
		chatID = scope[i+len(":dm:"):]
		if h := strings.Index(chatID, "#"); h >= 0 {
			threadID = chatID[h+1:]
			chatID = chatID[:h]
		}
		return source, contract.ChatDM, chatID, threadID, true
	}
	if i := strings.Index(scope, ":group:"); i >= 0 {
		return scope[:i], contract.ChatGroup, scope[i+len(":group:"):], "", true
	}
	return "", "", "", "", false
}

// parseChatType maps the slash arg ("dm"|"group") to a contract.ChatType.
func parseChatType(s string) (contract.ChatType, bool) {
	switch strings.ToLower(s) {
	case "dm":
		return contract.ChatDM, true
	case "group":
		return contract.ChatGroup, true
	default:
		return "", false
	}
}
