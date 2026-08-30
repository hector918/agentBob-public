package core

import "context"

// AgentRequest is one turn's input for the agent.
//
// PR 5 of session-module split: Event → Batch +
// TranscriptOnly removed. Turn now builds the synthesized prompt
// text + attachments + transcript-only check itself from Batch via
// prompt.BuildUserText / prompt.MergeAttachments / prompt.IsTranscriptOnlyTurn
// at the Respond entry. The gateway just hands over the raw batch.
type AgentRequest struct {
	SessionScope string
	// Batch is the raw event(s) coalesced for this turn. len(Batch) >= 1.
	// Turn derives the synthesized user-turn text + merged attachments
	// from Batch at Respond entry; the freshest event (Batch[len-1])
	// carries per-turn metadata (lang, source, user, required model tags).
	Batch []MessageEvent
	// SessionID is an optional pin: when set, the agent writes/reads against
	// THIS exact session_id rather than the key's current latest generation.
	// Used for audit A-Q2 routing: replying to an old bot msg in
	// message_index resolves to the specific session_id that msg belonged
	// to, so the conversation continues in its original history even when
	// /new has since rolled the key to a fresh generation. Empty → use
	// EnsureSession(key) — the path for new threads and DM messages.
	SessionID string
	// InboxID is the agora inbox this turn's chat scope routes to. Under the
	// chat-scoped model the session key is the real chat scope; the inbox is
	// resolved per-turn from chat → inbox_sources by the gateway (runTurn)
	// and carried here. "" for non-agora turns / agora-disabled. makeActx
	// stashes it on AgentCtx.InboxID; the agora identity / visibility /
	// company resolution reads it (OrgCache.InboxByID) instead of parsing
	// the scope. See docs/agora.md §一·五.
	InboxID string
	// AccountID is the cross-entry account this turn's triggering sender
	// (Batch[0]) is bound to — resolved once at the gateway (runTurn) from the
	// sender's handle; "" for an unbound sender / accounts off. makeActx stashes
	// it on AgentCtx.AccountID; agora reads it for the optional "no account → no
	// service" gate + display-name lookup. NOT used for usage accounting — that
	// keys on the raw handle (Batch[0] source+uid). See docs/accounts.md §13.2.
	AccountID string
	// Language is the triggering sender's account reply-language preference
	// (/language), resolved once at the gateway (runTurn); "" when unset /
	// unbound / accounts off. Non-empty → overrides this turn's auto-detected
	// i18n lang AND adds a prompt line telling the model to reply in it (makeActx
	// → AgentCtx.Language). docs/accounts.md §13.4.
	Language string
	// Confirmer is the per-turn "post the user a question with buttons"
	// helper used by tools that want a yes/no decision (#100, todo's
	// mode-switch). It is asynchronous: Post returns immediately and the
	// answer arrives later as a callback (the synchronous Ask track was
	// removed — D18). Hub builds it from the inbound source +
	// callbacks.Manager. nil → tools that need it fall back to returning
	// an action_required envelope and let the model relay in plain text.
	Confirmer Confirmer
	// FileSender delivers a produced file back to the user. Hub builds it
	// as a closure over the inbound Source + this turn's reply Target;
	// the agent loop copies it onto every tool call's AgentCtx. nil →
	// tools that need it nil-check and degrade.
	FileSender FileSender
}

// MetaEvent returns the per-turn metadata event — the freshest event
// in the batch. Used for stable metadata reads (source, user, lang,
// required model tags) that are the same across every batch element.
// Returns a zero MessageEvent when Batch is empty (test fixtures that
// don't set Batch shouldn't crash deep inside agent helpers).
func (r AgentRequest) MetaEvent() MessageEvent {
	if len(r.Batch) == 0 {
		return MessageEvent{}
	}
	return r.Batch[len(r.Batch)-1]
}

// Agent produces a reply for an inbound message and renders it through the Sink.
//
// The full agent loop (prompt assembly → model call → tool dispatch → compression → …)
// lives in agent-orchestrator. Right now it is minimal: per-session history in the
// Store + one streaming model call per turn. No tools yet.
type Agent interface {
	// Respond handles one inbound message: it streams the reply to sink (Delta…
	// then Finish), persists it, and returns the complete reply text (for logging).
	// On error it returns ("", err) WITHOUT calling Finish — the caller renders the error.
	Respond(ctx context.Context, req AgentRequest, sink Sink) (string, error)
	// DumpLivePrompt builds the exact live prompt req would produce and sends
	// it to sink WITHOUT calling the model and without any store write (admin
	// /prompt mode). Invoked by the gateway in place of Respond when the scope
	// is in dump mode. Returns an error only for logging (it surfaces failures
	// to the sink itself).
	DumpLivePrompt(ctx context.Context, req AgentRequest, sink Sink) error
}

// (Agent.Reset was deleted, review #4 [7]: /new has gone
// through pipeline-session's OpenSession since the v0.2 multi-session
// model — the method had zero call sites and its doc blessed a dead
// path. NewGeneration's sole consumer is now the corruption escape
// hatch.)
