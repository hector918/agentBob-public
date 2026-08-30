// leaf/session/55-queue.go is the busy-queue: the user-facing behaviour for
// messages that arrive while a turn is in flight. It is the trunk re-home of
// skeleton's busy-todo (pipeline-session/55-busy-todo-seed.go) — but stripped of
// the todo subsystem it used to parasitise (UpdateTodos / core.Todo /
// BusyTodoIDPrefix / RenderTodos). Here the queue IS the arbiter's existing
// pending state: m.pending[sid] (in-memory) + the durable WAL rows (crash
// recovery). This file only adds the visible layer on top — dedup, the queued /
// running-count / cleared / full notices — and the nudge fast-lane (takeNudge),
// all gated by the session's flow-declared mode (auto → silent + no nudge).
package session

import (
	"context"
	"log/slog"
	"strings"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
	"agentbob/i18n"
)

// queueContent builds the dedup content key for a busy-arrived event: the
// trimmed caption plus any attachment filenames. The filename suffix keeps an
// album's PHOTO siblings distinct (per-message-id filenames) — non-photo
// siblings (video/document/audio) may share a static fallback name, which is why
// queueDedup additionally requires a matching message id for attachment-bearing
// events. Returns "" when there is no caption — an uncaptioned attachment is not
// deduped here; it simply rides the pending queue and drains.
func queueContent(ev contract.MessageEvent) string {
	caption := strings.TrimSpace(ev.Text)
	if caption == "" || caption == NoCaptionStandIn {
		return ""
	}
	var names []string
	for _, a := range ev.Attachments {
		if a.FileName != "" {
			names = append(names, a.FileName)
		}
	}
	if len(names) == 0 {
		return caption
	}
	return caption + " [" + strings.Join(names, ", ") + "]"
}

// queueDedup reports whether m.pending[sid] already makes ev a duplicate. Caller
// holds m.mu. Plain-text events dedup by content (a user double-tapping the same
// message while a turn runs). An attachment-bearing event dedups ONLY on a true
// redelivery (same message id): a captioned album's siblings share one caption
// and — outside the photo path — a static fallback filename ("video.mp4", …), so
// content equality alone would swallow a genuinely different file. An empty
// content never dedups (uncaptioned attachments are always queued — each is its
// own work item).
func (m *Manager) queueDedup(sid string, ev contract.MessageEvent) bool {
	content := queueContent(ev)
	if content == "" {
		return false
	}
	for _, e := range m.pending[sid] {
		if queueContent(e) != content {
			continue
		}
		// Dedup is a per-SENDER double-tap guard: two different people in a group
		// both queuing "继续" are distinct inputs, not a repeat — dropping one would
		// lose the second speaker's turn (and their per-speaker row). Only a resend
		// from the SAME sender is a dedup (F95).
		if e.UserID != ev.UserID {
			continue
		}
		if len(ev.Attachments) > 0 || len(e.Attachments) > 0 {
			if ev.MessageID != "" && e.MessageID == ev.MessageID {
				return true // true redelivery of the same message
			}
			continue // same caption, different message → a distinct sibling file
		}
		return true
	}
	return false
}

// notifies reports whether the busy-queue should send chat notices under mode.
// Only the two explicitly-visible modes notify; auto is silent, and an UNKNOWN /
// empty mode (read in the small race window before the turn caches it on sidMeta)
// is treated as silent too — never spuriously notify what might be an auto session.
func notifies(mode contract.SessionMode) bool {
	return mode == contract.SessionModeManual || mode == contract.SessionModeNone
}

// notifyQueued sends the busy-queue notice for an event just appended to the
// pending batch at the given depth: the first of a wave gets the "queued" notice,
// later ones a running-count ack. Silent unless mode notifies. Best-effort — an
// independent Send that never blocks dispatch (the 👀 read-ack is separate).
func (m *Manager) notifyQueued(ctx context.Context, ev contract.MessageEvent, mode contract.SessionMode, scope string, depth int) {
	// F152: a provisional (onboarding-pass) sender's session runs intro (mode=none, which
	// notifies), but intro is "one canned reply then silent" — a busy-queue notice on top
	// of that is misleading spam, so stay quiet for them regardless of mode.
	if ev.Onboarding || !notifies(mode) {
		return
	}
	src := m.srcByName(ev.Source)
	if src == nil {
		return
	}
	text := i18n.T("system.queue_queued", ev.Lang)
	if depth > 1 {
		text = i18n.T("system.queue_ack_n", ev.Lang, depth, m.config.MaxBatch)
	}
	if err := src.Send(ctx, m.replyTarget(ev, src), text); err != nil {
		slog.Debug("queue: queued-notice send failed", "scope", scope, "err", err)
	}
}

// notifyQueueFull tells the chat its message was dropped because the pending
// batch overflowed (the cap is MaxBatch). Silent under auto. Best-effort.
func (m *Manager) notifyQueueFull(ctx context.Context, ev contract.MessageEvent, mode contract.SessionMode, scope string) {
	// F152: stay silent for a provisional onboarding sender (see notifyQueued).
	if ev.Onboarding || !notifies(mode) {
		return
	}
	src := m.srcByName(ev.Source)
	if src == nil {
		return
	}
	if err := src.Send(ctx, m.replyTarget(ev, src), i18n.T("system.queue_full", ev.Lang, m.config.MaxBatch)); err != nil {
		slog.Debug("queue: queue-full notice send failed", "scope", scope, "err", err)
	}
}

// notifyCleared sends the "handled N queued messages" ack after a DRAIN turn
// served a batch that had been queued behind a previous turn. Silent under auto
// or when nothing was queued. Best-effort.
func (m *Manager) notifyCleared(ctx context.Context, src contract.Source, target contract.Target, lang string, mode contract.SessionMode, count int) {
	if !notifies(mode) || count <= 0 || src == nil {
		return
	}
	if err := src.Send(ctx, target, i18n.T("system.queue_cleared", lang, count)); err != nil {
		slog.Debug("queue: cleared-ack send failed", "err", err)
	}
}

// nudgeEligible reports whether an event can be pulled into a running turn as a
// nudge: PLAIN TEXT only (no attachments — images/voice need preprocess and stay
// queued for the image-drain path), with non-empty text.
func nudgeEligible(ev contract.MessageEvent) bool {
	// An internal event (worker return / agora dispatch / cron wake) is a SYSTEM
	// reply, not a human tapping out a follow-up — nudging it would impersonate the
	// user AND skip history (a nudge has no WAL row and its live reply is its only
	// trace). Keep internals out of the fast-lane so they take the normal drain path
	// (their own turn, persisted) (F94).
	if ev.IsInternal() {
		return false
	}
	if len(ev.Attachments) > 0 {
		return false
	}
	t := strings.TrimSpace(ev.Text)
	return t != "" && t != NoCaptionStandIn
}

// takeNudge pops the next nudge-eligible event from sid's pending queue — the
// first plain-text message — and returns it as a UserMsg, removing it from the
// in-memory queue (a busy-queued message has no WAL row to drop — 40-submit.go).
// Exactly-once: a nudged message is removed from pending, so a later drain never
// re-serves it. What makes that safe is the CALLER: the turn core persists the
// returned UserMsg as a real user row before showing it to the model (80-nudge.go),
// so the message survives in history whether or not the model addresses it — the
// old contract ("its live reply IS its acknowledgement") was an assumption nothing
// verified, and a model that ignored the nudge destroyed the message. Returns nil
// when nothing is eligible. The arbiter wires this as TurnWork.NextNudge for
// non-auto sessions (passing the session scope); the turn core calls it at loop-top
// (at most once per turn).
func (m *Manager) takeNudge(sid, scope string, preTurn map[string]bool) *contract.UserMsg {
	m.mu.Lock()
	q := m.pending[sid]
	idx := -1
	for i, e := range q {
		// F146: skip messages already queued when this turn started (preTurn) — the
		// nudge fast-lane is for a GENUINE mid-turn interjection, not for reordering the
		// pre-queued backlog (e.g. a text waiting behind an unprocessed image), which
		// the drain handles in order.
		if nudgeEligible(e) && !preTurn[e.MessageID] {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return nil
	}
	ev := q[idx]
	// Remove idx while preserving order; the 3-index cap forces append to allocate
	// rather than alias q's backing array.
	m.pending[sid] = append(q[:idx:idx], q[idx+1:]...)
	if len(m.pending[sid]) == 0 {
		delete(m.pending, sid)
	}
	// Track the msg id so removeServedPending includes it at turn SUCCESS, uniform with
	// the served batch. A busy-queued message has no WAL row, and boot recovery
	// re-sends nothing — so on a mid-turn crash this nudge is simply dropped (the
	// sender re-sends), like the turn's in-flight batch. The in-memory pending removal
	// above already prevents a double-drain.
	if ev.MessageID != "" {
		m.nudged[sid] = append(m.nudged[sid], ev.MessageID)
	}
	m.mu.Unlock()
	// The one durable-ish trace of the fast-lane: a nudged message leaves no WAL row,
	// and until the turn persists its user row there is nothing to look at. Cheap
	// (once per turn at most) and it is the line that tells a later investigation the
	// lane fired at all.
	slog.Info("queue: nudge pulled a busy-arrived message into the running turn", "scope", scope, "sid", sid, "msg_id", ev.MessageID)
	return &contract.UserMsg{
		// A nudge is never internal (nudgeEligible rejects those), so the author is
		// always the human speaker — the same attribution flow/compose stamps on a
		// normally-drained user row.
		Author: contract.MsgAuthor{Kind: "human", ID: ev.UserID, Name: ev.UserName},
		Text:   nudgeText(ev),
	}
}

// nudgeText renders a nudge event's CONTENT the way the normal drain path renders it:
// the quoted-reply line (joinEvents' prompt.ReplyLine) in front of the text, so a
// reply-to interjection still says what it is replying to.
//
// It deliberately does NOT add the "[名字]: " speaker prefix any more. It used to
// (F143), because the old fast-lane injected the nudge into the SYSTEM PROMPT, which
// skips renderSpeakers entirely — so attribution had to be baked into the string.
// A nudge is now persisted as a real user row carrying its Author, so
// renderSpeakers (leaf/turn/10-core.go) attributes it at build time exactly like
// every other user row — including the "<2 distinct speakers → no prefix" rule that
// a baked-in prefix could not honour. Baking it in as well would render "[名字]: [名字]: …".
func nudgeText(ev contract.MessageEvent) string {
	text := strings.TrimSpace(ev.Text)
	line := prompt.ReplyLine(ev)
	switch {
	case line == "":
		return text
	case text == "":
		return line
	default:
		return line + "\n" + text
	}
}
