package arrangement

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/arrangement/store"
)

// Define validates a spec JSON and stores a new arrangement as "started" (frozen).
// Cross-company is intentional — the gate is the arrangement_define grant, not company
// membership (docs/arrangement.md). Editing a running arrangement is not allowed; to
// change it, Cancel and Define a new one.
func (m *Module) Define(ctx context.Context, req contract.ArrangeDefine) (string, error) {
	if strings.TrimSpace(req.Company) == "" {
		return "", fmt.Errorf("必须指定 company")
	}
	sp, err := parseSpec(req.Spec)
	if err != nil {
		return "", err // structural validation gate
	}
	// Permission gate (create-time, fail-fast): every bucket role must be able to PROCESS
	// its bucket (collaborator = arrangement_pull + arrangement_submit). A bucket whose role
	// can't pull/submit would strand items — the runaway-loop root cause — so reject up front
	// instead of opening a doomed arrangement.
	for _, b := range sp.Buckets {
		ok, perr := m.roleCanProcess(ctx, req.Company, b.Role)
		if perr != nil {
			return "", perr
		}
		if !ok {
			return "", fmt.Errorf("角色 %q 无法处理其环节:缺协作权限(arrangement_pull + arrangement_submit)。请管理员在公司 %q 的权限里给该角色授予后再创建。", b.Role, req.Company)
		}
	}
	// Per-company RUNNING cap: count started defs that hold a runnable (queued/in_flight)
	// item, NOT every started def — a def whose only items are parked (blocked/unmet/
	// rejected_no_upstream) consumes no worker capacity and must not occupy a cap slot
	// (it never auto-completes, so counting it would eventually wedge new Define). (L-AG-D1)
	// Best-effort count-then-insert: two simultaneous Defines could both pass and exceed
	// the cap by one — acceptable for a low-frequency admin action; cancel one to free a slot.
	cap := m.maxConcurrentArrangements.Load()
	n, err := m.store.CountRunningDefs(ctx, req.Company)
	if err != nil {
		return "", err
	}
	if int64(n) >= cap {
		return "", fmt.Errorf("公司 %q 已有 %d 条编排在运行,达到并发上限 %d(取消一条,或用 /arrangement maxconcurrent_for_arrangement 调整)", req.Company, n, cap)
	}
	now := nowUnix()
	id, err := m.store.InsertDef(ctx, req.Company, req.CallerScope, "started", req.Spec, now)
	if err != nil {
		return "", err
	}
	// Auto-kickoff (排了就开跑): drop one work item at the entry stage so DEFINING = it
	// runs — the stage's content IS the instruction. Fan-out / extra feeds still use
	// Inject. Atomic with the def: if the kickoff insert fails, roll the def back so a
	// failed Define leaves no orphan 'started' zero-item def (it would linger forever as
	// a phantom started def — never running, never completing, and never pruned, since
	// PruneTerminalDefs only touches terminal statuses).
	if _, ierr := m.store.InsertItem(ctx, id, sp.entryRole(), 0, "", now); ierr != nil {
		if derr := m.store.DeleteDef(ctx, id); derr != nil {
			slog.Warn("arrangement: kickoff insert failed AND def rollback failed — orphan def left", "id", id, "err", derr)
		}
		return "", fmt.Errorf("编排创建失败:开跑任务无法写入:%w", ierr)
	}
	return id, nil
}

// Inject drops a new work item onto a started arrangement's entry bucket.
func (m *Module) Inject(ctx context.Context, req contract.ArrangeInject) (string, error) {
	d, err := m.store.GetDef(ctx, req.ArrangementID)
	if err != nil {
		return "", fmt.Errorf("找不到编排 %q：%w", req.ArrangementID, err)
	}
	if d.Status != "started" {
		return "", fmt.Errorf("编排 %q 不在运行中(status=%s),无法投放", req.ArrangementID, d.Status)
	}
	sp, err := parseSpec(d.Spec)
	if err != nil {
		return "", fmt.Errorf("编排 spec 损坏：%w", err)
	}
	stage := sp.entryRole()
	if s := strings.TrimSpace(req.Stage); s != "" { // fan-out target (else entry)
		if sp.indexOf(s) < 0 {
			return "", fmt.Errorf("编排 %q 没有 %q 这个环节", req.ArrangementID, s)
		}
		stage = s
	}
	// Same create-time gate as Define (the second door): don't inject into a stage whose
	// role can't process it.
	if ok, perr := m.roleCanProcess(ctx, d.Company, stage); perr != nil {
		return "", perr
	} else if !ok {
		return "", fmt.Errorf("环节 %q 的角色无法处理(缺 arrangement_pull + arrangement_submit),不投放。", stage)
	}
	return m.store.InsertItem(ctx, d.ID, stage, req.Priority, req.Payload, nowUnix())
}

// roleCanProcess reports whether (company, role) can PROCESS a bucket — i.e. is a
// "collaborator" with BOTH arrangement_pull and arrangement_submit. fail-closed: agora
// unavailable → error (can't verify → don't open). The arrangement model is
// pull→work→submit, so a bucket role lacking either would strand its queue.
func (m *Module) roleCanProcess(ctx context.Context, company, role string) (bool, error) {
	if m.agora == nil {
		return false, fmt.Errorf("无法校验角色权限:agora 未配置")
	}
	ag := m.agora()
	if ag == nil {
		return false, fmt.Errorf("无法校验角色权限:agora 未就绪")
	}
	// agora SUPPLIES the (company,role) projection; the membership check is the same
	// unified one used everywhere (proj.Granted) — no special RoleHasGrant. docs §14.
	proj := ag.RoleProjection(ctx, company, role)
	return proj.Granted("tool:use:arrangement_pull") &&
		proj.Granted("tool:use:arrangement_submit"), nil
}

// Pull claims the top queued item of (arrangement, role) for the caller, after checking
// the caller is an active member of that role.
func (m *Module) Pull(ctx context.Context, req contract.ArrangePull) (contract.ArrangePulled, bool, error) {
	d, err := m.store.GetDef(ctx, req.ArrangementID)
	if err != nil {
		return contract.ArrangePulled{}, false, fmt.Errorf("找不到编排 %q：%w", req.ArrangementID, err)
	}
	if d.Status != "started" {
		return contract.ArrangePulled{}, false, nil
	}
	sp, err := parseSpec(d.Spec)
	if err != nil {
		return contract.ArrangePulled{}, false, fmt.Errorf("编排 spec 损坏：%w", err)
	}
	if sp.indexOf(req.Role) < 0 {
		return contract.ArrangePulled{}, false, fmt.Errorf("编排 %q 没有 %q 这个环节", req.ArrangementID, req.Role)
	}
	ag := m.agora()
	if ag == nil {
		return contract.ArrangePulled{}, false, nil // can't verify membership → fail-closed
	}
	// Mirror selectRound's gate: a disabled/paused company's buckets can't be drained —
	// items stay queued and the pull resumes in place once the company is re-activated.
	// Without this, a nudge enqueued just before the pause would still let the member
	// pull, do side effects and submit, making pause advisory-only.
	if !ag.CompanyActive(ctx, d.Company) {
		return contract.ArrangePulled{}, false, nil
	}
	if !slices.Contains(ag.MemberScopesForRole(ctx, d.Company, req.Role), req.CallerScope) {
		return contract.ArrangePulled{}, false, nil // not a member of this role
	}
	it, claimed, err := m.store.ClaimTop(ctx, d.ID, req.Role, req.CallerScope, nowUnix())
	if err != nil {
		return contract.ArrangePulled{}, false, err
	}
	if !claimed {
		return contract.ArrangePulled{}, false, nil
	}
	return contract.ArrangePulled{ItemID: it.ID, Content: sp.contentFor(req.Role), Payload: it.Payload}, true, nil
}

// Submit records the caller's product and routes the item (forward / back / park),
// guarded by claimed_by. A cancelled arrangement parks the item as cancelled.
func (m *Module) Submit(ctx context.Context, req contract.ArrangeSubmit) error {
	it, err := m.store.GetItem(ctx, req.ItemID)
	if err != nil {
		return fmt.Errorf("找不到任务 %q：%w", req.ItemID, err)
	}
	if it.ClaimedBy != req.CallerScope {
		return fmt.Errorf("这个任务不在你名下(由 %q 在办)", it.ClaimedBy)
	}
	d, err := m.store.GetDef(ctx, it.ArrangementID)
	if err != nil {
		return fmt.Errorf("编排 %q 已不存在：%w", it.ArrangementID, err)
	}

	newRole, newStatus, newPayload := it.Role, "", it.Payload

	switch {
	case d.Status == "cancelled":
		// Best-effort, RACY preservation of a product that landed just as the cancel did:
		// Cancel runs CancelDef (→ cancelled) THEN CancelLive (flips the item off
		// in_flight), so this note is only kept in the narrow window between the two — once
		// CancelLive completes, RouteItem's in_flight guard rejects the submit and this
		// payload is discarded anyway. Not a reliable capture; it just avoids dropping a
		// product that raced in before the item flipped. (L-AG-S2)
		newStatus = "cancelled"
		newPayload = appendNote(it.Payload, it.Role, "取消前产出", req.Output)
	case req.Result == "back":
		newPayload = appendNote(it.Payload, it.Role, "打回", req.Reason)
		sp, perr := parseSpec(d.Spec)
		if perr != nil {
			return fmt.Errorf("编排 spec 损坏：%w", perr)
		}
		if prev := sp.prevRole(it.Role); prev != "" {
			newRole, newStatus = prev, "queued"
		} else {
			newStatus = "rejected_no_upstream" // nothing upstream to send back to → park
		}
	case req.Result == "park":
		st := strings.TrimSpace(req.Status)
		if st == "" {
			st = "blocked"
		}
		// Reserved-status guard (single authority here, like the two create-time doors):
		// the park status is free-form, but the engine's own item statuses would be
		// misread — 'queued' re-enters dispatch (park→pull loop), 'in_flight' wedges a
		// cap slot forever (never dispatched, never lease-reclaimed), 'done' fires a
		// false terminal delivery, 'cancelled' is silently swept, 'unmet' gets
		// re-queued by self-heal. 'dead' is unused today but kept reserved so a park
		// can never claim it as a free-form status.
		switch st {
		case "queued", "in_flight", "unmet", "done", "dead", "cancelled":
			return fmt.Errorf("停泊状态 %q 与引擎保留状态冲突,请换一个描述性状态(如 blocked)", st)
		}
		newStatus = st
		newPayload = appendNote(it.Payload, it.Role, "停泊:"+st, req.Reason)
	default: // forward (default)
		newPayload = appendNote(it.Payload, it.Role, "产出", req.Output)
		sp, perr := parseSpec(d.Spec)
		if perr != nil {
			return fmt.Errorf("编排 spec 损坏：%w", perr)
		}
		if next := sp.nextRole(it.Role); next != "" {
			newRole, newStatus = next, "queued"
		} else {
			newStatus = "done" // terminal bucket
		}
	}

	routed, err := m.store.RouteItem(ctx, it.ID, req.CallerScope, newRole, newStatus, newPayload, nowUnix())
	if err != nil {
		return err
	}
	if !routed {
		return fmt.Errorf("交回失败：任务状态已变化(可能已被取消或重复提交)")
	}
	// Terminal bucket done → report the product to the company bridge + close the def.
	if newStatus == "done" {
		m.deliverTerminal(ctx, d, it.Role, req.Output)
	}
	return nil
}

// deliverTerminal reports ONE finished terminal product to the owning company's BRIDGE
// (which forwards it to the human operator), then closes the def IF no live items remain.
// The report goes to the bridge — NOT the def author — because the commander may itself be
// an agent. No bridge / no agora → the product stays in the item (logged), undelivered.
// A TRANSIENT emit failure, by contrast, leaves the def open (started): closing it there
// would silently mark the arrangement done with the report lost; an open def with no live
// items stays visible in status for a retry/manual close instead. (audit)
// The def is closed only when its last live item is done: arrangement_inject can fan a def
// into several concurrent items, so a single terminal completion is not the whole def.
func (m *Module) deliverTerminal(ctx context.Context, d store.Def, role, output string) {
	report := fmt.Sprintf("【编排完成】%s · 终端角色 %s\n\n%s", d.ID, role, strings.TrimSpace(output))
	switch ag := m.agora(); {
	case ag == nil:
		slog.Warn("arrangement: agora absent — terminal product kept in item, not delivered", "arr", d.ID)
	default:
		bridge, ok := ag.CompanyBridgeScope(ctx, d.Company)
		if !ok {
			slog.Warn("arrangement: company has no bridge — terminal product kept in item, not delivered", "arr", d.ID, "company", d.Company)
			break
		}
		gw := m.gw()
		if gw == nil {
			slog.Warn("arrangement: gateway absent — cannot deliver terminal product", "arr", d.ID)
			break
		}
		em, ok := gw.SourceByName(contract.SourceNameInternal).(contract.MessageEmitter)
		if !ok {
			slog.Warn("arrangement: internal source not an emitter — cannot deliver terminal product", "arr", d.ID)
			break
		}
		now := clock.Now()
		ev := contract.MessageEvent{
			Source:     contract.SourceNameInternal,
			ChatID:     bridge, // bridge scope → flow/agora BridgeRouting forwards outbound to the operator
			MessageID:  strconv.FormatInt(now.UnixNano(), 10),
			Text:       report,
			ReceivedAt: now,
			Dispatch:   &contract.MessageEventDispatchMeta{ForceNewSession: true, NoReply: true},
		}
		if err := em.Emit(ev); err != nil {
			// Transient failure: do NOT CompleteDef — leave the def started so the loss
			// is visible (an open def with no live items) rather than silently closed.
			slog.Warn("arrangement: terminal report emit failed — def left open", "arr", d.ID, "bridge", bridge, "err", err)
			return
		}
	}
	// Close the def ONLY when no live items remain (fan-out siblings may still be running);
	// CompleteDef is guarded on status='started' so a concurrent Cancel is never clobbered.
	n, err := m.store.CountLiveForDef(ctx, d.ID)
	if err != nil {
		slog.Warn("arrangement: count live items failed — leaving def open", "arr", d.ID, "err", err)
		return
	}
	if n > 0 {
		return // siblings still live → keep the def started
	}
	if err := m.store.CompleteDef(ctx, d.ID, nowUnix()); err != nil {
		slog.Warn("arrangement: mark def done failed", "arr", d.ID, "err", err)
	}
}

// Cancel marks an arrangement cancelled and parks its queued items.
func (m *Module) Cancel(ctx context.Context, arrangementID string) error {
	if _, err := m.store.GetDef(ctx, arrangementID); err != nil {
		return fmt.Errorf("找不到编排 %q：%w", arrangementID, err)
	}
	now := nowUnix()
	// Guarded transition: only cancel a live (started) def. 0 rows → the def
	// already reached a terminal status (done/cancelled) — the CancelDef itself is a
	// no-op there (its status='started' guard protects a 'done' from a late cancel).
	n, err := m.store.CancelDef(ctx, arrangementID, now)
	if err != nil {
		return err
	}
	if n == 0 {
		// Don't early-return: a retry after a partial failure (def already cancelled,
		// but the live items never got parked) still needs CancelLive to run. It is
		// idempotent — a no-op once no queued/in_flight rows remain.
		slog.Info("arrangement: def already cancelled — parking any stranded live items", "arr", arrangementID)
	}
	return m.store.CancelLive(ctx, arrangementID, now)
}

// Status returns the arrangements + ALL live/parked items (the contract snapshot; the
// webui panel fetches items paged via the store, so it does not load them all here).
// The two renderers (/arrangement + the webui panel) call the error-returning helpers
// directly so they can surface a "failed to load" note — this contract shape can't carry it.
func (m *Module) Status(ctx context.Context) contract.ArrangeStatus {
	rows, _ := m.arrangementRows(ctx)
	out := contract.ArrangeStatus{Arrangements: rows}
	if items, err := m.store.ListLive(ctx); err == nil {
		now := nowUnix()
		for _, it := range items {
			out.Items = append(out.Items, itemRow(it, now))
		}
	}
	return out
}

// arrangementRows builds the arrangement (def) rows — shared by Status and the webui
// panel, so the panel can list arrangements without also loading every live item.
// A store error comes back alongside nil rows so renderers can say "may be partial"
// instead of showing a healthy-empty view (audit D4).
func (m *Module) arrangementRows(ctx context.Context) ([]contract.ArrangeRow, error) {
	defs, err := m.store.ListDefs(ctx, "")
	if err != nil {
		return nil, err
	}
	rows := make([]contract.ArrangeRow, 0, len(defs))
	for _, d := range defs {
		row := contract.ArrangeRow{ID: d.ID, Company: d.Company, Author: d.Author, Status: d.Status}
		if sp, perr := parseSpec(d.Spec); perr == nil {
			row.Stages = sp.roles()
			var b strings.Builder
			for _, r := range sp.roles() {
				fmt.Fprintf(&b, "[%s] %s\n", r, sp.contentFor(r))
			}
			row.Detail = strings.TrimRight(b.String(), "\n")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// itemRow projects a store item to its webui row (age / in-flight age vs now). Shared by
// Status (all items) and the panel's paged fetch.
func itemRow(it store.Item, now int64) contract.ArrangeItemRow {
	row := contract.ArrangeItemRow{
		ItemID: it.ID, ArrangementID: it.ArrangementID, Role: it.Role,
		Status: it.Status, ClaimedBy: it.ClaimedBy, AgeSeconds: now - it.CreatedAt,
	}
	if it.Status == "in_flight" && it.ClaimedAt > 0 {
		row.InFlightSeconds = now - it.ClaimedAt
	}
	return row
}

// appendNote appends a labeled section to the accumulating payload (so downstream — incl.
// a verifier — sees the original task + every step's output + every reject reason).
func appendNote(payload, role, label, text string) string {
	if strings.TrimSpace(text) == "" {
		return payload
	}
	return payload + fmt.Sprintf("\n\n[%s · %s]\n%s", role, label, text)
}

var _ contract.Arrangements = (*Module)(nil)
