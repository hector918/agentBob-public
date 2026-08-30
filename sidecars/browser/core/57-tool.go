package core

import (
	"context"
	"encoding/json"
)

// Tier is the runtime gating mode of a tool — docs/agent-loop-design.md §4.
// As of (DEC-2 cleanup) Tier only encodes "does dispatch
// pause for a /approve flow?" — authorization is decided by
// permissions.yaml via core.Gate (NewGate in agentbob/permissions).
// Visibility filtering also goes through gate.Visible, not Tier.
//
//   - TierUser            🟢 runs unconditionally if the gate allows
//   - TierTargetConsent   🟠 dispatch prompts the TARGET user for /approve
//     (no tool currently returns this; kept as a seam, exercised by
//     approvals/00-manager_test.go)
//   - TierAdminApproval   🔴 dispatch prompts an admin for /approve (e.g. dangerous ops)
//
// The historical TierAdmin tier (an IsAdmin check at dispatch) was
// deleted: permissions.yaml + grants now own who-may-do-what; the
// dispatch-time tier label was a parallel mechanism that could only
// drift against yaml. Tools previously at TierAdmin are TierUser now;
// permissions.yaml restricts them by role.
type Tier int

const (
	TierUser Tier = iota
	TierTargetConsent
	TierAdminApproval
)

// AgentCtx is what a tool handler sees about the turn it runs inside. Each
// call to Tool.Run gets a fresh AgentCtx populated by the loop. Heavier
// dependencies (Memory, SkillRegistry, ConsentManager, ModelPool) are added
// as fields when their subsystems land.
type AgentCtx struct {
	SessionScope string
	SessionID    string // db row id (for tools that read history directly)
	Event        MessageEvent
	IsAdmin      bool
	// Iter is the current loop iteration (0-based). Used by:
	//   - agent-orchestrator/40-dispatch.go — structured slog "iter" key
	//     on tool-call / tool-result audit lines
	//   - prompt/30-assembler.go — slog "iter" key on
	//     prompt build trace
	//   - turnlc — flows through TurnEvent.Iter to
	//     the per-turn distill pipeline
	//
	// Also reserved as a hook for future tools that need to know "am I
	// being called for the first time this turn or N-th?" (e.g. a tool
	// that refuses retries). Audit G1; consumer list
	// updated per DECISION 1.2 (was incorrectly documented
	// as "no tool consumes it" — fields like this get deleted silently
	// if the comment stays out of date).
	Iter  int
	Store SessionStore
	// Phase 4: ActiveSkills field removed; admin /skill
	// activate path replaced by model-driven skill_view + skills-index.
	//
	// PR-1: SessionAudiences field removed — the audience
	// tag layer (admin/user/worker tags + tool_audiences yaml +
	// audienceMatch) is gone in favour of grants-only visibility via
	// core.Gate. See docs/permissions-design.md for the new model.
	//
	// InboxID is the agora inbox this turn's chat scope routes to (chat-
	// scoped model: resolved per-turn from chat → inbox_sources, carried
	// via AgentRequest.InboxID). "" for non-agora turns. The single source
	// the agora identity / visibility / company resolution reads from
	// (OrgCache.InboxByID) — computed once, so all three agree. See
	// docs/agora.md §一·五.
	InboxID string
	// CompanyID is the agora Company this session belongs to, derived from
	// the turn's InboxID → Employment.CompanyID (or bridge inbox's
	// OwnerCompanyID). "" for non-agora / legacy sessions. Populated at
	// makeActx time so per-session helpers (send_message etc.) find the
	// caller's Company without re-walking.
	CompanyID string
	// CompanyName is the human-readable, UNIQUE-indexed name of the turn's
	// Company (same OrgCache walk as CompanyID, then Company.Name). "" for
	// non-agora / legacy sessions. It is the addressing key for file_storage's
	// shared spaces — colleagues default to their company's named space
	// (docs/spaces.md §2/D9); CompanyID is opaque so the readable Name is
	// what the model passes and what the space dir is keyed on.
	CompanyName string
	// AccountID is the cross-entry account the turn's triggering sender is bound
	// to (from AgentRequest.AccountID). "" for an unbound sender / accounts off.
	// agora reads it for the optional "no account → no service" gate + name
	// lookup. See docs/accounts.md §13.2.
	AccountID string
	// MemberID is the agora Member this turn ACTS AS (Inbox → Employment →
	// Member), populated at makeActx from the turn's InboxID. "" for non-agora.
	// The per-member browser-profile key: in agora the browser's login belongs
	// to the member acting, not the triggering person (AccountID), so the
	// browser profile is keyed by MemberID when set, AccountID otherwise. See
	// docs/remote-browser-handoff.md.
	MemberID string
	// Language is the triggering sender's account reply-language preference
	// (from AgentRequest.Language). "" = unset; non-empty → the prompt's
	// LanguageSource injects a "reply in <Language>" line. docs/accounts.md §13.4.
	Language string
	// Visibility is the per-Inbox tool/skill filter for this turn,
	// resolved from the session's Inbox at makeActx time. nil = no
	// filter (no Inbox or Inbox has no Visibility set). See
	// core.InboxVisibility. PR-2.
	Visibility *InboxVisibility
	// VisibleToolSpecs is the tool list the model sees THIS turn — the
	// output of ToolRegistry.SpecsForSession (session-aware ∩ grants
	// ∩ inbox.Visibility), stashed on actx by assembleFromHistory just
	// before BuildMain. The prompt's ToolPolicySource reads it to assemble
	// the "工具按场景选" rubric from exactly the visible tools' SelectionHints
	// — so the rubric never mentions a tool the model can't call, and is
	// identical for the real turn and the /prompt dump (same funnel). nil /
	// empty (e.g. offline `bob debug prompt`) → rubric renders empty, only
	// the cross-cutting appendix shows. Transient; never persisted.
	VisibleToolSpecs []ToolSpec
	// URLLibrary is bob's URL memory layer. May be nil at construction
	// time when urllib is disabled; tools should nil-check before use.
	// Slice 7. See urllib.
	URLLibrary URLLibrary
	// Approvals lets a tool's Run() gate sensitive operations *dynamically*
	// (audit D-residual). For static gating, set Tier=TierAdminApproval —
	// the dispatcher prompts before invoking Run(). For arg-aware gating
	// (e.g. terminal looking at `rm -rf` in the cmd), do the check inside
	// Run() via Approvals.Register / Unregister + select on the channel.
	// May be nil when the gateway didn't wire one (tools nil-check).
	Approvals ApprovalRequester
	// Sink is the current turn's reply Sink. Tools use it to emit the
	// "🔐 approval needed: ..." prompt before blocking on Approvals.
	// May be nil for synthetic / replay calls; tools nil-check.
	Sink Sink
	// FileSender delivers a produced file to the user for this turn. Wired by
	// the gateway (closure over the Source + this turn's Target). May be nil
	// for synthetic / replay calls; tools nil-check.
	FileSender FileSender
	// ModelPool is the agent's model pool, exposed so tools can make
	// side-LLM calls (e.g. browser_vision asking a vision-capable
	// entry to describe a screenshot). The standard `Tools` field on
	// ModelRequest stays empty for these calls — vision lookup, not
	// another agent turn. May be nil for synthetic / replay calls.
	ModelPool ModelPool
	// SideRunner is the agent-orchestrator dispatcher for side-LLM
	// operations (compression, judge, learning-analyse, etc.). Tools
	// that need a side-LLM call should prefer it over hand-building
	// BuildSide + ModelPool.Chat — RunSide funnels prompt assembly +
	// audit logging through one path. May be nil for synthetic /
	// replay calls + on test harnesses that didn't wire one; callers
	// nil-check and fall back to ModelPool when unavoidable.
	// Phase 3b.
	SideRunner SideRunner
	// Confirmer is the per-turn "post the user a question with buttons"
	// helper (#100). It is asynchronous: Confirmer.Post sends the buttons
	// and returns immediately; the answer arrives later as a callback
	// (the synchronous Ask track was removed — D18). May be
	// nil when the source / hub didn't wire one — callers should nil-check
	// and degrade (return action_required envelope; let the model relay in
	// natural language).
	Confirmer Confirmer
	// TurnState is the lifecycle module's per-turn working memory
	// (turnlc.TurnState). Typed as `any` here so core stays free of a
	// turnlc import; tools that care type-assert
	// in their own package. nil = no lifecycle wired (test paths /
	// synthetic calls). C3 fix — skill_view uses this for
	// in-turn dedup short-circuit.
	TurnState any
	// TurnID is the current turn's uuid (turnlc-supplied; "" when
	// turnlc is not wired). Tools that mark per-turn state (skill_view
	// dedup) pass this through to the lifecycle module so the recorded
	// info is tied to the right turn.
	TurnID string
	// Turn is the opaque per-turn coordinator handle (*turnlc.TurnCtx),
	// typed `any` so core stays free of a turnlc import. A6
	// (docs/loop-core-spec.md §3.6): the TurnCtx travels by parameter
	// instead of a sid-keyed registry; tools that need turn-scoped
	// services (report's actions summary) type-assert this in their own
	// package. nil = no lifecycle wired.
	Turn any
	// SubAgent runs a blocking delegated sub-loop (docs/subagent.md).
	// Wired by makeActx to the agent itself; the delegate tool is its
	// only consumer. May be nil for synthetic / replay calls + test
	// harnesses; tools nil-check.
	SubAgent SubAgentRunner
}

// PromptKey is the key under which the prompt module stashes per-turn
// state (nudges + history) for this context. It returns TurnID when
// available so concurrent turns sharing one SessionScope (v0.2
// multi-session) get isolated state — see R16. Falls back to
// SessionScope on the turnlc-nil debug/test path, which is
// single-threaded so scope-keying is correct there.
func (c AgentCtx) PromptKey() string {
	if c.TurnID != "" {
		return c.TurnID
	}
	return c.SessionScope
}

// FileSender is the narrow per-turn helper a tool uses to deliver a
// produced file back to the user. The gateway wires it as a closure
// over the inbound Source + this turn's reply Target, so a tool need
// not know either. SendFile's error means REJECTION (mirrors
// Source.SendFile / Source.Send), not a delivery confirmation.
type FileSender interface {
	SendFile(ctx context.Context, path, caption string) error
}

// URLLibrary is the contract AgentCtx exposes to tools. The full
// interface lives in urllib; this is the subset
// tools and the dispatcher actually call. Kept here to avoid a
// circular import (urllib uses core.MessageEvent indirectly via the
// agent, while core can't import urllib).
type URLLibrary interface {
	Record(ctx context.Context, url, title, excerpt, query string)
	Recall(ctx context.Context, query string, max int) []URLCandidate
	SearchEngines() []URLCandidate
}

// URLCandidate mirrors urllib.Candidate at the core level. Same shape.
type URLCandidate struct {
	URL     string
	Title   string
	Excerpt string
}

// ToolStatus is the structured outcome class of a ToolResult (A4,
// docs/loop-core-spec.md §2). The zero value is ToolStatusRaw so every
// pre-A4 construction (ToolResult{Content: "..."}) keeps its exact legacy
// meaning: Content holds the model-visible bytes verbatim.
type ToolStatus int

const (
	// ToolStatusRaw — legacy/passthrough: Content is the model-visible text
	// as-is (plain string, pre-rendered envelope, or post-pipeline bytes).
	ToolStatusRaw ToolStatus = iota
	// ToolStatusOK — structured success; Data carries the payload.
	ToolStatusOK
	// ToolStatusErr — structured failure; Error (+ optional Category) carries it.
	ToolStatusErr
)

// ToolResult is what a tool returns to the dispatcher.
//
// A4 (docs/loop-core-spec.md §2): results are constructed STRUCTURED via
// OKResult / ErrResult / ErrResultCategory and serialized to the D1 JSON
// envelope exactly once, at the dispatcher chokepoint (sealResult →
// Envelope). In-process consumers (turn lifecycle, trace UI, audit) read
// the typed fields instead of re-parsing JSON.
//
//   - Status/Data/Error/Category/Hint — the structured outcome. Valid from
//     construction until the dispatcher seals the result; after sealing they
//     remain as the construction-time view.
//   - Content      — the authoritative model-visible bytes: persisted as the
//     role:"tool" message and fed back to the model. Empty on a freshly
//     constructed structured result; the dispatcher seals it from
//     Envelope() BEFORE the result pipeline (belt cap / scrub / compress
//     mutate Content only, never the typed fields).
//   - ExitRequest  — nil = continue. Non-nil = end the turn with this exit
//     state. For clarify-style "ask user and stop", set
//     State=Flow34ExitToolRequested and Detail = the
//     user-facing reply. Content still gets persisted as the
//     tool row so a future replay sees a coherent
//     assistant-tool pair.
type ToolResult struct {
	Status   ToolStatus
	Data     string
	Error    string
	Category string
	Hint     string

	Content     string
	ExitRequest *TurnExit
}

// OKResult builds a structured success result. data is whatever string most
// helps the model continue (text / JSON / base64); empty is allowed for
// side-effect tools whose result is just "it worked". The optional hint
// nudges the model on success.
func OKResult(data string, hint ...string) ToolResult {
	r := ToolResult{Status: ToolStatusOK, Data: data}
	if len(hint) > 0 {
		r.Hint = hint[0]
	}
	return r
}

// ErrResult builds a structured failure result. msg is the model-readable
// reason; the optional hint is a recovery suggestion kept in its own field
// so the model can tell failure from advice.
func ErrResult(msg string, hint ...string) ToolResult {
	r := ToolResult{Status: ToolStatusErr, Error: msg}
	if len(hint) > 0 {
		r.Hint = hint[0]
	}
	return r
}

// ErrResultCategory is ErrResult plus a machine-readable category tag
// ("config" / "timeout" / "transport" / "image_format" / ...) for
// downstream tools / skills to branch on. Empty category renders the same
// envelope as plain ErrResult.
func ErrResultCategory(msg, category string, hint ...string) ToolResult {
	r := ErrResult(msg, hint...)
	r.Category = category
	return r
}

// Envelope renders the model-visible bytes for this result.
//
// Raw results return Content verbatim. Structured results marshal the D1
// envelope through a map — exactly the construction the legacy prompt-side
// helpers used — so the serialized bytes (alphabetical key order from Go's
// map marshaling) are byte-identical to the pre-A4 output. Do not "tidy"
// this into a struct marshal: field-order output would change the bytes
// the model (and any prefix cache) sees.
func (r ToolResult) Envelope() string {
	switch r.Status {
	case ToolStatusOK:
		m := map[string]any{"ok": true, "data": r.Data}
		if r.Hint != "" {
			m["hint"] = r.Hint
		}
		b, _ := json.Marshal(m)
		return string(b)
	case ToolStatusErr:
		m := map[string]any{"ok": false, "error": r.Error}
		if r.Category != "" {
			m["category"] = r.Category
		}
		if r.Hint != "" {
			m["hint"] = r.Hint
		}
		b, _ := json.Marshal(m)
		return string(b)
	default:
		return r.Content
	}
}

// IsErr reports whether this result is a failure. Structured results answer
// from Status; Raw results fall back to parsing Content for an explicit
// {"ok":false} envelope — the SINGLE remaining parse point (non-envelope /
// malformed content is treated as success, matching the legacy dispatcher
// and turn-lifecycle semantics).
func (r ToolResult) IsErr() bool {
	switch r.Status {
	case ToolStatusOK:
		return false
	case ToolStatusErr:
		return true
	}
	var env struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(r.Content), &env); err != nil || env.OK == nil {
		return false
	}
	return !*env.OK
}

// HintSet reports whether the result carries a non-empty hint (structured
// field, or the "hint" key of a Raw envelope).
func (r ToolResult) HintSet() bool {
	if r.Status != ToolStatusRaw {
		return r.Hint != ""
	}
	var env struct {
		Hint *string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(r.Content), &env); err != nil {
		return false
	}
	return env.Hint != nil && *env.Hint != ""
}

// Tool is one runnable tool. Spec is what the model sees; Run is the handler.
// The dispatcher validates the model's arguments against Spec.Parameters and
// checks Tier before invoking Run.
type Tool interface {
	// Spec — name, description, JSON Schema for parameters.
	Spec() ToolSpec
	// Tier — permission gating.
	Tier() Tier
	// Serialize — if true, this tool must NOT run in parallel with siblings in
	// the same iteration (e.g. a future `terminal`). The dispatcher honors it.
	Serialize() bool
	// Run executes the tool. args is the raw JSON the model emitted (already
	// validated, in principle — Run can still defensive-parse).
	Run(ctx context.Context, actx AgentCtx, args json.RawMessage) (ToolResult, error)
}
