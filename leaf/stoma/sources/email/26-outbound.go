package email

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbob/contract"
)

// 26-outbound.go — the outbound recovery log (F24). Every outbound message is
// persisted to a FILE between "enqueued to the send worker" and "SMTP accepted
// it", so a crash / restart / permanent SMTP failure in that window replays the
// reply on the next boot instead of losing it silently.
//
// Why a file, not a DB row: trunk deliberately gives the email source no DB
// handle — a durable buffer whose authoritative history already lives in
// session messages is the file's weight, not the store's. This mirrors the
// reply-coalescer's own file persistence (75-coalesce.go). The two are
// complementary layers: the coalescer holds the PRE-enqueue combined draft
// (deleted once it hands the baton to enqueueSend); this log owns durability
// from enqueue until SMTP-accept.
//
// Lifecycle:
//  1. enqueueSend writes a row (sent-marker = the file's existence) BEFORE the
//     channel send. A fresh send that is then rejected (queue full / ctx done)
//     deletes its row — the caller was told it failed.
//  2. The send worker DELETEs the row on SMTP success. On permanent failure it
//     KEEPS the row (+ summons the admin) so the next boot retries it.
//  3. Source.Run scans the dir at startup, re-enqueues every survivor younger
//     than outboundTTL, and prunes older ones (an admin who restarted a day
//     later has handled it by hand; the sender's reply window has closed).

// outboundTTL bounds how long an undelivered outbound row is replayed before it
// is pruned as stale. Mirrors the skeleton's D-C3 24h window.
const outboundTTL = 24 * time.Hour

// outboundRow is the on-disk JSON shape of one pending outbound message. Body
// is the fully-rendered RFC 2822 message (same []byte handed to SMTP), so a
// replay is byte-equivalent to the original attempt — same headers, same
// Message-ID, so the recipient sees the SAME mail, not a duplicate.
type outboundRow struct {
	To        string `json:"to"`
	MessageID string `json:"message_id"`
	Body      []byte `json:"body"` // base64 in JSON
	QueuedAt  int64  `json:"queued_at"`
}

// outboundLog persists pending outbound messages under
// $BOB_HOME/email-outbound/<sourceID>/ as one JSON file per message.
type outboundLog struct {
	src *Source
	dir string
}

// newOutboundLog builds the log + creates its dir. A dir-create failure is
// logged, not fatal — record() then degrades to at-most-once (its write fails
// and the send proceeds unpersisted), same posture as the coalescer.
func newOutboundLog(src *Source, dir string) *outboundLog {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("email: outbound-log dir create failed (undelivered replies won't survive a restart)",
			"source", src.Name(), "dir", dir, "err", src.redact(err.Error()))
	}
	return &outboundLog{src: src, dir: dir}
}

// record writes req to a new row and returns its file id (the sanitised
// Message-ID). Atomic (tmp + rename). A write error returns "" — the caller
// leaves outboundID unset and the send proceeds unpersisted.
func (o *outboundLog) record(req sendReq) (string, error) {
	id := req.messageID
	if id == "" {
		id = req.to // last-resort key; a Message-ID is always minted in practice
	}
	row := outboundRow{To: req.to, MessageID: req.messageID, Body: req.body, QueuedAt: req.queuedAt.Unix()}
	data, err := json.Marshal(row)
	if err != nil {
		slog.Warn("email: outbound row marshal failed", "source", o.src.Name(), "err", err.Error())
		return "", err
	}
	final := filepath.Join(o.dir, safeSidFile(id))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("email: persist outbound row failed (reply may be lost on restart)",
			"source", o.src.Name(), "to", req.to, "err", o.src.redact(err.Error()))
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		slog.Warn("email: persist outbound row rename failed", "source", o.src.Name(), "err", o.src.redact(err.Error()))
		_ = os.Remove(tmp)
		return "", err
	}
	return safeSidFile(id), nil
}

// delete removes a delivered / abandoned row's file. Best-effort. name is the
// value record returned (already a safe filename).
func (o *outboundLog) delete(name string) {
	if name == "" {
		return
	}
	if err := os.Remove(filepath.Join(o.dir, name)); err != nil && !os.IsNotExist(err) {
		slog.Warn("email: delete outbound row failed", "source", o.src.Name(), "file", name, "err", o.src.redact(err.Error()))
	}
}

// replay re-enqueues every undelivered row younger than outboundTTL at startup
// and prunes older / corrupt ones. Bounded at sendQueueSize per boot so a long
// outage's backlog can't overrun the send queue; the overflow stays on disk for
// the next boot. Called from Source.Run after the send worker is up.
func (o *outboundLog) replay(ctx context.Context) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("email: replay outbound log failed", "source", o.src.Name(), "err", o.src.redact(err.Error()))
		}
		return
	}
	cutoff := time.Now().Add(-outboundTTL).Unix()
	replayed := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Sweep orphan tmp files left by a crash between WriteFile and Rename.
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(o.dir, name))
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(o.dir, name)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			slog.Warn("email: read outbound row failed", "source", o.src.Name(), "file", name, "err", o.src.redact(rerr.Error()))
			continue
		}
		var row outboundRow
		if err := json.Unmarshal(data, &row); err != nil || len(row.Body) == 0 {
			slog.Warn("email: dropping corrupt outbound row", "source", o.src.Name(), "file", name)
			_ = os.Remove(path)
			continue
		}
		if row.QueuedAt < cutoff {
			slog.Warn("email: pruning stale undelivered outbound row",
				"source", o.src.Name(), "to", row.To, "queued_ago", time.Since(time.Unix(row.QueuedAt, 0)).Round(time.Hour))
			_ = os.Remove(path)
			continue
		}
		if replayed >= sendQueueSize {
			slog.Warn("email: outbound replay hit the queue cap — remaining rows stay for the next boot",
				"source", o.src.Name(), "cap", sendQueueSize)
			return
		}
		// Re-enqueue with the SAME file id (name) so enqueueSend does not
		// re-record it and the worker deletes THIS file on success.
		req := sendReq{
			to:         row.To,
			body:       row.Body,
			queuedAt:   time.Unix(row.QueuedAt, 0),
			messageID:  row.MessageID,
			outboundID: name,
		}
		if err := o.src.enqueueSend(ctx, req); err != nil {
			// Queue full / ctx done during replay: KEEP the file (it was pre-set
			// as a replay, so enqueueSend left it) for the next boot.
			slog.Warn("email: outbound replay enqueue failed — keeping row", "source", o.src.Name(), "to", row.To, "err", o.src.redact(err.Error()))
			return
		}
		replayed++
	}
	if replayed > 0 {
		slog.Info("email: replayed undelivered outbound mail", "source", o.src.Name(), "count", replayed)
	}
}

// outboundDelete removes an outbound row by id when the log is enabled. Safe on
// a nil log / empty id.
func (s *Source) outboundDelete(id string) {
	if s.outbound == nil || id == "" {
		return
	}
	s.outbound.delete(id)
}

// notifyAdmin summons the operator for a delivery fault the source alone can
// observe (undeliverable outbound mail). Lazy + no-op until stoma wires the
// admin line, and a no-op when none is attached. origin = the source name.
func (s *Source) notifyAdmin(level contract.AdminLevel, format string, args ...any) {
	if s.adminLine == nil {
		return
	}
	line := s.adminLine()
	if line == nil {
		return
	}
	// B15: run Notify on a fresh goroutine with a recover so a panic in the
	// admin line never kills the process. Some callers hold a lock (the
	// coalescer's onTimer holds batchCoalescer.mu), so spawning also avoids
	// blocking Notify under that lock. Mirrors stoma's Module.alert (D8).
	origin := s.Name()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("email: adminLine.Notify panicked", "panic", r, "origin", origin)
			}
		}()
		line.Notify(context.Background(), level, origin, format, args...)
	}()
}
