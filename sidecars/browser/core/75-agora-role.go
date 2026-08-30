package core

import (
	"context"
	"errors"
	"time"
)

// ErrAgoraNotFound is the sentinel returned by all agora Get/GetByName
// paths when the row doesn't exist (Members / Companies / Positions /
// Employments / Channels / Inboxes / InboxSources / SessionMeta). "Not
// found" is a normal control-flow outcome — callers should errors.Is-
// check and surface a user-friendly "no such X" without logging it as a
// backend failure. One shared sentinel for the whole agora subsystem
// (M3a+).
var ErrAgoraNotFound = errors.New("agora: row not found")

// ErrAgoraInboxArchived is returned by InboxStore.UpdateInboxStatus when
// the target inbox exists but is archived. Archived is the TERMINAL state
// a Terminate lands on (D1: an archived inbox is unaddressable) — pause /
// resume must not touch it, or a two-step pause→resume would revive an
// inbox whose employment is already terminated. Revival goes through
// re-hire (ReactivateInbox, which rebinds employment_id).
var ErrAgoraInboxArchived = errors.New("agora: inbox is archived (terminal)")

// Agora persistence — interfaces for the postgres-only agora subsystem
// (docs/agora.md). agora is bob's "business automation platform" layer:
// Companies, Members, Employments, Channels, Inboxes, and the dispatch queue.
//
// Persistence convention: agora tables live behind the store middleware
// (core.SessionStore / core.PermissionStore lineage) but per agora design
// v0.5 §11.8 route to PRIMARY ONLY (no dual-write); a failed primary surfaces
// as ErrStoreConnLost so the agora resolver / dispatcher can halt rather than
// silently operate on an empty secondary. All agora tables MUST be prefixed
// `bob_agora_`.
//
//: the M1 per-scope role-tagging store (RoleAssignment / RoleStore
// / bob_agora_role_assignments) was removed — it fed the tool-audience gate,
// which was itself retired in PR-1, leaving the assignment table
// write-only with no runtime consumer. The live "role" concept now is
// Employment.RoleName → the Company's permissions_yaml grants (send-resolver).

// DispatchRow is one row in bob_agora_dispatch_queue. The full row has
// more columns (started_at / completed_at / worker_id / error) that the
// M3 dispatcher writes — M2 only needs the enqueue-side fields.
//
// Fields:
//   - InboxID       optional in M2 (no Inbox concept yet); set when the
//     M3 routing layer knows the inbox id.
//   - SessionScope  required — the target scope (or a synthetic scope
//     the dispatcher's router will resolve in M3).
//   - Priority      lower = sooner. send_message default = 20,
//     report --urgent = 5.
//   - Kind          message / report / admin / resume / inbox_check /
//     validate_session.
//   - TriggerRef    free-form caller-supplied breadcrumb (M2 stores a
//     JSON debug payload; M3 may switch to a typed envelope).
//   - EnqueuedAt    unix seconds (clock.NowUnix()).
type DispatchRow struct {
	InboxID      string
	SessionScope string
	Priority     int
	Kind         string
	TriggerRef   string
	EnqueuedAt   int64
}

// DispatchEnqueuer is the M2 minimal-surface enqueue interface. The CLI
// (`bob agora send` / `bob agora report`) is the only consumer today.
// M3 widens it with the worker-side Pop / UpdateStatus / etc on the
// DispatchStore interface below; the same concrete Store struct in
// store/sqlite + store/postgres satisfies both.
//
// Errors flow through classifyErr (same as RoleStore) so callers can
// errors.Is-check core.ErrStoreConnLost under fallback's primary-only
// routing.
type DispatchEnqueuer interface {
	// EnqueueDispatch inserts row into bob_agora_dispatch_queue and
	// returns the new row's id. Defaults applied at INSERT time:
	//   - status = 'pending'
	//   - started_at / completed_at / worker_id / error = NULL
	EnqueueDispatch(ctx context.Context, row DispatchRow) (int64, error)
}

// DispatchItem is one row popped from bob_agora_dispatch_queue. The
// dispatcher's process loop drives off these — fields chosen to be the
// minimum set needed for routing (kind → bridge vs agent loop) +
// progress accounting (ID for status update).
type DispatchItem struct {
	ID           int64
	InboxID      string
	SessionScope string
	Priority     int
	Kind         string
	TriggerRef   string
	EnqueuedAt   int64
}

// DispatchStore is the M3b worker-side interface — pop / mark
// done|failed / reconcile-on-startup. Sibling of DispatchEnqueuer;
// same concrete *Store satisfies both. Errors classified through
// classifyErr so the agora-halt cascade can errors.Is-check
// ErrStoreConnLost.
type DispatchStore interface {
	DispatchEnqueuer

	// PopPending claims up to `limit` rows in priority + enqueued_at
	// order, flipping their status pending → processing and stamping
	// started_at + worker_id atomically. Returns the claimed rows so
	// the dispatcher can dispatch them. Concurrent dispatchers would
	// each get a disjoint slice (FOR UPDATE SKIP LOCKED on pg /
	// per-row UPDATE in sqlite).
	PopPending(ctx context.Context, workerID string, nowUnix int64, limit int) ([]DispatchItem, error)

	// MarkDone flips status processing → done + stamps completed_at.
	MarkDone(ctx context.Context, id int64, nowUnix int64) error

	// MarkFailed flips status processing → failed + writes error.
	MarkFailed(ctx context.Context, id int64, nowUnix int64, errMsg string) error

	// ReconcileStale moves rows stuck in 'processing' older than
	// staleBefore unix-seconds back to 'pending' (worker_id / started_at
	// cleared). Called once on dispatcher startup so crashed-mid-work
	// items get re-picked. Returns count moved.
	ReconcileStale(ctx context.Context, staleBefore int64) (int, error)

	// UpdateDispatchStatus is the ADMIN retry/cancel path. It flips one
	// row's status to the given value ('pending' | 'cancelled'), guarded by
	// the current status so a row that is in-flight can't be double-run:
	//   - 'pending'   only re-pends a TERMINAL row (status IN failed,
	//     cancelled); clears worker_id / started_at / completed_at / error.
	//     A 'processing' row is NOT re-pended here — that would let a second
	//     worker pop and run the same payload while the first is still going.
	//   - 'cancelled' only cancels a non-terminal row (status IN pending,
	//     processing); it will NOT rewrite an already-done/failed history row.
	// A 0-row update (unknown id OR guard not satisfied) returns
	// ErrAgoraNotFound.
	UpdateDispatchStatus(ctx context.Context, id int64, status string) error

	// RependProcessing is the DISPATCHER-internal re-pend: it flips a row
	// the dispatcher itself just popped (status='processing', worker_id ==
	// the caller's own id) back to 'pending', clearing worker_id /
	// started_at. Used when a freshly-popped item can't immediately run
	// (its session_scope sid-lock is held) so it must be returned to the
	// queue without being executed. The worker_id guard scopes it to the
	// caller's own claims so it can never disturb another dispatcher's
	// in-flight row. Returns the number of rows affected (0 = the row was
	// no longer the caller's processing row — e.g. reconciled away).
	RependProcessing(ctx context.Context, id int64, workerID string) (int, error)

	// RequeueDispatch is the RESTART-recovery re-pend: it flips a TERMINAL
	// row (status IN done, failed) back to 'pending' so the dispatcher
	// re-delivers its payload. Used by the startup pending_inbound sweep
	// for internal (bridge) messages whose emitted event died mid-turn in
	// the previous process — the dispatch row is the only durable carrier
	// of the message, so re-pending it IS the re-send. 'cancelled' rows are
	// excluded (admin's explicit decision stands); 'pending'/'processing'
	// rows are excluded (already due for delivery / in flight). Returns
	// requeued=false with nil error when the guard rejected the flip or
	// the row no longer exists (e.g. pruned).
	RequeueDispatch(ctx context.Context, id int64) (requeued bool, err error)

	// ListRecentDispatch returns the most recent rows (regardless of
	// status), newest enqueued first. inboxID="" → all. limit<=0 → 50.
	// Used by admin /agora company tail / /agora member tail to surface
	// queue activity per scope.
	ListRecentDispatch(ctx context.Context, inboxID string, limit int) ([]DispatchItem, error)

	// PruneDispatchQueue deletes rows in terminal state ('done',
	// 'cancelled', or 'failed') whose completed_at is older than
	// `before`. Returns the number of rows deleted. Called by the
	// dispatcher's periodic prune gate (R7-12, time-based — ticker
	// rate independent). 'failed' rows are pruned too (D8'
	// decision: limited debug value beyond a few days).
	PruneDispatchQueue(ctx context.Context, before time.Time) (int64, error)

	// CancelPendingForScope bulk-cancels every dispatch row whose
	// session_scope column matches sessionScope AND status == 'pending'.
	// Dispatch rows are keyed by SCOPE (every producer — the CLI today —
	// writes a session_scope, and the dispatcher routes/locks by it), so
	// callers MUST pass the session's scope (SessionInfo.Key), NOT its DB
	// id. Returns the number of rows flipped. Used by admin finalize / kill
	// paths (D3') so a terminated session doesn't get its
	// leftover message/report items consumed by the dispatcher after the
	// fact. Idempotent: no rows → returns (0, nil).
	CancelPendingForScope(ctx context.Context, sessionScope string) (int, error)

	// CancelPendingForInbox bulk-cancels every PENDING dispatch row for an
	// inbox. Called by Terminate's archive path (decision ⑬): an
	// archived inbox is terminal (D1) and PopPending skips it, so its pending
	// backlog would otherwise sit forever (PruneDispatchQueue only reaps
	// terminal rows) and re-run if the inbox were later re-hired. Returns the
	// number of rows flipped; idempotent (no pending rows → (0, nil)).
	CancelPendingForInbox(ctx context.Context, inboxID string) (int, error)
}
