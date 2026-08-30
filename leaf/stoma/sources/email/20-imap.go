package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"agentbob/contract"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// fetchFailureCap bounds per-uid transient-failure retries before a poison
// message is \Seen'd to break the re-fetch loop. See Source.fetchFail.
const fetchFailureCap = 5

// permanentErr marks a fetch/parse failure as a PERMANENT CONTENT defect
// (malformed MIME structure, no From address) — the message will never parse,
// so it is safe to \Seen and stop re-processing. A fetch error that is NOT
// wrapped this way is treated as TRANSIENT (transport / server-side) and left
// UNSEEN for a reconnect to re-fetch, bounded by the per-uid failure counter.
type permanentErr struct{ err error }

func (e permanentErr) Error() string { return e.err.Error() }
func (e permanentErr) Unwrap() error { return e.err }

// permanent wraps err as a permanent content defect (see permanentErr).
func permanent(err error) error { return permanentErr{err: err} }

// isPermanent reports whether err (or anything it wraps) is a permanentErr.
func isPermanent(err error) bool {
	var p permanentErr
	return errors.As(err, &p)
}

// bumpFetchFailure records one more consecutive transient failure for uid and
// returns the new count.
func (s *Source) bumpFetchFailure(uid imap.UID) int {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	s.fetchFail[uid]++
	return s.fetchFail[uid]
}

// clearFetchFailure resets uid's transient-failure counter after a success.
func (s *Source) clearFetchFailure(uid imap.UID) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	delete(s.fetchFail, uid)
}

// runPollLoop is the per-account IMAP loop. Until ctx is cancelled, the
// loop dials the IMAP server, selects the configured folder, then on every
// poll tick fetches every UNSEEN message, hands each to fetchAndEmit,
// optionally marks \Seen. Connection lifetime is bounded by
// imapForcedReconnect (1h). A connect / auth error triggers exponential
// backoff (1s → 30s capped).
//
// Panic recovery wraps each per-message handler so a corrupt MIME doesn't
// kill the whole loop.
func (s *Source) runPollLoop(ctx context.Context) {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		// Single session — runOneSession holds the IMAP conn for up to
		// imapForcedReconnect, polling on the configured tick. Returns nil
		// on a clean ctx-cancel exit OR the hourly forced reconnect;
		// non-nil on error.
		err := s.runOneSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Clean session end (hourly forced reconnect). Do NOT count as
			// an error-path reconnect, sleep, or inflate the backoff. Reset
			// so a later real error starts fresh from 1s.
			backoff = initialBackoff
			continue
		}
		slog.Warn("email: IMAP session ended with error; will reconnect",
			"source", s.Name(), "err", s.redact(err.Error()), "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOneSession dials + LOGINs once, then polls on the configured tick
// until ctx is cancelled OR imapForcedReconnect elapses. The returned error
// is the reason for session end; nil on a clean ctx-cancel.
func (s *Source) runOneSession(ctx context.Context) error {
	cfg := &s.cfg
	addr := fmt.Sprintf("%s:%d", cfg.IMAP.Host, cfg.IMAP.Port)
	// tls.Config{ServerName} is always passed explicitly so a self-host on
	// a non-matching dns name still works.
	tlsConfig := &tls.Config{ServerName: cfg.IMAP.Host}

	// Stuck-SYN guard: an explicit 15s dial timeout so a dead IMAP host
	// fails fast instead of pinning on the kernel's tcp_syn_retries (~3min).
	opts := &imapclient.Options{
		TLSConfig: tlsConfig,
		Dialer:    &net.Dialer{Timeout: 15 * time.Second},
	}
	var (
		c   *imapclient.Client
		err error
	)
	switch cfg.IMAP.Security {
	case secNone:
		c, err = imapclient.DialInsecure(addr, opts)
	case secSTARTTLS:
		c, err = imapclient.DialStartTLS(addr, opts)
	default: // secTLS
		c, err = imapclient.DialTLS(addr, opts)
	}
	if err != nil {
		return fmt.Errorf("dial %s (%s): %w", addr, cfg.IMAP.Security, err)
	}
	defer func() { _ = c.Close() }()

	// ctx-cancel watcher. go-imap v2 commands block on the socket and take
	// no context; runOneSession only checks ctx between polls. Closing the
	// client from here unblocks any in-flight command at once. Torn down
	// via sessionDone on normal return (LIFO: close(sessionDone) runs
	// before the deferred c.Close above), so it never races a reused
	// client; a double Close is benign.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-sessionDone:
		}
	}()

	if err := c.Login(cfg.LoginUser(), s.password).Wait(); err != nil {
		return fmt.Errorf("LOGIN: %w", err)
	}
	if _, err := c.Select(cfg.Folder, &imap.SelectOptions{}).Wait(); err != nil {
		return fmt.Errorf("SELECT %q: %w", cfg.Folder, err)
	}

	pollInterval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	sessionDeadline := time.NewTimer(imapForcedReconnect)
	defer sessionDeadline.Stop()

	// Poll once immediately, then on every tick.
	if err := s.pollOnce(ctx, c); err != nil {
		return fmt.Errorf("first poll: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sessionDeadline.C:
			slog.Debug("email: hourly forced IMAP reconnect", "source", s.Name())
			return nil // returns to runPollLoop's reconnect path
		case <-tick.C:
			if err := s.pollOnce(ctx, c); err != nil {
				return fmt.Errorf("poll: %w", err)
			}
		}
	}
}

// pollOnce runs one SEARCH UNSEEN + handle-each-result cycle. Per-message
// handling is wrapped in panic-recover so a corrupt MIME doesn't kill the
// loop.
//
// Two-stage fetch: each uid first fetches MINIMAL headers (Message-ID) and
// runs the in-memory dedup. Only an un-deduped message gets the full
// BODY[] fetch + disk write. (The sender SCREEN is the gate's job, above
// this layer; the stage-1 peek here is purely dedup so a re-shown unseen
// message isn't re-fetched whole.)
// clampUIDs bounds one poll's unseen set so it never exceeds what the dedup
// ring can hold. Beyond dedupCap the ring would evict ids that are STILL in
// the unseen set, and with mark_seen:false the next poll re-emits (and
// re-replies to) the evicted mail — a domino over the entire backlog every
// poll (F26). Clamping to the NEWEST cap (SEARCH returns ascending uids — the
// tail) keeps the ring covering everything processed; older mail is simply not
// emitted until the unseen count drops (with mark_seen:true the backlog drains
// in cap-sized chunks; with mark_seen:false it needs a human to read or
// archive the mailbox).
func (s *Source) clampUIDs(uids []imap.UID) []imap.UID {
	if len(uids) <= dedupCap {
		return uids
	}
	slog.Error("email: unseen count exceeds dedup capacity — processing only the newest",
		"source", s.Name(), "unseen", len(uids), "cap", dedupCap,
		"hint", "with mark_seen:false the mailbox must stay under the cap or older mail is never emitted")
	return uids[len(uids)-dedupCap:]
}

func (s *Source) pollOnce(ctx context.Context, c *imapclient.Client) error {
	// NOOP before SEARCH so the server flushes its pending unilateral
	// updates (the EXISTS for mail that arrived since SELECT / the last
	// poll) into this session. Without it a long-lived session only ever
	// finds mail present at SELECT time.
	if err := c.Noop().Wait(); err != nil {
		return fmt.Errorf("NOOP: %w", err)
	}
	crit := &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	data, err := c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return fmt.Errorf("UID SEARCH UNSEEN: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil
	}
	uids = s.clampUIDs(uids)
	slog.Debug("email: poll found unseen", "source", s.Name(), "count", len(uids))
	for _, uid := range uids {
		if ctx.Err() != nil {
			return nil
		}
		// One panic recover scope per message — bound the blast radius of
		// a malformed MIME stream.
		func(uid imap.UID) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("email: panic during message handling — skipping",
						"source", s.Name(), "uid", uid, "panic", fmt.Sprintf("%v", r))
				}
			}()
			if err := s.screenAndMaybeEmit(ctx, c, uid); err != nil {
				// Error grading (F23): only a PERMANENT content defect (bad MIME,
				// no From) is \Seen'd immediately — re-parsing it every poll is
				// pointless. A TRANSIENT transport/server error (screen-fetch, no
				// message/body section, fetch close) is left UNSEEN so a reconnect
				// re-fetches it, so a server's momentary NO on a live connection
				// doesn't permanently lose the mail. That retry is bounded per-uid:
				// a message that fails transiently fetchFailureCap times is a poison
				// message → \Seen it to break the re-fetch loop.
				//
				// All \Seen writes still honour mark_seen:false — the operator's
				// opt-out means a broken message re-screens cheaply each poll (the
				// dedup ring / cheap header pass suppress re-emit).
				markSeen := isPermanent(err)
				if !markSeen {
					if n := s.bumpFetchFailure(uid); n >= fetchFailureCap {
						markSeen = true
						if s.cfg.ShouldMarkSeen() {
							slog.Error("email: uid exceeded transient-failure cap — marking \\Seen to break the re-fetch loop",
								"source", s.Name(), "uid", uid, "failures", n, "err", s.redact(err.Error()))
						} else {
							slog.Error("email: uid exceeded transient-failure cap — mark_seen disabled, cannot break the re-fetch loop",
								"source", s.Name(), "uid", uid, "failures", n, "err", s.redact(err.Error()))
						}
					}
				}
				marked := markSeen && s.cfg.ShouldMarkSeen()
				// B16: once markSeen is decided true (permanent defect OR cap hit),
				// the per-uid transient counter has done its job — clear its entry so
				// fetchFail stays bounded by the live unseen set (10-email.go:66-73),
				// even when STORE is disallowed (mark_seen:false) and the row is never
				// actually \Seen'd.
				if markSeen {
					s.clearFetchFailure(uid)
				}
				slog.Warn("email: fetch/parse failed for one message",
					"source", s.Name(), "uid", uid, "permanent", isPermanent(err),
					"marked_seen", marked, "err", s.redact(err.Error()))
				if marked {
					_ = s.markSeen(c, uid)
				}
				return
			}
			// Success — clear any prior transient-failure count for this uid.
			s.clearFetchFailure(uid)
		}(uid)
	}
	return nil
}

// screenAndMaybeEmit is the two-stage fetch entry. Stage 1 pulls just the
// Message-ID (PEEK so \Seen is not set) and runs the cheap in-memory dedup.
// Stage 2 (full body fetch + MIME parse + emit) runs only for a
// not-yet-seen message.
//
// Sender access control is NOT done here — that's the inbound gate's job
// (above this layer). Email captures every inbound and lets the gate
// reject; the only screen at this stage is dedup, so a server re-showing an
// already-handled unseen message isn't re-fetched whole.
func (s *Source) screenAndMaybeEmit(ctx context.Context, c *imapclient.Client, uid imap.UID) error {
	cfg := &s.cfg
	hdr, err := fetchScreenHeaders(c, uid)
	if err != nil {
		return fmt.Errorf("screen-fetch: %w", err)
	}
	if hdr.messageID == "" {
		// Header-less mail: synthesise the SAME stand-in fetchAndEmit
		// derives (uid + source) so both stages dedup on one canonical id.
		hdr.messageID = fmt.Sprintf("noid-uid%d@%s", uid, s.Name())
	}
	if s.dedupContains(hdr.messageID) {
		// Already-processed message that the server still shows as unseen —
		// flag it so the next poll doesn't include it again.
		if cfg.ShouldMarkSeen() {
			_ = s.markSeen(c, uid)
		}
		return nil
	}
	// Un-deduped → do the full fetch + emit.
	return s.fetchAndEmit(ctx, c, uid)
}

// screenHeader is the minimal routing field fetched in stage 1: just the
// Message-ID, enough to dedup. The reply-thread chain (In-Reply-To /
// References) and From are rebuilt by parseMIME on the stage-2 full fetch.
type screenHeader struct {
	messageID string // angle brackets stripped
}

// fetchScreenHeaders runs BODY.PEEK[HEADER.FIELDS (Message-ID)] for one
// uid — a few hundred bytes vs the full message. PEEK so the server doesn't
// auto-set \Seen on us; we own the \Seen lifecycle explicitly.
func fetchScreenHeaders(c *imapclient.Client, uid imap.UID) (screenHeader, error) {
	section := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"Message-ID"},
		Peek:         true,
	}
	opts := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}
	cmd := c.Fetch(imap.UIDSetNum(uid), opts)
	defer cmd.Close()
	msg := cmd.Next()
	if msg == nil {
		return screenHeader{}, fmt.Errorf("no message for uid %d", uid)
	}
	var literal *imapclient.FetchItemDataBodySection
	for {
		item := msg.Next()
		if item == nil {
			break
		}
		if bs, ok := item.(imapclient.FetchItemDataBodySection); ok {
			literal = &bs
			break
		}
	}
	if literal == nil || literal.Literal == nil {
		return screenHeader{}, fmt.Errorf("no header section in fetch response")
	}
	return parseScreenHeader(literal.Literal)
}

// fetchAndEmit pulls the full body for one uid, parses it, and emits the
// resulting MessageEvent. Dedup is upstream (screenAndMaybeEmit).
func (s *Source) fetchAndEmit(ctx context.Context, c *imapclient.Client, uid imap.UID) error {
	cfg := &s.cfg
	bodySection := &imap.FetchItemBodySection{}
	fetchOpts := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}
	fetchCmd := c.Fetch(imap.UIDSetNum(uid), fetchOpts)

	msg := fetchCmd.Next()
	if msg == nil {
		_ = fetchCmd.Close()
		return fmt.Errorf("no message returned for uid %d", uid)
	}
	var literal *imapclient.FetchItemDataBodySection
	for {
		item := msg.Next()
		if item == nil {
			break
		}
		if bs, ok := item.(imapclient.FetchItemDataBodySection); ok {
			literal = &bs
			break
		}
	}
	if literal == nil || literal.Literal == nil {
		_ = fetchCmd.Close()
		return fmt.Errorf("no body section in fetch response")
	}

	subdir := s.resolveSubdir()
	pe, err := parseMIME(literal.Literal, s.files, subdir, cfg.MaxAttachmentBytes)
	// Close the FETCH command now that its body literal is consumed.
	// go-imap v2 blocks the client decoder until the running command is
	// closed, so the markSeen / deleteMessage STOREs below — issued on the
	// SAME client — would deadlock if the fetch were still open.
	if cerr := fetchCmd.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("fetch close: %w", cerr)
	}
	if err != nil {
		// A MIME structure error is a permanent content defect — \Seen it so we
		// don't re-parse a broken message every poll. (Transport errors above —
		// no message / no body section / fetch close — stay transient.)
		return permanent(fmt.Errorf("parseMIME: %w", err))
	}
	if pe.MessageID == "" {
		// Some senders / lists omit Message-ID — derive a stable stand-in
		// from uid + folder so dedup keys don't collide across accounts.
		pe.MessageID = fmt.Sprintf("noid-uid%d@%s", uid, s.Name())
	}
	if pe.FromAddress == "" {
		slog.Warn("email: message without parseable From — skipping",
			"source", s.Name(), "uid", uid, "message_id", pe.MessageID)
		// Commit the dedup id before returning so the in-memory ring
		// suppresses re-emit on subsequent polls even when \Seen can't be
		// set (mark_seen:false).
		s.markSeenAndCheck(pe.MessageID)
		return permanent(fmt.Errorf("no From address"))
	}

	// Defence-in-depth dedup: stage-1 took the cheap header-only pass, but
	// the full parse can surface a different / synthesised Message-ID.
	if s.markSeenAndCheck(pe.MessageID) {
		// Already emitted, but if a prior poll's \Seen STORE failed this message is
		// still UNSEEN — we'd full-body re-fetch it every poll forever (the post-emit
		// markSeen below is unreachable on this dedup short-circuit). Retry the
		// idempotent STORE here so the flag eventually sticks.
		if s.cfg.ShouldMarkSeen() {
			_ = s.markSeen(c, uid)
		}
		return nil
	}

	// Forwarded-alias resolution (only when forwarded_domains is set): the
	// ORIGINAL recipient (agent@/sales@/… of your own domain) a forwarder
	// delivered this for. Carried on ev.ThreadID so it (a) splits the DM
	// scope per-alias upstream and (b) keys the agora matcher on the
	// recipient — WITHOUT touching ChatID (which doubles as the outbound
	// recipient address). Resolved BEFORE the quote-strip so the prior-body
	// cache is per-alias.
	pe.ForwardedAlias = resolveForwardedAlias(pe, cfg.ForwardedDomains)

	body := pe.TextBody
	if cfg.StripInboundQuotes != nil && *cfg.StripInboundQuotes {
		// Prior outbound body feeds the chunked-substring strip. In-memory
		// cache only (stashed by a recent Sink.Send for this sender+alias);
		// on a miss (e.g. first inbound after a restart) fall back to
		// regex-only. (D-B5)
		key := inboxKey(pe.FromAddress, pe.ForwardedAlias)
		prior := s.lookupPriorBody(key)
		body = StripInboundQuotesWithPrior(body, prior)
	}
	body = strings.TrimSpace(body)

	// Subject fallback (F22): a mail whose body is empty after strip — an
	// HTML-only part that stripped to nothing, an all-quoted reply, or a
	// subject-is-the-whole-message FYI — still carries intent in its Subject.
	// Fall back to it so the message opens a turn instead of being silently
	// dropped at submit (empty text + no attachments → bare return), and so an
	// attachment-only mail carries its real Subject instead of the generic
	// NoCaptionStandIn. ONLY when the body is empty: a mail WITH body keeps the
	// Subject out of the turn input (it is "Re:" noise on every reply).
	if body == "" {
		if subj := strings.TrimSpace(pe.Subject); subj != "" {
			body = subj
		}
	}

	ev := contract.MessageEvent{
		Source:    s.Name(),
		ChatType:  contract.ChatDM,
		ChatID:    pe.FromAddress,
		ThreadID:  pe.ForwardedAlias,
		UserID:    pe.FromAddress,
		UserName:  senderDisplay(pe),
		MessageID: pe.MessageID,
		ReplyToID: pe.InReplyTo,
		ReplyRefs: referenceIDs(pe.References),
		// Email is async + threaded: a reply continues its thread's
		// session, a freshly-composed mail (no In-Reply-To / References
		// hit) opens a new one. Opt into per-reply DM session routing.
		DMReplyRouting: true,
		// IsAdmin is the gate's call; the source doesn't know it.
		//
		// ACCEPT-RISK (hector): ChatID/UserID — and therefore the
		// gate's allowlist AND admin verdicts — trust the spoofable From header;
		// no SPF/DKIM verification here. The upstream receiving MTA is relied on
		// to enforce sender authentication (same posture as the SSH-hostkey
		// accept-risk). Revisit only if the mailbox ever moves to an MTA that
		// doesn't filter, or by reading Authentication-Results (forwarded_domains
		// aliasing makes strict DKIM alignment nontrivial).
		IsAdmin:     false,
		Text:        body,
		Attachments: pe.Attachments,
		ReceivedAt:  time.Now(),
	}

	// Stash the parsed metadata so the agent's reply (next turn) can build
	// threading headers + a minimal quote without re-parsing the MIME.
	s.stashInboundContextFromMessage(pe)

	// Emit FIRST, then mark \Seen: the IMAP \Seen flag is the durable queue. An
	// un-emitted message — ctx cancelled mid-send on a graceful shutdown — stays
	// UNSEEN and is re-fetched next boot (at-least-once). Marking before the emit
	// was at-most-once but silently lost the last in-flight message every
	// shutdown. The dedup ring guards a within-process re-fetch; a cross-restart
	// re-fetch may re-reply once (the at-least-once tradeoff).
	select {
	case s.out <- ev:
	case <-ctx.Done():
		return nil // not emitted → leave UNSEEN for the next boot to re-fetch
	}
	if cfg.ShouldMarkSeen() {
		if err := s.markSeen(c, uid); err != nil {
			slog.Warn("email: markSeen failed — message may be re-fetched",
				"source", s.Name(), "uid", uid, "err", s.redact(err.Error()))
		}
	}
	if cfg.DeleteAfterProcess {
		if err := s.deleteMessage(c, uid); err != nil {
			slog.Warn("email: delete after process failed",
				"source", s.Name(), "uid", uid, "err", s.redact(err.Error()))
		}
	}
	return nil
}

// markSeen sets the \Seen flag on uid.
func (s *Source) markSeen(c *imapclient.Client, uid imap.UID) error {
	storeFlags := imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagSeen},
		Silent: true,
	}
	return c.Store(imap.UIDSetNum(uid), &storeFlags, nil).Close()
}

// deleteMessage sets \Deleted on uid (an EXPUNGE would be needed to
// actually remove; we skip that — admin can configure their server to
// auto-expunge, or run a separate cron).
func (s *Source) deleteMessage(c *imapclient.Client, uid imap.UID) error {
	storeFlags := imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagDeleted},
		Silent: true,
	}
	return c.Store(imap.UIDSetNum(uid), &storeFlags, nil).Close()
}

// resolveSubdir returns the attachment staging bucket. A fixed bucket equal
// to the source name (the per-session_scope resolver is the inbound
// pipeline's job, above this layer).
func (s *Source) resolveSubdir() string {
	return s.Name()
}
