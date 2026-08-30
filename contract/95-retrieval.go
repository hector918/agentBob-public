package contract

import (
	"context"
	"encoding/json"
)

const (
	// RetrievalPersonalCompany is the company sentinel for non-agora (personal)
	// messages — the cold-memory leaf's pre-built '__personal__' partition.
	RetrievalPersonalCompany = "__personal__"
	// RetrievalBotHandle is the source_handle stamped on assistant messages (the
	// speaker is bob itself, not a person).
	RetrievalBotHandle = "__bob__"
)

// RetrievalQuery is one recall query to the cold-memory leaf's POST /retrieve.
// Params/Filters serialize straight into the request body; the scope clamp
// (company / user_ids / channel_id) is folded into Filters by the caller — the
// leaf itself fail-closes a query that arrives without a company filter.
type RetrievalQuery struct {
	QueryType string         // entity_mentions | time_window | semantic
	Params    map[string]any // query_type-specific params
	Filters   map[string]any // generic filters incl. the clamped company_id / user_ids
	Limit     int
}

// RetrievalClient is the EXTERNAL read contract to the cold-memory leaf — what a
// consumer outside leaf/retrieval needs. Retrieve fail-opens: a returned error
// means the leaf is unreachable and the caller skips (never fails the turn). The
// write path (Ingest) stays on the concrete client inside leaf/retrieval. Provided
// to consumers (the recall_memory tool) via the registry; nil when retrieval is off.
type RetrievalClient interface {
	Retrieve(ctx context.Context, q RetrievalQuery) (json.RawMessage, error)
}

// RetrievalFeed is the write seam the FLOW calls after a turn completes: hand it
// the turn's user events + the assistant reply and it enqueues them (a durable
// outbox) for the feeder to push to the leaf. Provided by leaf/retrieval; the flow
// optional-requires it (absent / disabled → the flow skips, zero behavior change).
// The FLOW supplies the identity (company/room from its own scope semantics); the
// feed never re-derives it. See docs/cold-memory-trunk.md §3.
type RetrievalFeed interface {
	// EnqueueTurn is fire-and-forget: it persists outbox rows and returns; the
	// feeder pushes them asynchronously. It must not fail or block the turn. The
	// persist runs on the caller's turn ctx — a turn whose ctx is ALREADY cancelled
	// at finalization drops its rows (logged corpus gap), so the outbox is durable
	// only for a turn whose ctx outlives the enqueue (the normal case).
	EnqueueTurn(ctx context.Context, t RetrievalTurn)
}

// RetrievalTurn is one turn's corpus contribution, exactly as the flow holds it
// (no session read-back — trunk's MessageStore exposes neither stable ids nor a
// cursor). Each user event keeps its OWN speaker handle (so a group's many
// speakers are attributed correctly); the assistant reply is one entry under
// RetrievalBotHandle. The leaf dedups on a deterministic message_id derived from
// (sid, event id), so a re-enqueue is safe.
type RetrievalTurn struct {
	Sid      string
	Company  string         // "" → RetrievalPersonalCompany
	Room     string         // group scope; "" for a private DM
	Events   []MessageEvent // this turn's user messages (each carries its own handle / name / time)
	Reply    string         // assistant final reply (already digests this turn's tool output)
	AnchorID string         // the triggering event's MessageID — the assistant message_id anchor
}
