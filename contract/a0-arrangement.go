package contract

import "context"

// Arrangements is the arrangement-table capability (docs/arrangement.md): a flexible,
// per-company orchestration of role-owned priority buckets. An "arrangement" is one
// such orchestration (a company has many); the FIXED engine is just input (Inject) +
// dispatch (a ticker nudging idle members to Pull) + the rule "one priority bucket
// per role". The VARIABLE part is the arrangement's spec — a flexible JSON the author
// writes (ordered role buckets, each with free-form content/要求), validated at Define
// and FROZEN once started (only Cancel, never edit).
//
// Provided by leaf/arrangement (Optional). The arrangement_* tools (in leaf/tools)
// reach it lazily; the webui panel reads Status(). Cross-company authoring is
// intentional: the gate is the tool grant, not company membership.
type Arrangements interface {
	// Define validates a spec JSON and stores a new arrangement for req.Company in the
	// "started" (frozen) state. Returns the new arrangement id. The author may target
	// any company (the arrangement_define grant is the trust boundary).
	Define(ctx context.Context, req ArrangeDefine) (id string, err error)

	// Inject places a new work item onto a started arrangement's entry bucket.
	Inject(ctx context.Context, req ArrangeInject) (itemID string, err error)

	// Pull atomically claims the top queued item of (arrangement, role) for the calling
	// member and marks it in_flight. ok=false (no error) when the bucket is empty or the
	// caller is not an active member of that role. Returns the item payload + the
	// bucket's authored content (要求/指令).
	Pull(ctx context.Context, req ArrangePull) (item ArrangePulled, ok bool, err error)

	// Submit records the caller's product for the item it holds and routes it per
	// req.Result: forward (→ next bucket; terminal → done), back (→ previous bucket;
	// none → parked rejected_no_upstream), or park (leave the active flow with a
	// descriptive status, e.g. blocked). Fails if the caller does not hold the item.
	Submit(ctx context.Context, req ArrangeSubmit) error

	// Cancel marks an arrangement cancelled and parks its queued items; in-flight items
	// park when their holder next submits. Admin path.
	Cancel(ctx context.Context, arrangementID string) error

	// Status returns the arrangements + their live/parked items for the webui table.
	Status(ctx context.Context) ArrangeStatus
}

// ArrangeDefine creates an arrangement. Spec is the flexible JSON (see docs §spec).
type ArrangeDefine struct {
	CallerScope string
	Company     string
	Spec        string // JSON: {"buckets":[{"role":..,"content":..}, ...]}
}

// ArrangeInject adds a work item to an arrangement. Stage "" = the entry bucket; a
// non-empty Stage drops the item directly into that bucket — the primitive a member uses
// to FAN OUT (e.g. a verify member splitting a collected batch into N downstream tasks).
type ArrangeInject struct {
	CallerScope   string
	ArrangementID string
	Stage         string
	Payload       string
	Priority      int
}

// ArrangePull claims from (arrangement, role).
type ArrangePull struct {
	CallerScope   string
	ArrangementID string
	Role          string
}

// ArrangePulled is a claimed item handed to the member: the item id, the bucket's
// authored content (instructions/要求 the member acts on), and the accumulated payload.
type ArrangePulled struct {
	ItemID  string
	Content string
	Payload string
}

// ArrangeSubmit returns a member's product and routes the item.
type ArrangeSubmit struct {
	CallerScope string
	ItemID      string
	Output      string // the product (forward) or commentary
	Result      string // "forward" | "back" | "park"
	Status      string // park only: the descriptive status (default "blocked")
	Reason      string // back/park: a note appended to the payload
}

// ArrangeStatus is the webui snapshot.
type ArrangeStatus struct {
	Arrangements []ArrangeRow
	Items        []ArrangeItemRow
}

// ArrangeRow is one arrangement (for the webui list). Stages is the ordered bucket
// roles; Detail is a human-readable per-stage content dump shown in the row glance.
type ArrangeRow struct {
	ID      string
	Company string
	Author  string
	Status  string // started | done | cancelled
	Stages  []string
	Detail  string
}

// ArrangeItemRow is one live/parked item; InFlightSeconds is how long the holder has
// been on it (0 unless in_flight) — the webui surfaces the longest as the headline.
type ArrangeItemRow struct {
	ItemID          string
	ArrangementID   string
	Role            string
	Status          string // queued | in_flight | unmet | blocked | done | dead | cancelled | …
	ClaimedBy       string
	AgeSeconds      int64
	InFlightSeconds int64
}
