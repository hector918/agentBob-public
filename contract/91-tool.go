package contract

import (
	"context"
	"encoding/json"
)

// Tool is one dispatchable tool. The turn core looks it up by name from the
// turn's ToolSet and runs it; it does NOT authorize (the ToolSet is already the
// authorized projection — see docs/tools-warrant-credentials.md). Run returns a
// structured ToolResult; a returned error is the tool's INTERNAL failure, which
// the dispatcher wraps into an error ToolResult — never a loop error (spec I4).
type Tool interface {
	Spec() ToolSpec
	// Serialize reports whether this tool must NOT run concurrently with others
	// in the same round (shared mutable resource). false → may run in parallel.
	Serialize() bool
	Run(ctx context.Context, tc ToolContext, args json.RawMessage) ToolResult
}

// ToolContext is the per-turn context a tool's Run receives — the slim "actx".
// MINIMAL (slice 1): the session id + the reply sink (for progress traces). It
// grows as tools need it (the warrant handle for channels, the default space,
// the sender identity) — added when those land, not pre-provisioned.
type ToolContext struct {
	Sid string
	// Scope is the turn's session scope — a scope-aware tool (e.g. send_message) uses
	// it to resolve agora targets from the caller's position in the org graph. May be
	// "" outside a routed turn / for a flow that does not set it.
	Scope string
	Sink  Sink
	// UserText is the flow-joined user input for this turn — what the user asked. A
	// tool that records a visited URL to the library (scrapling / browser) uses it as
	// the recall query anchor (skeleton's actx.Event.Text), so a later web_search can
	// surface pages visited for a SIMILAR question. May be "" outside a turn.
	UserText string
	// Channels opens identity-bound space channels (file now, exec later). nil
	// when no warrant/space is wired (the tool reports the capability is absent).
	Channels ChannelOpener
	// Credentials resolves an identity-bound, kind-unique credential client (e.g.
	// the wordpress tool asks for kind="wordpress" and gets the *Client for the
	// site it's authorized for — never the secret). nil when no warrant is wired.
	Credentials CredentialOpener
	// Attachments is THIS turn's attachment bag as a capability (relayed from TurnSpec).
	// A media tool (ocr / vision / epub) supplies its appetite predicate and the set
	// resolves the model's reference to a real attachment's bytes — the model is blind to
	// the file, so it can only name what the prompt showed it. nil → no bag wired.
	Attachments AttachmentSet
	// Skills is the turn's authorized skill projection (the flow built it). skill_view
	// reads it; nil → no skills available.
	Skills SkillSet
	// Sub runs a blocking depth-1 sub-turn on a clean child context (the delegate
	// tool uses it). nil inside a sub-turn (depth 1) / when delegation is unwired.
	Sub SubRunner
	// History is a READ-ONLY conversation-log view: in a ShareHistory sub-turn it
	// is the delegating PARENT's conversation (the advisor); in a compacted MAIN
	// turn it is the turn's OWN log (the read-back, pull 化 P1). Non-nil does NOT
	// imply "inside a sub-turn".
	// The consuming tool (read_conversation_log) serializes/searches it per call;
	// nil → the capability is absent and the tool reports so. Never lets a tool
	// write history — reads only, via the session-owned store.
	History HistoryReader
	// Caller is the flow-resolved sender identity (handles / company / room). An
	// identity-aware tool (recall_memory) consumes it to clamp scope; the tool never
	// re-derives "who is asking". Zero value outside a routed turn / when the flow
	// did not fill it. See docs/cold-memory-trunk.md §4.
	Caller CallerIdentity
}

// ToolResult is one tool call's outcome. It serializes to a uniform JSON
// envelope (Envelope) at the model boundary so the model sees one shape and can
// self-correct (spec A4). ExitRequest, when non-nil, asks the turn to END with
// Reply (the clarify pattern — exit door ②).
type ToolResult struct {
	OK          bool
	Data        string // success payload (model-visible)
	Error       string // failure message
	Hint        string // recovery suggestion (success or error)
	ExitRequest *ToolExit
}

// ToolExit is a tool's request to end the turn, carrying the user-facing reply.
type ToolExit struct {
	Reply string
}

// OKResult builds a success result. ErrResult builds a failure result.
func OKResult(data string) ToolResult { return ToolResult{OK: true, Data: data} }
func ErrResult(msg string) ToolResult { return ToolResult{OK: false, Error: msg} }

// WithHint returns a copy with the recovery hint set.
func (r ToolResult) WithHint(hint string) ToolResult { r.Hint = hint; return r }

// Envelope renders the model-facing JSON: {"ok":true,"data":…} or
// {"ok":false,"error":…,"hint":…}. The single serialization boundary (A4) — the
// dispatcher feeds this back as the tool message's content.
func (r ToolResult) Envelope() string {
	var e struct {
		OK    bool   `json:"ok"`
		Data  string `json:"data,omitempty"`
		Error string `json:"error,omitempty"`
		Hint  string `json:"hint,omitempty"`
	}
	e.OK, e.Data, e.Error, e.Hint = r.OK, r.Data, r.Error, r.Hint
	b, err := json.Marshal(e)
	if err != nil {
		return `{"ok":false,"error":"result encode failed"}`
	}
	return string(b)
}

// ToolSet is the turn's authorized tool projection: the specs to advertise to
// the model + lookup-by-name for dispatch. The full catalog (ToolCatalog) is a
// ToolSet; warrant's per-identity authorized subset is also a ToolSet (same type,
// narrowed). The turn core holds one and only run()s from it — never judges.
type ToolSet interface {
	Specs() []ToolSpec
	Lookup(name string) (Tool, bool)
}

// ToolCatalog is the full set of registered tools, provided by leaf/tools. It IS
// a ToolSet; the flow projects from it (and, when warrant lands, authorizes the
// projection into the per-turn ToolSet).
type ToolCatalog interface {
	ToolSet
}

// HistoryReader is the narrow read-only face a sub-turn tool gets onto its
// delegating parent's conversation (ToolContext.History). Chronological order;
// limit<=0 → all. Implemented by the turn core as a closure over the session's
// MessageStore + the parent sid — no consumer ever sees the store itself.
type HistoryReader interface {
	Messages(ctx context.Context, limit int) ([]Message, error)
}
