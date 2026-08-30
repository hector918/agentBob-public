package session

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/i18n"
)

// NoCaptionStandIn is synthesised as the text of an attachment-only message so
// the turn has something to act on. The literal lives in contract (shared
// vocabulary with flow/compose's ASR merge rule).
const NoCaptionStandIn = contract.NoCaptionStandIn

// Submit accepts one inbound event for dispatch. It returns promptly — the work
// runs on its own goroutine — so the caller's consume loop is never blocked.
func (m *Manager) Submit(ctx context.Context, ev contract.MessageEvent) {
	m.handleWg.Add(1)
	go func() {
		defer m.handleWg.Done()
		m.submit(ctx, ev)
	}()
}

func (m *Manager) submit(ctx context.Context, ev contract.MessageEvent) {
	src := m.srcByName(ev.Source)
	if src == nil {
		slog.Warn("event from unknown source", "source", ev.Source)
		return
	}
	// ev.Lang is stamped once at ingress (the inbound flow); downstream reuses it.
	resolution := m.ResolveSession(ctx, ev)
	if resolution.Drop {
		// agora disabled-company drop (silent). Never set today (overlay lands
		// with agora), but honoured for forward-compat.
		return
	}
	scope := resolution.Key

	// Voice-preprocess (F165): transcribe audio to TEXT here — before placement clears
	// LocalPath and before the user row is persisted — and FOLD it into ev.Text. Spoken
	// content then lives in the message text for every downstream reader (prompt, stored
	// history, recall feed) as ordinary text; there is no separate transcript field /
	// "[语音转写]" section.
	//
	// TWO halves, two POSITIONS (the split itself lives in leaf/asr, which is the only
	// place that can see whose words these are):
	//   instruction — the sender's own captionless voice note. It IS the request, so it
	//     becomes the message text, exactly as if typed.
	//   material    — a captioned clip, a replied-to message's clip, a video soundtrack.
	//     Somebody else's words, so they go AFTER whatever the user actually said and
	//     never replace it. A captionless message keeps its stand-in as the text and the
	//     material follows: "no words of their own, and here is what the file contains"
	//     is the honest rendering, and dropping the stand-in would silently promote the
	//     material into the request.
	if m.config.Transcribe != nil {
		instruction, material := m.config.Transcribe(ctx, ev)
		if instruction != "" {
			if cur := strings.TrimSpace(ev.Text); cur == "" || cur == NoCaptionStandIn {
				ev.Text = instruction
			} else {
				ev.Text = cur + "\n\n" + instruction
			}
		}
		if material != "" {
			if cur := strings.TrimSpace(ev.Text); cur == "" {
				ev.Text = NoCaptionStandIn + "\n\n" + material
			} else {
				ev.Text = cur + "\n\n" + material
			}
		}
	}

	// Relocate this inbound's staged attachments into the resolved scope's SPACE —
	// ONCE per event, before any agora member fan-out (a staged file shared across
	// members moves exactly once). Mutates ev.Attachments in place so every
	// downstream reader (compose, the turn's tools) sees the space-relative Path.
	// nil hook → attachments keep their staged paths.
	if m.config.PlaceAttachments != nil {
		ev.Attachments = m.config.PlaceAttachments(scope, ev.Attachments)
	}

	// Best-effort "seen" indicator: 👀 react, ONCE per inbound — this also covers
	// the busy/blocked/dedup paths below (no second react downstream; a repeat
	// setMessageReaction is a visual no-op that only burns send-gate quota).
	// Internal events skip naturally (no ChatID/MessageID). Fire-and-forget — NOT
	// in handleWg (shutdown needn't wait for a UX nicety).
	//
	// F152: an onboarding-pass sender (un-approved, router-bound for intro) gets NO 👀:
	// intro's own restraint is "one canned reply then silent", so a "seen" signal on every
	// message with no turn behind it is misleading spam. The intro reply is their feedback.
	if reactor, ok := src.(contract.MessageReactor); ok && ev.ChatID != "" && ev.MessageID != "" && !ev.Onboarding {
		go func() {
			if err := reactor.ReactToMessage(ctx, ev.ChatID, ev.MessageID, "👀"); err != nil {
				slog.Debug("react failed", "source", ev.Source, "msg_id", ev.MessageID, "err", err)
			}
		}()
	}

	// Audio preprocess (transcribe) is DEFERRED to the turn (preprocess → turn):
	// submit hands raw events; the turn transcribes before the model sees them.

	text := strings.TrimSpace(ev.Text)
	if text == "" {
		if len(ev.Attachments) == 0 {
			return
		}
		text = NoCaptionStandIn
		ev.Text = text
	}

	// Slash commands never reach here: the inbound flow forks "/..." events to
	// the slash registry BEFORE submitting (flow/inbound, after the gate).

	// Resolve sid + admission gate + busy-dispatch, retried if the resolved sid
	// is mid-close (a /close-session can land between marking closing and moving
	// the cursor; a message resolving the still-active sid in that window must
	// not join its pending). Bounded so a close-storm can't spin.
	const maxCloseRetries = 4
	const closeRetryBackoff = 30 * time.Millisecond
	openedSid := "" // @=new opens once, reused across retries
	for attempt := 0; ; attempt++ {
		sid := resolution.SessionID
		if sid == "" {
			var s string
			var err error
			switch {
			case resolution.OpenNew:
				// @=new / email fresh compose / internal dispatch: open a FRESH
				// session, threading the caller frame when this event carries one
				// (ev.DispatchCaller(), set by send_message). A worker session
				// records its caller at creation; "" for external wire events.
				// Media-group album siblings converge on ONE fresh session (openNewSid).
				if openedSid == "" {
					openedSid, err = m.openNewSid(ctx, scope, ev)
				}
				s = openedSid
			default:
				// Default (resume-or-open): thread the caller frame so a NEW session
				// opened on a miss records it; an EXISTING resumed session keeps its
				// original caller (set once at creation, never overwritten).
				s, err = m.ResolveActiveSidWithCaller(ctx, scope, ev.Source, ev.UserID, ev.DispatchCaller())
			}
			if err != nil {
				// A transient store error must NOT silently drop the message — tell the
				// user now so they can re-send. No WAL row: the turn never started (and a
				// store error would likely fail the WAL write too), and boot recovery only
				// clears leftover rows silently (it never re-prompts anyone), so the
				// immediate notice IS the recovery here.
				slog.Warn("dispatch: could not resolve sid for scope", "scope", scope, "err", err)
				notice := i18n.T("system.transient_dispatch_error", ev.Lang)
				sink := src.NewSink(ctx, m.replyTarget(ev, src), scope, "", contract.DefaultSinkPrefs())
				m.finishLog(sink, notice, "transient dispatch-error notice", scope)
				return
			}
			sid = s
		}

		// An admin addressing this session marks it admin-owned (promote-only). This
		// lets a later PURE-INTERNAL continuation (a dispatch return / cron wake with no
		// human in the batch) inherit admin via flow/normal's flowIdentity; a batch with
		// a human present is judged by that human instead. Off the consume loop (submit's
		// own goroutine), best-effort.
		if ev.IsAdmin {
			m.markSessionAdmin(ctx, sid)
		}

		// Admission gate: a blocked session (e.g. a manual todo awaiting
		// verification) must NOT consume a turn — queue the message and surface
		// it. handler.Blocked is the seam; the stub returns false (no block)
		// until the turn module lands.
		if m.handler.Blocked(ctx, sid) {
			slog.Info("dispatch: session blocked; routing to pending", "scope", scope, "sid", sid)
			// No WAL row: a blocked message is deliberately held, not work bob was
			// interrupted on — only an in-flight batch is WAL'd, and boot recovery
			// just clears those rows silently (no re-send prompt). NotePending still
			// surfaces it; the submit-entry 👀 already acked receipt.
			m.handler.NotePending(ctx, sid, ev, contract.PendingBlocked)
			return
		}

		m.mu.Lock()
		if m.closing[sid] {
			m.mu.Unlock()
			if resolution.SessionID != "" || attempt >= maxCloseRetries {
				// No WAL row written (only the in-flight branch records one), so this
				// leaves no restart notice — a session closing out from under the
				// message is not an interrupted in-flight turn. But unlike a silent drop,
				// TELL the user so they can re-send (D17): an explicit-sid reply that lands
				// on a session mid-close otherwise gets total silence, like every other
				// unserved submit path notifies.
				slog.Warn("dispatch: target sid is closing; message left unserved (no restart notice — not in-flight)",
					"scope", scope, "sid", sid, "msg_id", ev.MessageID, "attempt", attempt)
				notice := i18n.T("system.session_closing_resend", ev.Lang)
				sink := src.NewSink(ctx, m.replyTarget(ev, src), scope, "", contract.DefaultSinkPrefs())
				m.finishLog(sink, notice, "session-closing notice", scope)
				return
			}
			// Short bounded backoff between retries so the in-progress close can
			// drain (move the cursor / clear m.closing[sid]) instead of spinning
			// hot. ctx-cancel still exits promptly; the retry cap is unchanged.
			select {
			case <-ctx.Done():
				return
			case <-time.After(closeRetryBackoff):
			}
			continue
		}
		if m.busy[sid] {
			mode := m.sidMeta[sid].mode
			// Internal return/dispatch events get queue PRIORITY: they bypass the
			// MaxBatch cap (a worker's product / an agora dispatch is a system reply,
			// not user spam — dropping it loses work). A real chat message past the cap
			// is still dropped (below). Loops (A→B→A) are unguarded by design (no runtime
			// caller-chain cap) — kept out by org permissions (a worker role without
			// send_message can't dispatch back up) + the planned content-repetition
			// guard, not by a count cap on this queue.
			if ev.IsInternal() || len(m.pending[sid]) < m.config.MaxBatch {
				// Dedup: an identical-content message already queued → silent skip (the
				// submit-entry 👀 already acked receipt). Internal return/dispatch events
				// skip user-style content dedup: two distinct worker replies with
				// identical text are separate products, not a user double-tap. (No WAL
				// row to drop — a queued message is not in-flight, so it was never
				// recorded for restart recovery.)
				if !ev.IsInternal() && m.queueDedup(sid, ev) {
					m.mu.Unlock()
					return
				}
				m.pending[sid] = append(m.pending[sid], ev)
				depth := len(m.pending[sid])
				// Album de-noise (D-α, enqueue half): a media-group album (e.g. a
				// 10-image DM) emits one event per attachment; each sibling enqueues and
				// would fire its own queued/running-count notice (~9 for 10 images). The
				// drain side already suppresses the per-event cleared-ack for single-event
				// drains (30-arbiter); this is the missed enqueue half. Only the album's
				// FIRST queued sibling notices — later siblings sharing its MediaGroupID
				// stay silent (the 👀 read-react still acks each). Read under the lock,
				// against the just-appended batch (which counts ev itself).
				albumNoticed := sameAlbumAlreadyNoticed(m.pending[sid], ev)
				m.mu.Unlock()
				m.handler.NotePending(ctx, sid, ev, contract.PendingQueued)
				// Internal events (worker return / agora dispatch / cron) are system
				// replies, not a waiting human — skip the "queued N/MaxBatch" notice
				// (they also bypass the cap, so depth can exceed MaxBatch and render a
				// nonsensical "18/16" — S-20). Album siblings past the first stay silent.
				if !ev.IsInternal() && !albumNoticed {
					m.notifyQueued(ctx, ev, mode, scope, depth)
				}
				return
			}
			m.dropped[sid] = true
			m.mu.Unlock()
			slog.Warn("pending batch full — dropping a message", "scope", scope, "sid", sid, "user", ev.UserName)
			// No WAL row (a dropped message was never in-flight): the user is told now
			// via notifyQueueFull, and NotePending surfaces the cancelled trace — no
			// restart re-send notice on top.
			m.handler.NotePending(ctx, sid, ev, contract.PendingDropped)
			m.notifyQueueFull(ctx, ev, mode, scope)
			return
		}
		m.turnSeq++
		token := m.turnSeq
		m.busy[sid] = true
		m.sidMeta[sid] = sidMetaRow{Scope: scope, StartedAt: clock.Now(), turn: token}
		m.mu.Unlock()
		// runTurnAndDrain WAL-records the in-flight batch at turn start (restart
		// recovery), so no recordPending here — queued/blocked/dropped/closing paths
		// above wrote no row and stay silent; only a running turn's message does.
		m.runTurnAndDrain(ctx, src, scope, sid, []contract.MessageEvent{ev}, false, token, false)
		return
	}
}

// sameAlbumAlreadyNoticed reports whether ev is a non-first sibling of a media-group
// album already represented in pending — i.e. an earlier sibling (same MediaGroupID)
// is already queued, so ev's queued/running-count notice is redundant (the album's
// first queued sibling already told the user). ev is assumed already appended to
// pending, so it is counted: two or more same-album events ⇒ a genuine earlier
// sibling exists. A media-group-less event never matches (each is its own work item).
// Note the in-flight batch is NOT separately consulted: the sibling that STARTED the
// turn emits no notice, so a pending-only count already yields exactly one queued
// notice per album whether the album arrived idle (first sibling starts the turn,
// second is the first pending sibling → notices) or behind an unrelated busy turn
// (first sibling is the first pending sibling → notices).
func sameAlbumAlreadyNoticed(pending []contract.MessageEvent, ev contract.MessageEvent) bool {
	if ev.MediaGroupID == "" {
		return false
	}
	n := 0
	for _, e := range pending {
		if e.MediaGroupID == ev.MediaGroupID {
			if n++; n >= 2 {
				return true
			}
		}
	}
	return false
}

// markSessionAdmin best-effort promotes a session to admin-owned when an admin
// addresses it (store.MarkSessionAdmin is idempotent). Failure only loses the
// inherit-admin-on-internal-resume nicety, so a warn is enough.
func (m *Manager) markSessionAdmin(ctx context.Context, sid string) {
	if err := m.store.MarkSessionAdmin(ctx, sid); err != nil {
		slog.Debug("could not mark session admin-owned", "sid", sid, "err", err)
	}
}

// albumSidTTL bounds how long an album's (scope, media-group) → sid mapping is
// remembered. Aligned with the telegram buffer's flushedStateTTL (5min): a
// late-delivered sibling re-emitted within that window still joins its album's
// session; past it the entry is only a leak (Telegram never reuses a group id).
const albumSidTTL = 5 * time.Minute

// albumSidEntry is one remembered album → fresh-sid mapping (see openNewSid).
type albumSidEntry struct {
	sid string
	at  time.Time
}

// openNewSid opens the fresh session an OpenNew resolution asked for. Two kinds of event
// COLLAPSE repeated OpenNews onto ONE session instead of forking a row each, via the same
// singleflight + TTL-bounded reuse cache (keyed by an opaque namespaced string):
//
//   - Media-group album siblings: the source emits one event per attachment (sharing a
//     caption), each resolving OpenNew — naively N sessions running N concurrent turns,
//     defeating the arbiter's one-image-per-turn drain. The FIRST sibling opens the
//     session, the rest ride its busy queue (siblings arrive within milliseconds).
//   - Onboarding-pass senders (F152): an un-approved stranger's repeated group @s each
//     resolve OpenNew and all just bounce off intro — naively a fresh empty session ROW
//     per message (bounded only by 30-day retention). Collapse them per (scope, sender)
//     so one stranger = one onboarding session, not one per @.
//
// A plain sender keeps @=new (a fresh session per opening @).
func (m *Manager) openNewSid(ctx context.Context, scope string, ev contract.MessageEvent) (string, error) {
	reuseKey := ""
	switch {
	case ev.MediaGroupID != "":
		reuseKey = "album\x00" + scope + "\x00" + ev.MediaGroupID
	case ev.Onboarding:
		reuseKey = "onboard\x00" + scope + "\x00" + ev.Source + "\x00" + ev.UserID
	default:
		return m.OpenSessionInScopeWithCaller(ctx, scope, ev.Source, ev.UserID, ev.DispatchCaller())
	}
	v, err, _ := m.sfAlbum.Do(reuseKey, func() (any, error) {
		if sid, ok := m.albumSid(reuseKey); ok {
			return sid, nil
		}
		sid, err := m.OpenSessionInScopeWithCaller(ctx, scope, ev.Source, ev.UserID, ev.DispatchCaller())
		if err == nil {
			m.rememberAlbumSid(reuseKey, sid)
		}
		return sid, err
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// albumSid returns the unexpired sid remembered for an album key, if any.
func (m *Manager) albumSid(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.albumSids[key]
	if !ok || time.Since(e.at) > albumSidTTL {
		return "", false
	}
	return e.sid, true
}

// rememberAlbumSid records an album's fresh sid, opportunistically pruning
// expired entries (the map only ever holds the last few minutes' albums, so the
// scan is cheap — no sweeper needed).
func (m *Manager) rememberAlbumSid(key, sid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.albumSids {
		if time.Since(e.at) > albumSidTTL {
			delete(m.albumSids, k)
		}
	}
	m.albumSids[key] = albumSidEntry{sid: sid, at: time.Now()}
}

// SidInFlight reports whether sid currently has a running turn (busy) or a
// /close-session draining it (closing). The cold-archive sweep consults this to
// skip a session a turn is actively using, so it can't delete the row + transcript
// out from under an in-flight (or just-revived) turn (D18). Safe for concurrent use.
func (m *Manager) SidInFlight(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[sid] || m.closing[sid]
}

// recordPending writes the crash-recovery row for an unanswered message.
//
// Internal-source events are NOT WAL'd: they are best-effort in-process emits
// (the bridge buffer is itself non-durable), and Stage A has no dispatch-queue
// requeue to replay them — a recorded row would only orphan forever. Durability
// for internal/agora dispatch lands with agora's dispatch queue, not this WAL.
//
// An empty MessageID is not WAL'd either: removeServedPending can only account
// rows by id, so an id-less row could never be consumed — it would sit until the
// boot clear / stale sweep for nothing (write/consume symmetry).
func (m *Manager) recordPending(ctx context.Context, key string, ev contract.MessageEvent) {
	if ev.IsInternal() || ev.MessageID == "" {
		return
	}
	var extras map[string]any
	if ev.MediaGroupID != "" {
		extras = map[string]any{"media_group_id": ev.MediaGroupID}
	}
	if err := m.store.AddPending(ctx, key, ev.Source, ev.ChatID, ev.ThreadID, ev.MessageID, extras); err != nil {
		slog.Warn("could not record pending message", "session", key, "err", err)
	}
}

func (m *Manager) finishLog(sink contract.Sink, text, what, key string) {
	if err := sink.Finish(text); err != nil {
		slog.Warn("could not deliver "+what, "session", key, "err", err)
	}
}
