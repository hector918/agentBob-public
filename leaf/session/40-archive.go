package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/session/messages"
	"agentbob/leaf/session/store"
)

// archiveReplayCap bounds the transcript stored in an archive payload: we archive
// the replay WINDOW (from the last compaction summary marker, else the last cap
// rows), NOT the full raw history. Rationale: a turn only ever replays that window
// (pre-summary rows are already folded into the summary and the model never sees
// them), so restore-fidelity == replay-fidelity — archiving more is wasted bytes and
// leaves the JSONB payload unbounded. The export is GetReplay, which now carries
// each row's attachments bag on contract.Message.Atts, so RecentAttachments
// survives the round-trip too. Mirrors the turn core's historyReplayCap (500); kept
// as a separate session-side const because turn can't be imported here.
const archiveReplayCap = 500

// archiveBatchLimit caps how many cold sessions one sweep archives, so a large
// first-run backlog drains over successive sweeps instead of one long serial pass
// holding the housekeeper worker.
const archiveBatchLimit = 500

// archivePayload is the serialized form of a cold session: its lifecycle meta plus
// the replay-window transcript (bounded by archiveReplayCap, incl. each row's
// attachments bag — contract.Message.Atts is `json:"atts,omitempty"`, the same key
// the former messages.ArchivedMessage envelope emitted, so payloads written before
// the bag existed still unmarshal and bag-less rows stay byte-identical).
// turn_events are NOT carried — they were already learned before the session went
// cold (the cold cutoff ≫ the learning settle age), so there is nothing left to
// re-learn on restore.
type archivePayload struct {
	Meta store.SessionInfo  `json:"meta"`
	Msgs []contract.Message `json:"msgs"`
}

// Archiver moves cold sessions to the archive blob table and revives them on a
// later reply (docs/wip-message-ownership.md §6 / loop-core-spec §11.8). It lives
// entirely inside the session module: the message transcript and the lifecycle
// row are both session subpackages over the same contract.DB, so archival is a
// single-module operation with no cross-module seam.
type Archiver struct {
	db       contract.DB
	sessions *store.PG
	msgs     *messages.PG
	// inFlight reports whether a sid has a running/closing turn right now (the
	// Manager's busy/closing). ArchiveCold skips such a sid so it never deletes the
	// row + transcript out from under an in-flight turn (D18). nil → no coordination
	// (archives every cold sid; used in tests with no live Manager).
	inFlight func(sid string) bool
}

// SetInFlight wires the liveness predicate (the Manager's InFlight) so the cold
// sweep can skip sessions a turn is actively using. Set once at module Start.
func (a *Archiver) SetInFlight(fn func(sid string) bool) { a.inFlight = fn }

// NewArchiver builds the archiver over the shared DB pool (for the atomic restore
// tx) plus the session lifecycle store and the message-history store (both session
// subpackages).
func NewArchiver(db contract.DB, sessions *store.PG, msgs *messages.PG) *Archiver {
	return &Archiver{db: db, sessions: sessions, msgs: msgs}
}

// ArchiveCold archives every session whose last activity is older than cutoff
// (epoch seconds), dead or alive. For each: serialize meta+transcript into the
// archive row, then drop the hot message rows, then drop the session row.
//
// The ORDER is the idempotency guarantee. ArchiveRow is ON CONFLICT DO NOTHING,
// so a crash anywhere is safe to retry:
//   - crash after ArchiveRow, before deleting messages → next sweep re-sees the
//     session as cold, ArchiveRow no-ops (row already present), deletes proceed.
//   - crash after deleting messages, before deleting the session → next sweep
//     re-sees it (now with no messages, judged cold by started_at), ArchiveRow
//     no-ops, message delete no-ops, session delete proceeds.
//
// The archive row is never the thing left dangling: it is written first and only
// removed by restore/purge. Returns the number of sessions archived.
func (a *Archiver) ArchiveCold(ctx context.Context, cutoff float64) (int, error) {
	cold, err := a.sessions.ColdSessionIDs(ctx, cutoff, archiveBatchLimit)
	if err != nil {
		return 0, fmt.Errorf("ArchiveCold: list cold: %w", err)
	}
	n := 0
	for _, c := range cold {
		// Skip a session a turn is actively using (busy) or that /close-session is
		// draining (closing): archiving it would race the live turn and delete its row +
		// transcript out from under it (D18). A cold sid only becomes in-flight via a
		// concurrent reply-revive; leave it this sweep — it's re-listed next sweep once
		// idle again. (The hair-thin window where a revive begins AFTER this check but
		// before DeleteSession remains, but is astronomically rare for a sid already cold
		// by the activity cutoff; the check closes the dominant case the ledger flagged.)
		if a.inFlight != nil && a.inFlight(c.SessionID) {
			continue
		}
		// Archive the replay window, not the full raw history (archiveReplayCap):
		// restore only needs what a turn would replay (from the last summary marker).
		// GetReplay carries each row's attachments bag on Atts (RecentAttachments).
		msgs, err := a.msgs.GetReplay(ctx, c.SessionID, archiveReplayCap)
		if err != nil {
			return n, fmt.Errorf("ArchiveCold: export %s: %w", c.SessionID, err)
		}
		meta, err := a.sessions.SessionByID(ctx, c.SessionID)
		if err != nil {
			return n, fmt.Errorf("ArchiveCold: meta %s: %w", c.SessionID, err)
		}
		payload, err := json.Marshal(archivePayload{Meta: meta, Msgs: msgs})
		if err != nil {
			return n, fmt.Errorf("ArchiveCold: marshal %s: %w", c.SessionID, err)
		}
		if err := a.sessions.ArchiveRow(ctx, c.SessionID, c.Scope, c.Source, c.StartedAt, c.LastMsgAt, c.EndedAt, clock.UnixSeconds(), string(payload)); err != nil {
			return n, err
		}
		if err := a.msgs.DeleteSessionMessages(ctx, c.SessionID); err != nil {
			return n, err
		}
		if err := a.sessions.DeleteSession(ctx, c.SessionID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Restore rehydrates an archived session by sid (the reply cold-revive path). It
// returns true only when the session was archived AND was alive when archived
// (revivable); a was-ended session (closed / finalized / rotated) is NOT revived
// — its context was deliberately retired, so the caller opens a fresh session.
// On success the hot session row + transcript are recreated and the archive row
// dropped.
func (a *Archiver) Restore(ctx context.Context, sessionID string) bool {
	payload, wasAlive, ok, err := a.sessions.ArchivedSession(ctx, sessionID)
	if err != nil {
		slog.Warn("session: restore lookup failed", "sid", sessionID, "err", err)
		return false
	}
	if !ok || !wasAlive {
		return false
	}
	var p archivePayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		slog.Warn("session: restore payload parse failed", "sid", sessionID, "err", err)
		return false
	}
	// Recreate the lifecycle row (alive), re-insert the transcript, and drop the
	// archive row in ONE transaction. Atomicity is load-bearing: a non-atomic
	// sequence could leave a LIVE-but-EMPTY session (crash after recreate, before
	// import) that ensureLive's SessionByID-first check would then shadow over the
	// still-present archive — silently losing the transcript. With one tx, a crash
	// or any step error rolls the whole thing back: the session stays non-live and
	// the archive row intact, so the next reply re-runs Restore cleanly.
	tx, err := a.db.Begin(ctx)
	if err != nil {
		slog.Warn("session: restore begin tx failed", "sid", sessionID, "err", err)
		return false
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if err := a.sessions.RecreateArchivedSession(ctx, tx, p.Meta); err != nil {
		slog.Warn("session: restore recreate failed", "sid", sessionID, "err", err)
		return false
	}
	if err := a.msgs.ImportMessages(ctx, tx, sessionID, p.Msgs); err != nil {
		slog.Warn("session: restore import messages failed", "sid", sessionID, "err", err)
		return false
	}
	if err := a.sessions.DeleteArchived(ctx, tx, sessionID); err != nil {
		slog.Warn("session: restore delete-archive failed", "sid", sessionID, "err", err)
		return false
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("session: restore commit failed", "sid", sessionID, "err", err)
		return false
	}
	slog.Info("session: restored archived session", "sid", sessionID, "messages", len(p.Msgs))
	return true
}

// PurgeOld hard-deletes archive rows older than cutoff and their reply-index rows
// (the second-level cleanup, §11.8). Past this hard TTL a reply opens a fresh
// session. Returns the number purged.
func (a *Archiver) PurgeOld(ctx context.Context, cutoff float64) (int, error) {
	sids, err := a.sessions.PurgeArchivedBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("PurgeOld: %w", err)
	}
	if err := a.sessions.DeleteMessageIndexForSessions(ctx, sids); err != nil {
		return len(sids), err
	}
	return len(sids), nil
}
