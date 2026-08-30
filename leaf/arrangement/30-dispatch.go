package arrangement

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/arrangement/store"
)

// pushItem is one queued wake: "nudge this member to come pull (arrangement, role)".
// It carries NO ownership — the item stays queued in its bucket; only the member's pull
// marks it in_flight (docs/arrangement.md).
type pushItem struct {
	arrangementID string
	role          string
	scope         string
}

// key is the pending-wake dedup key: one pending wake per (arrangement, role, scope) —
// NOT per scope alone, which would let one bucket's pending wake shadow a different
// bucket's wake for the same member (audit). The per-scope pacing lives in
// lastNudged + the drainer's cooldown check, not here.
func (p pushItem) key() string {
	return p.arrangementID + "\x00" + p.role + "\x00" + p.scope
}

// runSelector periodically SELECTS work and enqueues wakes — it never emits. Interval =
// the settable tick. Cheap: two queries (started defs + one GROUP BY over queued items).
func (m *Module) runSelector(ctx context.Context) {
	defer m.wg.Done()
	for {
		t := time.NewTimer(clampTick(time.Duration(m.tickNanos.Load())))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			m.selectRound(ctx)
		}
	}
}

// selectRound enqueues a wake per eligible IDLE member of every started+active
// arrangement's non-empty bucket (deduped per arrangement+role+scope bucket). Unstaffed roles → MarkUnmet. It
// does NOT touch item status and does NOT emit — items stay queued until a member pulls.
func (m *Module) selectRound(parent context.Context) {
	ag := m.agora()
	sm := m.sm()
	if ag == nil || sm == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, dispatchTimeout)
	defer cancel()

	// Lease-timeout recovery, GATED by liveness: an item left in_flight by a crashed/restarted
	// worker (its turn is gone, and the selector only dispatches `queued` — never in_flight)
	// would strand forever and stall the arrangement. Reclaim a stale in_flight item ONLY when
	// its claimer is NOT currently running a turn (RunningAtScope==0) — so a slow-but-alive
	// worker (turns have no wall-clock cap) is never reclaimed, which would re-execute its
	// external side effects. A restart resets RunningAtScope, so post-restart orphans become
	// eligible. (Residual: a worker that did a side effect then died BEFORE submit re-runs it
	// — at-least-once; worker work should be idempotent. See docs/arrangement.md §13.)
	if stale, e := m.store.StaleInFlight(ctx, nowUnix()-staleLeaseSeconds); e != nil {
		slog.Warn("arrangement: stale in_flight scan failed", "err", e)
	} else {
		for _, it := range stale {
			if sm.RunningAtScope(it.ClaimedBy) != 0 {
				continue // claimer still running a turn — slow but alive, leave it
			}
			if ok, re := m.store.RequeueInFlight(ctx, it.ID, nowUnix()); re != nil {
				slog.Warn("arrangement: requeue stale in_flight failed", "item", it.ID, "err", re)
			} else if ok {
				slog.Warn("arrangement: reclaimed orphaned in_flight (claimer not running)", "item", it.ID, "arrangement", it.ArrangementID, "claimer", it.ClaimedBy)
			}
		}
	}

	defs, err := m.store.ListDefs(ctx, "started")
	if err != nil {
		slog.Warn("arrangement: list started failed", "err", err)
		return
	}
	active := map[string]store.Def{} // started + company-active, by id
	for _, d := range defs {
		if ag.CompanyActive(ctx, d.Company) {
			active[d.ID] = d
		}
	}
	buckets, err := m.store.QueuedBuckets(ctx)
	if err != nil {
		slog.Warn("arrangement: queued-buckets query failed", "err", err)
		return
	}
	for _, b := range buckets {
		d, ok := active[b.ArrangementID]
		if !ok {
			continue // not started / company disabled or paused → leave queued
		}
		roundNow := nowUnix()
		// Serviceable = a role can actually drain its bucket: it has live members AND the
		// collaborator capability (pull+submit). The collaborator check is the RUNTIME
		// backstop to the create-time gate — it catches a grant REVOKED after define (TOCTOU)
		// that would otherwise leave items un-pullable (the runaway-loop cause). Either gap →
		// park the bucket unmet (paused) + self-heal when it's restored (below).
		scopes := ag.MemberScopesForRole(ctx, d.Company, b.Role)
		canProcess, _ := m.roleCanProcess(ctx, d.Company, b.Role)
		if len(scopes) == 0 || !canProcess {
			reason := "无可用成员"
			if len(scopes) > 0 && !canProcess {
				reason = "角色缺协作权限(arrangement_pull+submit)"
			}
			if parked, e := m.store.MarkUnmet(ctx, b.ArrangementID, b.Role, nowUnix()); e != nil {
				slog.Warn("arrangement: mark unmet failed", "id", b.ArrangementID, "role", b.Role, "err", e)
			} else if parked > 0 {
				slog.Warn("arrangement: role cannot serve bucket — items parked unmet", "id", b.ArrangementID, "company", d.Company, "role", b.Role, "reason", reason, "parked", parked)
			}
			continue
		}
		for _, scope := range scopes {
			if sm.RunningAtScope(scope) != 0 {
				continue // busy with some turn — the idle gate
			}
			m.enqueuePush(pushItem{arrangementID: b.ArrangementID, role: b.Role, scope: scope}, roundNow)
		}
	}

	// Self-heal (docs/arrangement.md §unmet): an unmet bucket whose role has regained
	// members → flip its items back to queued (next tick picks them up). Covers transient
	// gaps — a re-staffed role, a re-activated member inbox — that earlier read as empty.
	unmet, err := m.store.UnmetBuckets(ctx)
	if err != nil {
		slog.Warn("arrangement: unmet-buckets query failed", "err", err)
		return
	}
	for _, b := range unmet {
		d, ok := active[b.ArrangementID]
		if !ok {
			continue
		}
		if canProcess, _ := m.roleCanProcess(ctx, d.Company, b.Role); len(ag.MemberScopesForRole(ctx, d.Company, b.Role)) == 0 || !canProcess {
			continue // still unserviceable (no member or lost collaborator) — leave parked
		}
		if n, e := m.store.RequeueUnmet(ctx, b.ArrangementID, b.Role, nowUnix()); e != nil {
			slog.Warn("arrangement: requeue unmet failed", "id", b.ArrangementID, "role", b.Role, "err", e)
		} else if n > 0 {
			slog.Info("arrangement: role re-staffed — unmet items re-queued", "id", b.ArrangementID, "role", b.Role, "requeued", n)
		}
	}
}

// runDrainer emits at most ONE wake per push interval (default 8s) until the queue is
// empty, then idles — spacing the starts so a selector tick can't fire a burst of
// concurrent member turns.
func (m *Module) runDrainer(ctx context.Context) {
	defer m.wg.Done()
	for {
		t := time.NewTimer(clampPush(time.Duration(m.pushNanos.Load())))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			m.drainOne(ctx)
		}
	}
}

// drainOne pops one wake and emits it IF still valid: the member is still idle AND the
// bucket still has queued work. It never marks the item — the member's pull does that,
// strictly AFTER this push, keeping the table truthful (no false "picked up").
func (m *Module) drainOne(parent context.Context) {
	p, ok := m.dequeuePush()
	if !ok {
		return // queue empty
	}
	gw := m.gw()
	sm := m.sm()
	ag := m.agora()
	if gw == nil || sm == nil || ag == nil {
		return
	}
	if sm.RunningAtScope(p.scope) != 0 {
		return // went busy since enqueue — drop; selector re-enqueues if still due
	}
	// Distinct buckets' wakes for one scope can coexist in the queue (dedup is per
	// bucket+scope), so re-check the per-scope cooldown here: the previous wake's turn
	// may not have registered busy yet. Drop — the selector re-enqueues if still due.
	m.pushMu.Lock()
	cooling := nowUnix()-m.lastNudged[p.scope] < nudgeCooldownSec
	m.pushMu.Unlock()
	if cooling {
		return
	}
	ctx, cancel := context.WithTimeout(parent, dispatchTimeout)
	defer cancel()
	if n, err := m.store.CountQueued(ctx, p.arrangementID, p.role); err != nil || n == 0 {
		return // bucket drained meanwhile — don't wake into an empty pull
	}
	emitter, ok := gw.SourceByName(contract.SourceNameInternal).(contract.MessageEmitter)
	if !ok {
		return
	}
	// Mirror selectRound's CompanyActive gate (same as Pull): a company paused since
	// the enqueue must not have its members woken — items stay queued and dispatch
	// resumes in place once the company is re-activated. A GetDef error DROPS the wake
	// (the selector re-enqueues) rather than waking with empty content — the gate must
	// not be silently skipped on a store hiccup (Pull fail-closes too, but the wake
	// shouldn't fire at all).
	d, err := m.store.GetDef(ctx, p.arrangementID)
	if err != nil {
		return
	}
	if !ag.CompanyActive(ctx, d.Company) {
		return
	}
	content := ""
	if sp, perr := parseSpec(d.Spec); perr == nil {
		content = sp.contentFor(p.role)
	}
	now := clock.Now()
	ev := contract.MessageEvent{
		ChatID:     p.scope,
		MessageID:  strconv.FormatInt(now.UnixNano(), 10),
		Text:       pullNudgeText(p.arrangementID, p.role, content),
		ReceivedAt: now,
		// ForceNewSession: one pull = one fresh session. NoReply: a fire-and-forget wake —
		// the member just calls arrangement_pull/submit; the turn must RUN even with no
		// reply target (flow/agora uses a discard sink), not be dropped like a routing
		// failure. See contract.MessageEventDispatchMeta.NoReply.
		Dispatch: &contract.MessageEventDispatchMeta{ForceNewSession: true, NoReply: true},
	}
	if err := emitter.Emit(ev); err != nil {
		slog.Debug("arrangement: nudge emit failed", "scope", p.scope, "err", err)
		return
	}
	// Stamp the emit so the selector won't re-enqueue this scope until the woken turn
	// has had time to register busy (closes the async emit→busy double-nudge window).
	m.pushMu.Lock()
	m.lastNudged[p.scope] = nowUnix()
	m.pushMu.Unlock()
}

// enqueuePush adds a wake unless one for that (arrangement, role, scope) bucket+member
// is already queued OR the scope was nudged within the cooldown (its last wake's turn
// may not be busy yet). now is the round's clock read.
func (m *Module) enqueuePush(p pushItem, now int64) {
	m.pushMu.Lock()
	defer m.pushMu.Unlock()
	if m.pushSet[p.key()] || now-m.lastNudged[p.scope] < nudgeCooldownSec {
		return
	}
	m.pushSet[p.key()] = true
	m.pushQ = append(m.pushQ, p)
}

// dequeuePush pops the front wake (FIFO), or ok=false when empty.
func (m *Module) dequeuePush() (pushItem, bool) {
	m.pushMu.Lock()
	defer m.pushMu.Unlock()
	if len(m.pushQ) == 0 {
		return pushItem{}, false
	}
	p := m.pushQ[0]
	m.pushQ = m.pushQ[1:]
	if len(m.pushQ) == 0 {
		m.pushQ = nil // release the backing array when drained
	}
	delete(m.pushSet, p.key())
	// Bound lastNudged: drop entries already past the cooldown (a cooled entry is a no-op
	// in enqueuePush, so dropping it is safe) so the map can't grow per distinct scope.
	cutoff := nowUnix() - nudgeCooldownSec
	for scope, ts := range m.lastNudged {
		if ts < cutoff {
			delete(m.lastNudged, scope)
		}
	}
	return p, true
}

// pushDepth reports the current push-queue length (for the status panel).
func (m *Module) pushDepth() int {
	m.pushMu.Lock()
	defer m.pushMu.Unlock()
	return len(m.pushQ)
}

// pullNudgeText is the wake message a bucket's member gets — it names what to pull and
// includes the bucket's authored content as a hint (the member re-fetches it via pull).
func pullNudgeText(arrangementID, role, content string) string {
	hint := ""
	if content != "" {
		hint = "\n本环节要求：" + content
	}
	return fmt.Sprintf("【任务派发】你在编排「%s」的「%s」环节有待处理任务。%s\n"+
		"请调用 arrangement_pull(arrangement_id=\"%s\", role=\"%s\") 领取,完成后用 arrangement_submit 交回"+
		"(result=forward 通过/往下;result=back 打回上一环节;result=park 做不动则停泊并写明 status/reason)。\n"+
		"⚠️ 任务只有在你调用 arrangement_submit 之后才算交付——即使活儿做完了,没 submit 它就一直挂在你名下、不会前进。所以务必把 submit 当作本轮的最后一步。\n"+
		"若领取为空(已被同事取走),无需动作。",
		arrangementID, role, hint, arrangementID, role)
}
