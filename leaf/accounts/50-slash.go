package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/leaf/accounts/store"
)

// slashAccounts handles /accounts <subcommand>. Registered non-admin so the open
// "new" path works; every other subcommand checks sc.IsAdmin inside. Code-minting
// subcommands are DM-only (a group reply would leak the code).
func (m *Manager) slashAccounts(ctx context.Context, sc contract.SlashContext) error {
	fields := strings.Fields(sc.Args)
	if len(fields) == 0 {
		// Bare `/accounts` answers "who am I here" BEFORE the usage line. Every other
		// subcommand needs an account id, and until now nothing told you your own:
		// `show` demands one, `list` is admin-only and prints the whole table. Mirrors
		// bare `/agora`, which prints the identity this chat resolves to.
		return sc.Sink.Finish(m.selfLine(ctx, sc) + i18n.T("slash.accounts.usage", sc.Lang))
	}
	sub, rest := fields[0], fields[1:]
	switch sub {
	case "new":
		return m.cmdNew(ctx, sc, rest)
	case "list":
		return m.cmdList(ctx, sc)
	case "show":
		return m.cmdShow(ctx, sc, rest)
	case "mint":
		return m.cmdMint(ctx, sc, rest)
	case "flow":
		return m.cmdFlow(ctx, sc, rest)
	case "approve":
		return m.cmdApprove(ctx, sc, rest)
	case "bindto":
		return m.cmdBindTo(ctx, sc, rest)
	case "pause":
		return m.cmdStatus(ctx, sc, rest, "paused")
	case "resume":
		return m.cmdStatus(ctx, sc, rest, "active")
	case "apikey":
		return m.cmdAPIKey(ctx, sc, rest)
	default:
		_ = sc.Sink.Finish(i18n.T("slash.accounts.unknown_sub", sc.Lang, sub))
		return fmt.Errorf("accounts: unknown subcommand %q", sub)
	}
}

// cmdNew: non-admin creates + binds self; admin creates empty (or --me to bind).
func (m *Manager) cmdNew(ctx context.Context, sc contract.SlashContext, args []string) error {
	// Default: bind the new account to the sender's current channel. A first-time
	// sender — admin or not — with no account here always gets bound. Only an admin
	// who ALREADY has an account here defaults to an empty account instead (the
	// "admin creates an account for someone/something else" path). Flags override.
	bindSelf := !sc.IsAdmin
	if sc.IsAdmin {
		src, uid := sc.Event.AccountHandle()
		_, ok, err := m.AccountForHandle(ctx, src, uid)
		if err != nil {
			// Same posture as slashBind: a transient handle-read blip must fail the
			// command, not silently fall through to the empty-account path (which would
			// mint an unbound account for an admin who actually has one here).
			_ = sc.Sink.Finish(i18n.T("slash.accounts.read_failed", sc.Lang, err.Error()))
			return err
		}
		if !ok {
			bindSelf = true // admin has no account on this handle → bind like a first-timer
		}
	}
	var nameParts []string
	for _, a := range args {
		switch a {
		case "--me":
			bindSelf = true
		case "--empty":
			// Empty (unbound) accounts are admin-only: /accounts is registered non-admin
			// for the self-bind path, so without this a non-admin could mint unbounded
			// flow="normal" rows via --empty (no sweep reclaims handle-less accounts).
			// Reject loudly rather than silently ignoring the flag.
			if !sc.IsAdmin {
				_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
				return errors.New("accounts: --empty is admin-only")
			}
			bindSelf = false
		default:
			nameParts = append(nameParts, a)
		}
	}
	name := strings.TrimSpace(strings.Join(nameParts, " "))
	if name == "" {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.new_usage", sc.Lang))
		return errors.New("accounts: account name required")
	}
	if bindSelf {
		a, err := m.CreateAndBindSelf(ctx, sc.Event, name)
		if errors.Is(err, ErrHandleBoundElsewhere) {
			_ = sc.Sink.Finish(i18n.T("slash.accounts.already_bound", sc.Lang))
			return err
		}
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.accounts.create_failed", sc.Lang, err.Error()))
			return err
		}
		return sc.Sink.Finish(i18n.T("slash.accounts.created_bound", sc.Lang, a.ID, a.DisplayName))
	}
	a, err := m.store.CreateAccount(ctx, name, "", nowUnix())
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.create_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.created_empty", sc.Lang, a.ID, a.DisplayName, a.ID))
}

func (m *Manager) cmdList(ctx context.Context, sc contract.SlashContext) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: admin required")
	}
	accts, err := m.store.ListAccounts(ctx)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.read_failed", sc.Lang, err.Error()))
		return err
	}
	if len(accts) == 0 {
		return sc.Sink.Finish(i18n.T("slash.accounts.list_empty", sc.Lang))
	}
	// One aggregate count query for all rows — not a per-account HandlesForAccount fan-out.
	counts, cerr := m.store.HandleCountsByAccount(ctx)
	var b strings.Builder
	for _, a := range accts {
		b.WriteString(i18n.T("slash.accounts.list_row", sc.Lang, a.ID, a.DisplayName, a.Flow, counts[a.ID]))
	}
	out := strings.TrimRight(b.String(), "\n")
	if cerr != nil { // don't let a transient store error render as a confident "0 channels"
		out += "\n" + i18n.T("slash.accounts.enrich_failed", sc.Lang)
	}
	return sc.Sink.Finish(out)
}

// selfLine renders "this channel → which account" for the bare `/accounts` reply.
// It ALWAYS returns a line: the whole point is to answer the question, and staying
// silent on a store blip would read as "you have no account here". The three
// outcomes stay distinct for that reason — unbound, unreadable, and bound-but-the
// -account-row-would-not-load are different facts.
func (m *Manager) selfLine(ctx context.Context, sc contract.SlashContext) string {
	src, uid := sc.Event.AccountHandle()
	id, ok, err := m.AccountForHandle(ctx, src, uid)
	switch {
	case err != nil:
		return i18n.T("slash.accounts.self_unknown", sc.Lang) + "\n"
	case !ok:
		return i18n.T("slash.accounts.self_none", sc.Lang) + "\n"
	}
	a, gerr := m.store.GetAccount(ctx, id)
	if gerr != nil {
		return i18n.T("slash.accounts.self_id_only", sc.Lang, id) + "\n"
	}
	return i18n.T("slash.accounts.self", sc.Lang, a.ID, a.DisplayName, a.Flow, statusText(a.Status)) + "\n"
}

// cmdShow details one account. With NO id it shows the caller's own — that is the
// question a person actually arrives with, and it needs no admin rights because the
// answer is their own row. Naming someone else's id stays admin-only.
func (m *Manager) cmdShow(ctx context.Context, sc contract.SlashContext, args []string) error {
	var id string
	if len(args) > 0 {
		id = args[0]
	}
	// The caller's own account is needed in both directions: with no id it IS the
	// answer, and with one it decides whether a non-admin may look — at itself, yes,
	// by id or not. Refusing someone their own row just for naming it would be a
	// distinction nobody could guess. An admin naming an id skips the read entirely.
	if id == "" || !sc.IsAdmin {
		src, uid := sc.Event.AccountHandle()
		self, ok, err := m.AccountForHandle(ctx, src, uid)
		if err != nil {
			_ = sc.Sink.Finish(i18n.T("slash.accounts.read_failed", sc.Lang, err.Error()))
			return err
		}
		switch {
		case id == "" && !ok:
			_ = sc.Sink.Finish(i18n.T("slash.accounts.self_none", sc.Lang))
			return errors.New("accounts: no account bound on this handle")
		case id == "":
			id = self
		case !ok || id != self:
			_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
			return errors.New("accounts: admin required")
		}
	}
	a, err := m.store.GetAccount(ctx, id)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.not_found", sc.Lang, id))
		return err
	}
	hs, herr := m.store.HandlesForAccount(ctx, id)
	u, uerr := m.store.AccountUsageBreakdown(ctx, id)
	var b strings.Builder
	b.WriteString(i18n.T("slash.accounts.show_header", sc.Lang, a.ID, a.DisplayName, a.Flow))
	// Status is the axis ORTHOGONAL to flow (F42): a paused account stays suspended at the
	// router regardless of its granted flow, so surface it here — its invisibility is what
	// made /accounts approve on a paused account look like a no-op.
	b.WriteString(i18n.T("slash.accounts.show_status", sc.Lang, statusText(a.Status)))
	b.WriteString(i18n.T("slash.accounts.show_channels", sc.Lang, len(hs)))
	for _, h := range hs {
		b.WriteString(i18n.T("slash.accounts.show_channel_row", sc.Lang, h.Source, h.PlatformUID, h.BoundSource))
	}
	// Turn count isn't recorded (ReportConsumption is per model-call, not per turn),
	// so show only the per-kind token breakdown rather than a misleading "0 轮".
	b.WriteString(i18n.T("slash.accounts.show_usage_label", sc.Lang))
	first := true
	for _, kind := range slices.Sorted(maps.Keys(u.TokensByKind)) {
		kt := u.TokensByKind[kind]
		if !first {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%s %d/%d", kind, kt.In, kt.Out)
		first = false
	}
	if first {
		b.WriteString(i18n.T("slash.accounts.show_usage_none", sc.Lang))
	}
	if herr != nil || uerr != nil { // a transient store error must not read as "0 channels / no usage"
		b.WriteString("\n" + i18n.T("slash.accounts.enrich_failed", sc.Lang))
	}
	return sc.Sink.Finish(b.String())
}

func (m *Manager) cmdMint(ctx context.Context, sc contract.SlashContext, args []string) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: admin required")
	}
	if sc.Event.ChatType != contract.ChatDM {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.mint_dm_only", sc.Lang))
		return errors.New("accounts: mint is DM-only")
	}
	if len(args) == 0 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.mint_usage", sc.Lang))
		return errors.New("accounts: mint requires an account id")
	}
	code, err := m.MintForAdmin(ctx, args[0])
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.mint_failed", sc.Lang, args[0]))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.mint_code", sc.Lang, code))
}

// cmdBindTo binds the sender's current handle to <account-id> + auto-allowlists them —
// the redeem action an /accounts mint or /bindaccount token carries in its batch. It is
// admin-gated (sc.IsAdmin): the token that legitimately invokes it froze AsAdmin=true, so
// the recipient runs it under that vouched authority, while a raw non-admin typing it
// cannot self-bind to an arbitrary account. A trailing "repoint" arg (present in an
// admin-minted /accounts mint token) rebinds a handle already on another account.
func (m *Manager) cmdBindTo(ctx context.Context, sc contract.SlashContext, args []string) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: bindto requires admin/token authority")
	}
	if len(args) < 1 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.bindto_usage", sc.Lang))
		return errors.New("accounts: bindto <account-id> required")
	}
	repoint := slices.Contains(args[1:], "repoint")
	allowlisted, err := m.BindFromCode(ctx, sc.Event, args[0], repoint)
	if err != nil {
		switch {
		case errors.Is(err, ErrHandleBoundElsewhere):
			_ = sc.Sink.Finish(i18n.T("slash.accounts.redeem_handle_bound_elsewhere", sc.Lang))
		case errors.Is(err, store.ErrAccountNotFound):
			_ = sc.Sink.Finish(i18n.T("slash.accounts.redeem_account_not_found", sc.Lang))
		default:
			// Kept for retry (the token isn't burned on a failed batch) — bindto is idempotent.
			slog.Warn("accounts: bindto failed (kept for token retry)", "account", args[0], "err", err)
			_ = sc.Sink.Finish(i18n.T("slash.accounts.redeem_transient_retry", sc.Lang))
		}
		return err
	}
	if !allowlisted {
		// Bound, but auto-allowlist didn't take (no granter / write blip) — honest, not "ok".
		return sc.Sink.Finish(i18n.T("slash.accounts.redeem_bound_pending", sc.Lang))
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.redeem_bound_ok", sc.Lang))
}

// cmdFlow sets the per-account flow assignment. Admin-only — ordinary users
// cannot change which flow handles an account.
func (m *Manager) cmdFlow(ctx context.Context, sc contract.SlashContext, args []string) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: admin required")
	}
	if len(args) < 2 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.flow_usage", sc.Lang))
		return errors.New("accounts: flow <id> <flows> required")
	}
	// Rejoin the remaining tokens with "," then normalize through the shared parse rule
	// (contract.EntitledFlows) — args came from strings.Fields, so "normal, agora",
	// "normal agora" and "normal,agora" all end up as the same de-duped comma-list.
	// Flow names are space-free identifiers, so treating whitespace as a separator is safe.
	flows := strings.Join(contract.EntitledFlows(strings.Join(args[1:], ",")), ",")
	if flows == "none" {
		flows = "" // explicit clear: revoke all entitlement (the panel's disable-last-flow sentinel)
	}
	// Hold bindMu across the write so this blind whole-column set can't interleave with
	// Approve's read-then-write grant (lost update) — bindMu is the single serialization
	// point for flow writes.
	m.bindMu.Lock()
	err := m.store.SetAccountFlow(ctx, args[0], flows, nowUnix())
	m.bindMu.Unlock()
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.flow_failed", sc.Lang, args[0]))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.flow_set", sc.Lang, args[0], flows))
}

// cmdApprove grants a pending (bare) account access in ONE action — normal, plus agora
// when its onboard scope is an agora scope, plus an allowlist roster entry. Admin-only;
// the one-click counterpart to manually composing /accounts flow + /gate allow.
func (m *Manager) cmdApprove(ctx context.Context, sc contract.SlashContext, args []string) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: admin required")
	}
	if len(args) < 1 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.approve_usage", sc.Lang))
		return errors.New("accounts: approve <id> required")
	}
	flows, err := m.Approve(ctx, args[0])
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.approve_failed", sc.Lang, args[0]))
		return err
	}
	reply := i18n.T("slash.accounts.approved", sc.Lang, args[0], strings.Join(flows, ","))
	// F42: approve grants FLOW, which is orthogonal to STATUS — a paused account stays
	// suspended at the router until /accounts resume, so a bare "✅ Approved" would
	// mislead the admin into thinking the block is lifted. Append the real next step
	// (never auto-resume: that would silently override another admin's deliberate pause).
	if a, gerr := m.store.GetAccount(ctx, args[0]); gerr == nil && a.Status == "paused" {
		reply += "\n" + i18n.T("slash.accounts.approved_still_paused", sc.Lang, args[0])
	}
	return sc.Sink.Finish(reply)
}

// cmdStatus sets an account's status ("active"/"paused") — the /accounts pause|resume
// verbs. Admin-only. A paused account's access falls back to the intro flow (a "suspended"
// notice); resume restores normal routing.
func (m *Manager) cmdStatus(ctx context.Context, sc contract.SlashContext, args []string, status string) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: admin required")
	}
	if len(args) < 1 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.status_usage", sc.Lang))
		return errors.New("accounts: pause/resume <id> required")
	}
	if err := m.store.SetAccountStatus(ctx, args[0], status, nowUnix()); err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.status_failed", sc.Lang, args[0]))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.status_set", sc.Lang, args[0], status))
}

// cmdAPIKey handles /accounts apikey <new|list|revoke>. Admin-only (keys grant
// billed model access). `new` is additionally DM-only — its reply carries the
// plaintext token, shown exactly once.
func (m *Manager) cmdAPIKey(ctx context.Context, sc contract.SlashContext, args []string) error {
	if !sc.IsAdmin {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.admin_required", sc.Lang))
		return errors.New("accounts: apikey requires admin")
	}
	if len(args) == 0 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_usage", sc.Lang))
		return errors.New("accounts: apikey missing action")
	}
	switch args[0] {
	case "new":
		return m.cmdAPIKeyNew(ctx, sc, args[1:])
	case "list":
		return m.cmdAPIKeyList(ctx, sc, args[1:])
	case "revoke":
		return m.cmdAPIKeyRevoke(ctx, sc, args[1:])
	default:
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_usage", sc.Lang))
		return fmt.Errorf("accounts: unknown apikey action %q", args[0])
	}
}

// cmdAPIKeyNew mints a key: /accounts apikey new <account-id>
// [kinds=llm,translate | models=x,y] [label words...]. DM-only (the token is a
// secret). The two forms are exclusive: kinds= admits the key to those LANES (each
// request then names the one it wants and may add soft tag preferences), models=
// pins it to exact entry names. Unspecified policy takes the documented default
// kinds=llm — a bare `new <account-id>` mints a plain chat key.
func (m *Manager) cmdAPIKeyNew(ctx context.Context, sc contract.SlashContext, args []string) error {
	if sc.Event.ChatType != contract.ChatDM {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_dm_only", sc.Lang))
		return errors.New("accounts: apikey new is DM-only")
	}
	if len(args) < 1 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_new_usage", sc.Lang))
		return errors.New("accounts: apikey new <account-id> required")
	}
	accountID := args[0]
	var kinds, models, labelParts []string
	// Track whether a policy flag was WRITTEN, not just whether it parsed to
	// something: `kinds=` with an empty value (an unset shell variable) must not be
	// mistaken for "no policy given" and silently widened to the default lane.
	allowGiven := false
	// Anything unrecognised used to fall through to the LABEL, which turned every
	// near-miss into a silently wrong policy: `kind=image` (no s), `kinds =image`,
	// and `kinds=llm, image` (a space after the comma — strings.Fields cuts the list
	// in two) all minted a plain llm key wearing the policy as its label. The mistake
	// then surfaced much later as a 403 pointing nowhere near its cause. So a token
	// that looks like it was MEANT as a flag is refused instead of worn: a trailing
	// comma, a stray `=`, or a bare flag name. Labels stay free text otherwise.
	badFlag := func(a string) error {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_bad_flag", sc.Lang, a))
		return fmt.Errorf("accounts: apikey malformed option %q", a)
	}
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "kinds="), strings.HasPrefix(a, "models="):
			if strings.HasSuffix(a, ",") {
				return badFlag(a) // the rest of the list was cut off by a space
			}
			if strings.HasPrefix(a, "kinds=") {
				allowGiven = true
				kinds = splitCSVArg(strings.ToLower(strings.TrimPrefix(a, "kinds=")))
			} else {
				allowGiven = true
				models = splitCSVArg(strings.TrimPrefix(a, "models="))
			}
		// A leading comma is the mirror image of the trailing one — `kinds=llm ,image`
		// arrives as two tokens the same way. What is NOT caught is a list written with
		// spaces and no commas at all (`kinds=llm image`): that token is indistinguishable
		// from a label, so the reply's policy line stays the last line of defence.
		case strings.ContainsRune(a, '='), strings.HasPrefix(a, ","), a == "kinds", a == "models":
			return badFlag(a)
		default:
			labelParts = append(labelParts, a)
		}
	}
	if len(models) > 0 && len(kinds) > 0 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_new_conflict", sc.Lang))
		return errors.New("accounts: apikey takes kinds= OR models=, not both")
	}
	// A policy flag written but empty is a mistake, not a default request.
	if allowGiven && len(models) == 0 && len(kinds) == 0 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_empty_allow", sc.Lang))
		return errors.New("accounts: apikey kinds=/models= given but empty")
	}
	// A typo'd lane would mint a key that can never match any entry — say so instead
	// of handing over a dead key.
	for _, k := range kinds {
		if !slices.Contains(apiKeyKinds, k) {
			_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_bad_kind", sc.Lang, k, strings.Join(apiKeyKinds, ", ")))
			return fmt.Errorf("accounts: apikey unknown kind %q", k)
		}
	}
	// Nothing given → the documented default lane. Explicit rather than resolved at
	// request time, so `apikey list` shows what the key actually reaches.
	if !allowGiven {
		kinds = []string{contract.KindLLM}
	}
	token, key, err := m.MintAPIKey(ctx, accountID, strings.Join(labelParts, " "),
		APIKeyPolicy{Kinds: kinds, Models: models})
	if err != nil {
		if errors.Is(err, store.ErrAccountNotFound) {
			_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_no_account", sc.Lang, accountID))
			return err
		}
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_mint_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.apikey_minted", sc.Lang, key.ID, apiKeyAllowText(key), token))
}

// cmdAPIKeyList lists keys for an account, or ALL keys when no id is given.
func (m *Manager) cmdAPIKeyList(ctx context.Context, sc contract.SlashContext, args []string) error {
	accountID := ""
	if len(args) > 0 {
		accountID = args[0]
	}
	keys, err := m.ListAPIKeys(ctx, accountID)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_list_failed", sc.Lang, err.Error()))
		return err
	}
	if len(keys) == 0 {
		return sc.Sink.Finish(i18n.T("slash.accounts.apikey_list_empty", sc.Lang))
	}
	var b strings.Builder
	b.WriteString(i18n.T("slash.accounts.apikey_list_header", sc.Lang))
	for _, k := range keys {
		status := k.Status
		if status == "" {
			status = "active"
		}
		fmt.Fprintf(&b, "\n%s · %s · %s · %s", k.ID, k.AccountID, status, apiKeyAllowText(k))
	}
	return sc.Sink.Finish(b.String())
}

// cmdAPIKeyRevoke flips a key to revoked.
func (m *Manager) cmdAPIKeyRevoke(ctx context.Context, sc contract.SlashContext, args []string) error {
	if len(args) < 1 {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_revoke_usage", sc.Lang))
		return errors.New("accounts: apikey revoke <key-id> required")
	}
	if err := m.RevokeAPIKey(ctx, args[0]); err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_no_key", sc.Lang, args[0]))
			return err
		}
		_ = sc.Sink.Finish(i18n.T("slash.accounts.apikey_revoke_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.apikey_revoked", sc.Lang, args[0]))
}

// splitCSVArg parses a "a,b,c" flag value into a slice (blanks dropped).
func splitCSVArg(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// apiKeyKinds are the pool entry kinds ("lanes") a key may be admitted into. Listed
// here (not derived) so the slash reply can name the valid set; the values are the
// contract's.
//
// KindImage admits the /v1/images/* endpoints rather than /v1/chat/completions —
// image generation is not chat-shaped. It is a lane like any other here: gating it
// differently would mean a second policy vocabulary for one endpoint pair, and
// modelgate already refuses an image request from a key without this lane.
var apiKeyKinds = []string{contract.KindLLM, contract.KindASR, contract.KindTranslate, contract.KindOCR, contract.KindImage}

// apiKeyAllowText renders a key's policy for a listing line — the form it carries.
// A key with neither reaches nothing (a pre-v8 row reads as "(none)", which is
// exactly what it now is).
func apiKeyAllowText(k store.APIKey) string {
	switch {
	case len(k.Models) > 0:
		return "models=" + strings.Join(k.Models, ",")
	case len(k.Kinds) > 0:
		return "kinds=" + strings.Join(k.Kinds, ",")
	default:
		return "(none)"
	}
}

// slashBind handles /bindaccount: a bound user self-mints a code to bind another
// of their channels. DM-only (the reply carries the code).
func (m *Manager) slashBind(ctx context.Context, sc contract.SlashContext) error {
	if sc.Event.ChatType != contract.ChatDM {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.bind_dm_only", sc.Lang))
		return errors.New("accounts: bind is DM-only")
	}
	src, uid := sc.Event.AccountHandle()
	accountID, ok, err := m.AccountForHandle(ctx, src, uid)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.read_failed", sc.Lang, err.Error()))
		return err
	}
	if !ok {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.bind_no_account", sc.Lang))
		return errors.New("accounts: no account bound on this handle")
	}
	code, err := m.MintForSelf(ctx, accountID)
	if err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.accounts.create_failed", sc.Lang, err.Error()))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.accounts.bind_code", sc.Lang, code))
}
