package core

// ApprovalDecision is what a /approve or /deny slash command yields to the
// dispatcher (or to a tool's Run() if it called the requester directly).
//
//   - Reason — one of "approved" | "denied" | "timeout" | "cancelled". Goes into
//     the error string the model sees so it can self-recover.
//   - By     — the user_id that decided. Empty on timeout / cancel.
type ApprovalDecision struct {
	Approved bool
	By       string
	Reason   string
}

// ApprovalRequester is the slice of the approval manager visible to the
// agent's dispatcher (and to tools that need approval gating from inside
// their own Run()). The dispatcher uses it like:
//
//	id, ch := req.Register(sessionScope, target, tool, tier)
//	defer req.Unregister(id)
//	sink.Delta("🔐 …  /approve "+id+" or /deny "+id+" (30s)")
//	select {
//	    case dec := <-ch:        // user typed /approve | /deny
//	    case <-time.After(30s):  // timeout → deny
//	    case <-ctx.Done():       // turn cancelled
//	}
//
// Splitting Register from "block-and-wait" keeps the prompt-emission step
// (which needs the dispatcher's sink) outside the manager — the manager
// itself never touches sinks or the delivery layer.
//
// The /approve / /deny slash handlers (in pipeline-gateway) call the
// manager's Resolve method directly, not through this interface.
type ApprovalRequester interface {
	// Register inserts a new pending row and returns its id plus the
	// result channel. id is short (6 hex chars), human-typable. The channel
	// is buffered (1) so Resolve never blocks.
	Register(sessionScope, target, tool string, tier Tier) (id string, ch <-chan ApprovalDecision)
	// Unregister drops a pending row by id. Idempotent — call as a defer
	// after Register so a timed-out / cancelled wait doesn't leak the row.
	Unregister(id string)
}
