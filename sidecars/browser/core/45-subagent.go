package core

import (
	"context"
	"strings"
)

// Sub-agent delegation primitives (docs/subagent.md). A delegated sub-loop
// runs the existing tool loop on a child session id under the PARENT's
// session scope — the parent turn blocks and receives only the final
// product text. core holds the narrow contracts so the tool package and
// the orchestrator never import each other (same pattern as SideRunner).

// SubSessionIDMarker is the substring every sub-session id carries:
// sub sids are named "<parent sid>:sub:<n>" by SubSessionStore. Regular
// sids come from newID() and can never contain it, so the marker doubles
// as the depth-1 guard key (a delegate call from inside a sub-loop is
// detected by its own sid).
const SubSessionIDMarker = ":sub:"

// IsSubSessionID reports whether sid names a delegated sub-session.
func IsSubSessionID(sid string) bool {
	return strings.Contains(sid, SubSessionIDMarker)
}

// SubAgentSpec is one delegated task, assembled by the delegate tool from
// the model's arguments. The task brief must be self-contained — the sub
// context gets NO parent history, memory, or persona (docs/subagent.md §5).
type SubAgentSpec struct {
	// Task is the parent-authored task brief, seeded as the sub context's
	// first user message.
	Task string
	// Suite names the fixed tool set the sub-loop advertises ("browser").
	// Fixed named collections only — never a free-form tool-name list.
	Suite string
	// SkillNames / SkillBodies are the parent-named skills, already
	// resolved (gate-checked + frontmatter-stripped) by the caller.
	// Parallel slices; bodies are injected into the slim system prompt.
	SkillNames  []string
	SkillBodies []string
	// Images are the image bytes attached to the sub context's seed user
	// message (the image-map driver, docs/subagent.md §3 scene ②). Store
	// history is text-only, so the runner keeps these on the live sub-loop
	// registration and re-attaches them on every message build. The Task
	// text must carry the describeAtt-style filename line itself —
	// multimodal APIs transmit bytes+MIME only (§6.7).
	Images []ImageRef
}

// SubAgentRunner runs one blocking sub-loop and returns the product text.
// Implemented by the agent orchestrator; exposed to tools via
// AgentCtx.SubAgent (nil when unwired). The error return is for harness-
// level refusals (depth limit, store unavailable); loop-level failures
// (iter cap, wall-clock timeout, panic) come back as an honest partial
// product in the string instead (fail-open, docs/subagent.md D3), with
// partial=true so callers can mark the product as incomplete rather than
// presenting it as a clean finish.
type SubAgentRunner interface {
	RunSubAgent(ctx context.Context, actx AgentCtx, spec SubAgentSpec) (out string, partial bool, err error)
}

// SubSessionStore creates delegated sub-session rows. Optional store
// capability (assert from SessionStore, like SessionArchiver): the row is
// the audit carrier for one delegated sub-loop — kind='sub' keeps it out
// of scope routing / learning sweep / alive-session listings, while the
// normal archive retention still covers it (docs/subagent.md D1).
type SubSessionStore interface {
	// CreateSubSession inserts a session row with id
	// "<parentSid>:sub:<n>" (n = 1-based per-parent counter),
	// parent_session_id=parentSid and kind='sub'. scope is the PARENT's
	// session scope (kept for audit; never routable thanks to the kind
	// filter).
	CreateSubSession(ctx context.Context, parentSid, scope, source, userID string) (string, error)
}
