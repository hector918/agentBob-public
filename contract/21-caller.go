package contract

// CallerIdentity is the resolved sender identity for the current turn, filled by
// the FLOW (which holds the source event + can reach the account graph) and
// relayed to tools via ToolContext. It is GENERIC — any identity-aware tool reads
// it; recall_memory is the first consumer.
//
// The flow is the identity authority: different flows resolve identity differently
// (DM = the caller's own handle; group = the shared room; agora = the company), so
// a tool never re-derives "who is asking" — it only consumes a pre-clamped scope.
// See docs/cold-memory-trunk.md §1.
type CallerIdentity struct {
	// Handles are the caller's own source_handle strings (platformKind:uid). In a
	// private DM an identity-aware tool clamps reads to these so one personal user
	// cannot read another's history. Minimal version: one handle
	// (MessageEvent.AccountHandle); the account-merged full set lands later.
	Handles []string
	// Company is the caller's company scope ("" = personal / no agora). A
	// company-scoped tool clamps to it.
	Company string
	// Room is the shared-room scope when the turn is in a group ("" = a private DM /
	// no room). A tool recalling group history clamps to the room (everyone in it),
	// not to the caller's own handle — group members already see the whole room.
	Room string
}
