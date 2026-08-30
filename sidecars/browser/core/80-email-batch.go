package core

import "context"

// EmailBatchRow is one coalescing-buffer row: the running combined reply
// the email source is accumulating for one session (sid) before flushing
// it as a single email. One row per (Source, SID); the source UPSERTs the
// full current state on each turn and DELETEs it on flush. Survives a
// restart so computed-but-unsent replies are not lost.
type EmailBatchRow struct {
	ID           int64
	Source       string // per-account SourceID (e.g. "email-binserv")
	SID          string // session id — the coalescing key
	Scope        string // session scope (may be "")
	Recipient    string // canonical recipient address (primary To — the original sender)
	Cc           string // reply-all Cc: comma-joined canonical addresses ("" = single-recipient reply)
	Alias        string // forwarded recipient alias for Reply-To ("" if none)
	CombinedText string // running combined reply text (segments already joined)
	SegCount     int    // number of segments combined so far
	Subject      string // threading: subject to render the flushed mail with
	InReplyTo    string // threading: parent Message-ID (no angle brackets)
	RefChain     string // threading: References header chain
	UpdatedAt    int64  // unix seconds — last segment append
}

// EmailBatchStore is the sibling-store surface the email source uses to
// persist its reply-coalescing buffer across restarts. Satisfied by the
// same concrete store impls as EmailOutboundStore.
type EmailBatchStore interface {
	// UpsertEmailBatch inserts or replaces the single row for (Source, SID)
	// with the full current state (the caller passes the complete combined
	// text each time, not a delta). Uniqueness is keyed on (source, sid).
	UpsertEmailBatch(ctx context.Context, row EmailBatchRow) error
	// ListPendingEmailBatch returns every batch row for sourceID, ordered by
	// updated_at ASC. Used at startup to rehydrate the in-memory buffer.
	ListPendingEmailBatch(ctx context.Context, sourceID string) ([]EmailBatchRow, error)
	// DeleteEmailBatch removes the row for (sourceID, sid) — called on flush.
	DeleteEmailBatch(ctx context.Context, sourceID, sid string) error
}
