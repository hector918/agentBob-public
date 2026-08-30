package retrieval

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/heartwood/scrub"
)

// contentCap bounds one message's stored content (bytes-ish via runes). Long
// mechanical blobs (pasted files / big tool dumps folded into a reply) are
// truncated so they don't bloat the corpus or dominate recall.
const contentCap = 8000

func now() float64 { return clock.UnixSeconds() }

// feed is the contract.RetrievalFeed: the FLOW hands it a finished turn and it
// builds + persists outbox rows. Fire-and-forget — errors log, never fail the turn.
type feed struct{ st *outbox }

var _ contract.RetrievalFeed = (*feed)(nil)

// EnqueueTurn persists this turn's user events + assistant reply as outbox rows.
// The flow has already stamped identity (company/room); each user event keeps its
// OWN speaker handle (group attribution). Best-effort: an insert error logs.
func (f *feed) EnqueueTurn(ctx context.Context, t contract.RetrievalTurn) {
	rows := buildRows(t)
	if len(rows) == 0 {
		return
	}
	// WithoutCancel: the turn ctx is cancelled on /close-session and shutdown, but
	// this best-effort corpus write must still land (the sibling idx.RecordSent writes
	// do the same). Shutdown ordering is safe — this module Needs contract.DB, which
	// outlives it.
	if err := f.st.insert(context.WithoutCancel(ctx), rows); err != nil {
		slog.Warn("retrieval: enqueue failed (corpus gap, turn unaffected)", "sid", t.Sid, "err", err)
	}
}

// buildRows turns a RetrievalTurn into outbox rows: one per non-empty user event
// (own handle; name-prefixed in a group so the speaker is searchable/readable) +
// one assistant row (under __bob__). Content is scrubbed (secrets never leave) and
// rune-capped. message_id is deterministic so a re-enqueue dedups at the leaf.
func buildRows(t contract.RetrievalTurn) []outboxRow {
	company := t.Company
	if company == "" {
		company = contract.RetrievalPersonalCompany
	}
	var rows []outboxRow
	for _, ev := range t.Events {
		text := strings.TrimSpace(ev.Text)
		// F165: skip an empty row AND a bare stand-in row — an attachment-only message
		// (e.g. an image with no caption) carries the literal NoCaptionStandIn as its
		// text, which is noise paired against the answer in the recall corpus. (Voice
		// notes DON'T hit this: their transcript replaced the stand-in at ingestion.)
		if text == "" || text == contract.NoCaptionStandIn {
			continue
		}
		content := scrub.Scrub(text)
		// In a group, prefix the speaker name so "who said it" is readable AND
		// findable by entity_mentions literal match. DM: no prefix (single, private).
		if t.Room != "" && ev.UserName != "" {
			content = "[" + ev.UserName + "]: " + content
		}
		content = truncRunes(content, contentCap)
		if strings.TrimSpace(content) == "" {
			continue
		}
		src, uid := ev.AccountHandle()
		rows = append(rows, outboxRow{
			MessageID: t.Sid + ":" + ev.MessageID,
			CompanyID: company,
			SessionID: t.Sid,
			UserID:    src + ":" + uid,
			ChannelID: t.Room,
			Role:      "user",
			Content:   content,
			Ts:        float64(ev.ReceivedAt.Unix()),
			Lang:      ev.Lang,
		})
	}
	// Only feed the assistant reply when it has a real anchor (the triggering user
	// message's id). An anchor-less turn — a proactive / resume / internal-nudge turn
	// with no user event — is a bot-initiated reply with no user query to pair with: low
	// retrieval value, AND its message_id would be "<sid>::a" for EVERY such turn in the
	// sid, so the leaf's message_id dedup would silently drop all but the first. Skip it.
	if reply := strings.TrimSpace(t.Reply); reply != "" && t.AnchorID != "" {
		rows = append(rows, outboxRow{
			MessageID: t.Sid + ":" + t.AnchorID + ":a",
			CompanyID: company,
			SessionID: t.Sid,
			UserID:    contract.RetrievalBotHandle,
			ChannelID: t.Room,
			Role:      "assistant",
			Content:   truncRunes(scrub.Scrub(reply), contentCap),
			Ts:        now(),
		})
	}
	return rows
}

// drain pushes the oldest batch to the leaf, deleting the rows it accepted. A
// permanent (content-fault) rejection dead-letters that batch; a transient error
// leaves the rows for the next tick. Returns the number pushed.
func drain(ctx context.Context, cl *Client, st *outbox, batch int) (int, error) {
	rows, err := st.pop(ctx, batch)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	ierr := cl.Ingest(ctx, rows)
	if ierr != nil && !errors.Is(ierr, ErrPermanent) {
		return 0, ierr // transient — leave rows, retry next tick
	}
	// success OR dead-letter (permanent): delete so the FIFO head can't wedge.
	// ACCEPTED granularity (review): the leaf 422s the WHOLE request on a
	// single bad message, so one poison row dead-letters its batch (≤ BatchSize good
	// neighbours dropped from the append-only corpus). Tolerated because the common
	// 422 cause (empty content) is pre-filtered client-side and a drop is loud (Warn
	// below). Revisit with a per-row fallback only if this ever actually fires.
	if derr := st.del(ctx, ids); derr != nil {
		return 0, derr
	}
	if errors.Is(ierr, ErrPermanent) {
		slog.Warn("retrieval: dead-lettered a poison batch", "count", len(ids), "err", ierr)
	}
	return len(rows), nil
}

// truncRunes caps s to n runes (rune-aware so a multi-byte rune isn't cut mid-way,
// which would make the leaf reject the batch on invalid UTF-8).
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
