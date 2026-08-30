package core

import "context"

// Email outbound recovery log — the sibling-store surface the email
// source uses to survive a process crash between "agent decided to
// reply" and "SMTP delivery accepted". See
// docs/email-source-design.md (D-C3 v1.1 addition).
//
// Why a side log, not a column on the agent-emit message row: the
// per-emit assistant message lives in the generic SessionStore and
// carries no source-specific tag. Threading a source id through every
// caller just to support recovery would couple the store contract to
// one source. A small per-source append log keeps the concern
// isolated; the email source is the only writer + the only reader.
//
// Lifecycle:
//  1. Sink.Send / sink.Finish writes a row at enqueue time
//     (sent_at = 0).
//  2. The send worker UPDATEs sent_at = unix-second on SMTP success.
//  3. Source.Run startup scans for sent_at == 0 AND queued_at within
//     the last 24h AND source_id matches; re-enqueues each survivor
//     into the sendqueue.
//
// 24h TTL: an admin who restarted bob > 24h after the user's mail
// landed has almost certainly handled it by hand, and alice's
// "expected reply" window has already closed. Replaying older rows
// would surprise more often than help.

// EmailOutboundRow is one row from the email-outbound log. Source
// identifies the account that produced it (matches the email
// source's per-account SourceID). MessageIDInternal is the row PK
// at write time; MessageIDRFC is the RFC 5322 Message-ID. Body is
// the fully-rendered RFC 2822 message — the same []byte the send
// worker hands to SMTP — so recovery is byte-equivalent to the
// original attempt (same headers, same threading, same Message-ID
// so the recipient sees it as the SAME mail, not a duplicate).
//
// Recipient + SessionScope let recovery rebuild a sendReq without
// re-running the agent.
//
// The sent_at column exists only at the SQL layer: insert leaves it
// NULL, MarkEmailOutboundSent stamps it, ListUnsentEmailOutbound /
// PruneEmailOutbound filter on it — no Go-side field is needed.
type EmailOutboundRow struct {
	ID           int64  // assigned by the store on insert
	Source       string // per-account SourceID (e.g. "email-support")
	Recipient    string // canonical address
	MessageIDRFC string // RFC 5322 Message-ID (no angle brackets)
	SessionScope string // for RecordMessage on success; may be ""
	Body         []byte // fully-rendered RFC 2822 message
	QueuedAt     int64  // unix seconds — set at enqueue
}

// EmailOutboundStore is the sibling-store surface the email source
// uses for crash-recovery. Satisfied by the same concrete store
// impls that satisfy SessionStore + AdminLineStore — see the
// store/{sqlite,postgres,fallback} packages.
type EmailOutboundStore interface {
	// RecordEmailOutbound persists one pending-delivery row at enqueue
	// time (sent_at NULL). Returns the row id so the send worker can
	// update sent_at without a follow-up SELECT. This method ignores
	// any provided ID (assigned server-side).
	RecordEmailOutbound(ctx context.Context, row EmailOutboundRow) (int64, error)

	// MarkEmailOutboundSent stamps sent_at on the row identified by id.
	// Idempotent — calling it twice with the same id is harmless. ts is
	// unix seconds (caller passes its own clock so the value is
	// consistent with the rest of the source's logs).
	MarkEmailOutboundSent(ctx context.Context, id int64, ts int64) error

	// DeleteEmailOutbound removes the row identified by id. Used when a
	// SYNCHRONOUS enqueue is REJECTED (queue full / ctx done) after the
	// crash-recovery row was already persisted: without the delete the next
	// restart's recovery scan would replay a mail the caller was just told
	// failed (→ the user can receive two). Idempotent — deleting a missing /
	// already-sent id is harmless.
	DeleteEmailOutbound(ctx context.Context, id int64) error

	// ListUnsentEmailOutbound returns every row with sent_at == 0 AND
	// queued_at >= sinceUnix AND source == sourceID. Order: by
	// queued_at ASC so re-enqueue replays in the original arrival
	// order. limit caps the result so a restart after a long outage
	// can't blow the sendqueue all at once; pass 0 for "no cap".
	ListUnsentEmailOutbound(ctx context.Context, sourceID string, sinceUnix int64, limit int) ([]EmailOutboundRow, error)

	// PruneEmailOutbound deletes SENT rows (sent_at IS NOT NULL) whose
	// queued_at < olderThanUnix, returning the number removed. Unsent
	// rows are never pruned so the crash-recovery replay window stays
	// intact. Mirrors PruneAdminLine — without it the table (each row
	// carrying a full RFC 2822 body) grows unbounded. Called from the
	// Sweep housekeeping tick.
	PruneEmailOutbound(ctx context.Context, olderThanUnix int64) (int64, error)

	// LatestOutboundBody returns the fully-rendered RFC 2822 body of the
	// MOST RECENT outbound row for (source, recipient), or (nil, nil) when
	// none exists. Reused by the email source's inbound quote-stripper as the
	// persistent "prior reply" to diff a quoted reply against — surviving a
	// restart that drops the in-memory prior-body cache. Only the latest is
	// ever needed (the sender quotes bob's last reply), so the table's age
	// prune is irrelevant: a reply to a >prune-age-old bob mail just misses
	// here and falls back to the regex stripper.
	LatestOutboundBody(ctx context.Context, sourceID, recipient string) ([]byte, error)
}
