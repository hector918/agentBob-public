package email

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
)

// 75-coalesce.go — email reply coalescing. Email is unlike a chat channel:
// replying to every turn separately spams the recipient's inbox. So when
// batch_window_seconds is set, a session's per-turn replies are HELD and
// combined into ONE email, sent once the session has been quiet (no new turn)
// for the window. The held buffer is persisted as a FILE per (source, sid)
// under $BOB_HOME/email-batch/<sourceID>/ so the frequent restarts this is
// meant to survive don't lose a computed-but-unsent reply. (trunk deliberately
// gives the email source no DB handle; a file is the right weight for a
// transient buffer whose authoritative history already lives in session
// messages.)
//
// Keyed by sid (session), not scope: a /new opens a fresh sid in the same
// scope, so keying by sid keeps the old session's buffer separate (and
// beginTurn force-flushes it when the new sid's first turn starts). Different
// senders are already different scopes → different sids, so they never mix.
// Non-turn sinks (slash replies, restart notices) come in with sid == "" and
// bypass coalescing entirely (NewSink sends them immediately) — control/notice
// mail must not wait out the window.

// batchSeparator delimits combined segments in the flushed mail body.
const batchSeparator = "\n\n────────\n\n"

// batchFileRow is the on-disk JSON shape of one held buffer. Mirrors
// skeleton's EmailBatchRow minus ID/Source (the directory implies the source,
// the filename the sid).
type batchFileRow struct {
	SID          string `json:"sid"`
	Scope        string `json:"scope"`
	Recipient    string `json:"recipient"`
	Cc           string `json:"cc"`
	Alias        string `json:"alias"`
	CombinedText string `json:"combined_text"`
	SegCount     int    `json:"seg_count"`
	Subject      string `json:"subject"`
	InReplyTo    string `json:"in_reply_to"`
	RefChain     string `json:"ref_chain"`
	UpdatedAt    int64  `json:"updated_at"`
}

// batchState is one session's in-flight coalescing buffer.
type batchState struct {
	sid       string
	scope     string
	recipient string // canonical recipient address (primary To)
	cc        string // reply-all Cc: comma-joined canonical addresses ("" = single-recipient)
	alias     string // forwarded alias (Reply-To); "" when not a forwarded mailbox
	combined  string // segments joined by batchSeparator (no header — added at render)
	segCount  int
	subject   string // already Re:-built, from the latest inbound
	inReplyTo string // latest inbound Message-ID (no <>)
	refChain  string // References chain
	active    int    // in-flight turns for this sid; >0 = still processing, never flush
	timer     *time.Timer
	// recordSent indexes the eventual combined-mail Message-ID against this sid for
	// reply routing. It is the flow's send-time anchor hook (coalesceSink.OnAnchor),
	// deferred: a coalesced send happens async AFTER the turn sink closed, so the
	// flow's own post-turn LastSent record (which sees "") can't reach it. Set per
	// turn (latest wins — same scope+sid across a batch's turns), nil until the first
	// OnAnchor. NOT persisted: a buffer rehydrated after a restart loses it, so a
	// reply to a post-restart coalesced mail falls back to a fresh session (same edge
	// the rehydrate doc already accepts).
	recordSent func(msgID string)
}

// batchCoalescer holds per-sid buffers + the quiet-window timers, persisting
// each buffer to a file so a restart mid-window doesn't lose it.
//
// Locking: c.mu guards every map + batchState. persistLocked (per segment) and
// flushLocked (sendBatch + delete) run their file/queue I/O WHILE holding c.mu
// — accepted for this single-mailbox, low-concurrency use: flushes are
// infrequent and a session's turns are sequential, so the lock is rarely
// contended. (No deadlock: sendBatch/persistLocked never re-enter c.mu,
// enqueueSend's channel send is non-blocking, and the lock order vs the
// source's priorMu/inboxMu/dedupMu is strictly one-way.)
type batchCoalescer struct {
	src    *Source
	dir    string // $BOB_HOME/email-batch/<sourceID>/
	window time.Duration
	maxSeg int

	mu      sync.Mutex
	bySid   map[string]*batchState
	byScope map[string]string // scope → currently-active sid (for /new force-flush)
}

func newBatchCoalescer(src *Source, dir string, window time.Duration, maxSeg int) *batchCoalescer {
	if maxSeg <= 0 {
		maxSeg = 200
	}
	// Create the dir once here, not per-segment in persistLocked (which holds the
	// hot lock). A later vanish (operator rm) just surfaces as a persist error.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("email: batch dir create failed (reply-coalescing buffers may not persist)",
			"source", src.Name(), "dir", dir, "err", src.redact(err.Error()))
	}
	return &batchCoalescer{
		src: src, dir: dir, window: window, maxSeg: maxSeg,
		bySid: map[string]*batchState{}, byScope: map[string]string{},
	}
}

// --- file persistence (replaces skeleton's core.EmailBatchStore) ---------

// safeSidFile maps a sid to a filesystem-safe filename. Real sids are
// already [A-Za-z0-9_]; any stray char is replaced so the name can never
// escape the dir or collide on a separator.
func safeSidFile(sid string) string {
	var b strings.Builder
	for _, r := range sid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "_"
	}
	return name + ".json"
}

// persistLocked writes st's buffer to its file (atomic: tmp + rename). Caller
// holds mu. Best-effort: a write error is logged, not propagated — the next
// segment retries, and the worst case is a lost reply on a restart that
// happens to land between this failed write and the next.
func (c *batchCoalescer) persistLocked(st *batchState) {
	row := batchFileRow{
		SID: st.sid, Scope: st.scope, Recipient: st.recipient, Cc: st.cc, Alias: st.alias,
		CombinedText: st.combined, SegCount: st.segCount, Subject: st.subject,
		InReplyTo: st.inReplyTo, RefChain: st.refChain, UpdatedAt: time.Now().Unix(),
	}
	data, err := json.Marshal(row)
	if err != nil {
		slog.Warn("email: batch row marshal failed", "source", c.src.Name(), "sid", st.sid, "err", err.Error())
		return
	}
	final := filepath.Join(c.dir, safeSidFile(st.sid))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("email: persist reply-batch buffer failed (reply may be lost on restart)",
			"source", c.src.Name(), "sid", st.sid, "err", c.src.redact(err.Error()))
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		slog.Warn("email: persist reply-batch buffer rename failed",
			"source", c.src.Name(), "sid", st.sid, "err", c.src.redact(err.Error()))
		_ = os.Remove(tmp)
	}
}

// deleteFile removes a flushed buffer's file. Best-effort.
func (c *batchCoalescer) deleteFile(sid string) {
	if err := os.Remove(filepath.Join(c.dir, safeSidFile(sid))); err != nil && !os.IsNotExist(err) {
		slog.Warn("email: delete flushed batch file failed", "source", c.src.Name(), "sid", sid, "err", c.src.redact(err.Error()))
	}
}

// setRecord stores the flow's send-time anchor hook for sid's buffer, so the
// eventual combined mail's Message-ID gets indexed against this session for
// reply routing. Called by coalesceSink.OnAnchor right after NewSink/beginTurn,
// so the state exists; a no-op if the buffer was already flushed in between.
func (c *batchCoalescer) setRecord(sid string, fn func(msgID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.bySid[sid]; st != nil {
		st.recordSent = fn
	}
}

// --- coalescing core (ported from skeleton 75-coalesce.go) ---------------

// beginTurn records that a new turn for sid has started — hold replies, stop
// the quiet-window timer. If a DIFFERENT sid is active in the same scope (a
// /new opened a fresh session), flush the old one first so its buffered reply
// isn't merged across the session boundary.
func (c *batchCoalescer) beginTurn(sid, scope string, t contract.Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if scope != "" {
		if prev, ok := c.byScope[scope]; ok && prev != sid {
			if st := c.bySid[prev]; st != nil {
				c.flushLocked(st)
			}
		}
		c.byScope[scope] = sid
	}
	st := c.bySid[sid]
	if st == nil {
		st = &batchState{sid: sid, scope: scope, recipient: CanonicalAddress(t.ChatID), alias: t.ThreadID}
		c.bySid[sid] = st
	}
	st.active++
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
}

// addSegment records a finished turn's reply, captures the latest inbound
// threading metadata, persists the buffer, then either force-flushes (cap hit)
// or arms the quiet-window timer (when no turn is still in flight).
func (c *batchCoalescer) addSegment(sid, scope string, t contract.Target, full string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.bySid[sid]
	if st == nil {
		// beginTurn always runs first; be defensive against a stray Finish.
		st = &batchState{sid: sid, scope: scope, recipient: CanonicalAddress(t.ChatID), alias: t.ThreadID, active: 1}
		c.bySid[sid] = st
		if scope != "" {
			c.byScope[scope] = sid
		}
	}
	if st.active > 0 {
		st.active--
	}
	if full = strings.TrimSpace(full); full != "" {
		if st.combined == "" {
			st.combined = full
		} else {
			st.combined += batchSeparator + full
		}
		st.segCount++
		// Latest inbound threading wins (reply threads to the most recent
		// message). Captured while the in-memory stash is still warm and
		// persisted below so a restart can still render the headers.
		if ic := c.src.lookupInboundContext(inboxKey(st.recipient, st.alias)); ic != nil {
			st.subject = buildSubject(ic.Subject)
			st.inReplyTo = ic.MessageID
			st.refChain = buildReferences(ic.References, ic.MessageID)
			cfg := &c.src.cfg
			st.cc = strings.Join(replyAllCc(cfg.Address, st.alias, st.recipient, cfg.ForwardedDomains, ic.ToAddrs, ic.CcAddrs), ", ")
		}
		c.persistLocked(st)
	}
	switch {
	case st.segCount >= c.maxSeg && st.segCount > 0:
		c.flushLocked(st) // bounded — progress mail mid-batch
	case st.active == 0 && st.segCount > 0:
		c.armLocked(st)
	case st.active == 0 && st.segCount == 0:
		// No turn in flight and nothing buffered (every segment trimmed to
		// empty). Neither flush nor timer would ever clear this entry, so drop
		// it now instead of letting an empty batchState linger.
		c.dropEmptyLocked(st)
	}
	return nil
}

func (c *batchCoalescer) armLocked(st *batchState) {
	if st.timer != nil {
		st.timer.Stop()
	}
	// Capture the *batchState pointer (not just the sid): time.AfterFunc's fire
	// can't be cancelled once it's queued on c.mu, so a stale timer may run
	// AFTER its state was flushed + a new same-sid state took its place.
	// onTimer compares pointer identity to skip that case.
	st.timer = time.AfterFunc(c.window, func() { c.onTimer(st) })
}

func (c *batchCoalescer) onTimer(st *batchState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bySid[st.sid] != st || st.active > 0 || st.segCount == 0 {
		// Stale (this state was flushed and a new same-sid state replaced it), a
		// new turn re-opened it (active>0), or nothing buffered → skip.
		return
	}
	c.flushLocked(st)
}

// flushLocked renders the combined reply, hands it to the send queue, and
// clears the buffer + its file. Caller holds mu.
func (c *batchCoalescer) flushLocked(st *batchState) {
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	delete(c.bySid, st.sid)
	if st.scope != "" && c.byScope[st.scope] == st.sid {
		delete(c.byScope, st.scope)
	}
	// sent reports whether the reply actually reached the send queue. An empty
	// buffer is trivially "sent" (nothing to deliver). When there IS content
	// but sendBatch could not enqueue (source closed by shutdown racing the
	// flush timer, or an enqueue error), we must NOT delete the file — leaving
	// it lets the next boot's rehydrate replay the computed-but-unsent reply.
	sent := true
	if st.segCount > 0 && st.combined != "" {
		sent = c.src.sendBatch(context.Background(), st)
	}
	if !sent {
		slog.Warn("email: batch flush could not send — keeping file for rehydrate",
			"source", c.src.Name(), "sid", st.sid)
		return
	}
	c.deleteFile(st.sid)
}

// dropEmptyLocked removes a batchState that holds no buffered content and has
// no turn in flight — an entry flushLocked/onTimer would never reach. No file
// exists for it (persistLocked only runs on non-empty segments), so a map
// delete is the whole cleanup. Caller holds mu.
func (c *batchCoalescer) dropEmptyLocked(st *batchState) {
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if c.bySid[st.sid] == st {
		delete(c.bySid, st.sid)
	}
	if st.scope != "" && c.byScope[st.scope] == st.sid {
		delete(c.byScope, st.scope)
	}
}

// rehydrate re-loads persisted buffers at startup and re-arms their timers.
// active stays 0 (no turns in flight post-boot), so each fires after one
// window unless new inbound re-opens it (beginTurn cancels the timer + the
// next Finish re-arms). Recipient, alias (Reply-To) and the threading headers
// are all restored from the file.
func (c *batchCoalescer) rehydrate(ctx context.Context) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("email: rehydrate reply-batch buffers failed", "source", c.src.Name(), "err", c.src.redact(err.Error()))
		}
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Sweep orphan tmp files left by a crash between WriteFile and Rename —
		// nothing else ever removes them, so without this they leak unbounded.
		if strings.HasSuffix(e.Name(), ".tmp") {
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			slog.Warn("email: read batch file failed", "source", c.src.Name(), "file", e.Name(), "err", c.src.redact(err.Error()))
			continue
		}
		var r batchFileRow
		if err := json.Unmarshal(data, &r); err != nil || r.SID == "" {
			slog.Warn("email: skip corrupt batch file", "source", c.src.Name(), "file", e.Name())
			continue
		}
		st := &batchState{
			sid: r.SID, scope: r.Scope, recipient: r.Recipient, cc: r.Cc, alias: r.Alias,
			combined: r.CombinedText, segCount: r.SegCount,
			subject: r.Subject, inReplyTo: r.InReplyTo, refChain: r.RefChain,
		}
		c.bySid[r.SID] = st
		if r.Scope != "" {
			c.byScope[r.Scope] = r.SID
		}
		if st.segCount > 0 {
			c.armLocked(st)
		}
		n++
	}
	if n > 0 {
		slog.Info("email: rehydrated reply-batch buffers", "source", c.src.Name(), "count", n)
	}
}

// batchHeader prefixes a combined reply that merges more than one turn, so the
// recipient knows it answers several of their messages at once.
func batchHeader(n int, lang string) string {
	return i18n.T("system.email_batch_header", lang, n)
}

// sendBatch renders st's combined reply and hands it to the send queue. Unlike
// sendReply it renders from the buffered state, NOT a fresh stash lookup — the
// stash is in-memory and gone after a restart, so the threading headers come
// from the persisted file. Returns true when the reply reached the send queue
// (caller may then delete the file); false ONLY when nothing durable exists
// (source closed, or enqueue failed) so the caller keeps the file for the next
// boot's rehydrate. An empty recipient returns true (nothing to recover).
func (s *Source) sendBatch(ctx context.Context, st *batchState) bool {
	if s.closed.Load() {
		return false
	}
	to := st.recipient
	if to == "" {
		return true
	}
	cfg := &s.cfg
	subject := st.subject
	if subject == "" {
		subject = i18n.T("email.subject.default", i18n.Detect(st.combined))
	}
	body := st.combined
	if st.segCount > 1 {
		// Header language = the batched body's OWN detected language (it's bob's reply
		// text), not a per-recipient memory — so the header matches what it heads with
		// no state. "" (no signal) → "default".
		hl := i18n.Detect(body)
		if hl == "" {
			hl = "default"
		}
		body = batchHeader(st.segCount, hl) + body
	}
	// Stash the user-visible combined body so the next inbound from this
	// sender+alias can be chunked-stripped against it (D-B5), mirroring
	// sendReply.
	s.recordPriorBody(inboxKey(to, st.alias), body)
	msgID := newMessageID(cfg.Address)
	rendered := renderEmail(emailRender{
		From:       cfg.Address,
		To:         to,
		Cc:         st.cc,
		ReplyTo:    st.alias,
		Subject:    subject,
		Date:       formatRFC2822Date(time.Now()),
		MessageID:  msgID,
		InReplyTo:  st.inReplyTo,
		References: st.refChain,
		Body:       body,
	})
	err := s.enqueueSend(ctx, sendReq{
		to:        to,
		body:      []byte(rendered),
		queuedAt:  time.Now(),
		messageID: msgID,
	})
	if err != nil {
		slog.Warn("email: batch flush enqueue failed", "source", s.Name(), "to", to, "err", s.redact(err.Error()))
		return false
	}
	// Reply-routing: index the combined mail's Message-ID against this sid so a
	// user reply (In-Reply-To = msgID) threads back to the originating session
	// instead of forking a fresh one. The flow's per-turn anchor hook is deferred
	// to here because the coalesced send happens after the turn sink closed. Async
	// + best-effort (mirrors the flow's own RecordSent posture) and off the held
	// c.mu — the callback runs a store write we must not block the lock on.
	if st.recordSent != nil {
		go st.recordSent(msgID)
	}
	return true
}

// coalesceSink is the per-turn contract.Sink for a coalescing account. It
// ignores streaming (email is block-mode anyway) and uses Finish's full
// content as the turn's segment. beginTurn was already called by NewSink.
// SendFile still delivers immediately (attachments are one-off, not text to
// coalesce); LastSent is "" — the coalesced reply is sent later, async, so the
// flow's post-turn LastSent record can't reach it. Instead the flow's send-time
// anchor hook (OnAnchor) is captured and replayed at the deferred send (sendBatch),
// so coalesced replies are still reply-routable.
type coalesceSink struct {
	c      *batchCoalescer
	ctx    context.Context
	target contract.Target
	scope  string
	sid    string
}

func (k *coalesceSink) ContentDelta(string) {}
func (k *coalesceSink) TraceDelta(string)   {}
func (k *coalesceSink) Finish(full string) error {
	return k.c.addSegment(k.sid, k.scope, k.target, full)
}
func (k *coalesceSink) SendFile(path, caption string) error {
	return k.c.src.SendFile(k.ctx, k.target, path, caption)
}
func (k *coalesceSink) LastSent() string { return "" }

// BareProductFinish (contract.BareProductSink marker): ContentDelta above is a
// no-op and Finish's full becomes a merged-mail segment VERBATIM — the turn
// core must hand it the bare trimmed reply, never the sunk ContentDelta
// accumulation (a tool-round preamble glued on would ship inside the mail body).
func (k *coalesceSink) BareProductFinish() {}

// OnAnchor captures the flow's reply-routing record hook. Because the coalesced
// send is deferred past the turn, the hook is stored on the sid's buffer and
// fired with the real Message-ID when sendBatch eventually delivers (see
// batchState.recordSent). Mirrors streamsink.Sink.OnAnchor so the flow's
// optional-interface assertion finds it.
func (k *coalesceSink) OnAnchor(fn func(msgID string)) { k.c.setRecord(k.sid, fn) }

var (
	_ contract.Sink            = (*coalesceSink)(nil)
	_ contract.BareProductSink = (*coalesceSink)(nil)
)
