// Package feishu is the Feishu (海外品牌 Lark) Source — bob's touchpoint on
// the 字节系 enterprise IM. It connects through the lark Go SDK's built-in
// WebSocket long-connection (no inbound HTTP route needed), receives messages
// via the event dispatcher, and replies through the IM REST API.
//
// REBUILD scope: receive (group → only when the bot is @'d or a reply-to-bot;
// p2p → always) + send via the shared streamsink core in BLOCK discipline
// (Feishu's message-edit endpoint is rate- and lifetime-capped, so live edit-
// streaming is deferred) + inbound attachment ingest (image/file/audio/media/
// post → staged for OCR/transcribe preprocess) + reactions (read-ack) + the
// interactive-card / post-content rendering nuances in the Wire. Access control
// (allow/deny/admin) is the gate's job (the inbound screen — screened by default).
// Inline-button callbacks and message-index reply-routing are deferred.
//
// Boundary: the lark SDK (github.com/larksuite/oapi-sdk-go/v3 and subpackages)
// appears ONLY in sources/feishu/*.go.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/files"
	"agentbob/leaf/stoma/sources/sendgate"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// feishuMaxBytes is the per-message outbound budget (UTF-8 bytes) = the streamsink
// split threshold. Held AT feishuCardMaxBytes (25000) so every split chunk still fits
// the interactive card: a larger budget (e.g. the old 30KB) let a middle chunk exceed
// the card cap and render as literal-markdown plain text while the short tail chunk
// rendered as a card — one reply, two visual forms. Feishu's text hard cap is ~150KB,
// so 25KB stays conservative. The streamsink core splits a longer reply into a chain
// of Create calls.
const feishuMaxBytes = 25000

// feishuCardMaxBytes caps the reply length rendered as an interactive markdown
// card (markup over this goes plain text instead — complete, markdown shown
// literally). It is a MARKDOWN-BODY length budget, not the wire size: the card
// JSON wrapper + JSON-escaping add overhead on top.
//
// Live probe against the real tenant: the single-markdown-element
// card renders its FULL body intact at every size up to ~30KB — no silent clip.
// The card request-body ceiling is 30KB (im-v1/message/create); a 30003B body
// wrapped to 30720B and was still accepted + shown in full. 25000 keeps the
// wrapped body comfortably under 30KB even for quote/backslash-heavy content
// (worst-case JSON-escaping expansion), reclaiming the whole 1.5–30KB band that
// used to drop to literal-markup plain text.
const feishuCardMaxBytes = 25000

// Source implements contract.Source for Feishu. One instance per app (one bot);
// a single bob process can run several (distinct app credentials, each its own
// uniquely-named Source).
type Source struct {
	// name is the unique source name ("feishu", "feishu2", …). It flows into
	// session keys, source health, and the rejected log, so each bot routes in
	// its own namespace (reply-to-bot never crosses bots).
	name string

	appID     string
	appSecret string
	api       *lark.Client // IM REST client for sends

	// botOpenID is THIS bot's own open_id, best-effort fetched once at Run
	// start (bot/v3/info). Used by mentionsBot to answer an @-mention only when
	// it targets THIS bot — a guard for the (unsupported) case of two bob bots
	// in one group. Empty when the fetch failed → mentionsBot falls back to the
	// single-bot "any bot mention = me" behaviour. Set before the WS pump
	// starts, then read-only.
	botOpenID string

	// files is the inbound attachment store (nil → don't capture). maxAttBytes
	// is the per-file cap.
	files       *files.Store
	maxAttBytes int64

	out chan<- contract.MessageEvent

	// parentCache memoizes resolved reply-parent quote info (text + sender) by
	// parent message id so N image-replies to one prompt don't refetch the same
	// parent N times. Guarded by parentMu (onMessageReceive runs concurrently).
	parentMu    sync.Mutex
	parentCache map[string]replyParentInfo

	// nameCache memoizes a sender's display name by open_id. The inbound event carries
	// only ids (no name), so the name is resolved once via Message.Get and reused for the
	// per-message author + the group "[名字]: " attribution. Guarded by nameMu.
	nameMu    sync.Mutex
	nameCache map[string]string

	// recvQueue decouples the WS ack pump from message processing: the handler
	// only enqueues here (fast), and a fixed worker pool drains it under bounded
	// concurrency. The deep buffer absorbs a media storm so the pump never blocks
	// on a synchronous download — a delayed ack is exactly what triggers Feishu's
	// at-least-once redelivery, so blocking the pump would amplify the storm.
	// Made in Run.
	recvQueue chan *larkim.P2MessageReceiveV1
	// recvSeen dedups Feishu's at-least-once WS redelivery (triggered when a
	// slow handler delays the ack). message_id → seen. Guarded by recvSeenMu;
	// bounded by drop-on-cap, mirroring parentCache.
	recvSeenMu sync.Mutex
	recvSeen   map[string]struct{}

	// gate is the bot's outbound serialiser + shared 429 cooldown + bounded 429
	// relay (sendgate.Gate). Inbound rides the WS long-connection, but sends go over
	// the REST API — without this, two concurrent turns replying into the same tenant
	// fire Message.Create/Reply in parallel and overrun feishu's per-bot rate limit,
	// with no shared backoff. Mirrors the telegram source's gate. (L-FEISHU-D1)
	gate *sendgate.Gate

	connected atomic.Bool // WS link up (SetOnReady/Disconnected)
	closed    atomic.Bool // Run teardown began
}

const (
	// recvConcurrency is the number of worker goroutines draining recvQueue.
	recvConcurrency = 8
	// recvQueueDepth is the buffer that absorbs a burst so the WS pump's enqueue
	// stays fast; the pump only blocks if a sustained storm fills it (rare).
	recvQueueDepth = 256
	// recvSeenMax bounds the dedup map; over it the whole map is dropped
	// (parentCache's bound) — far above any plausible redelivery burst, so a
	// redelivery (which arrives within seconds) is never evicted before it.
	recvSeenMax = 4096
)

// New builds a Feishu source named `name` ("feishu" for the primary bot). The
// app id and secret are read from the named env vars (appIDEnv/appSecretEnv);
// both are required. fileStore may be nil to disable inbound attachment capture.
// The secret never leaves this package except to build the SDK clients and to
// redact it out of error strings.
func New(name, appIDEnv, appSecretEnv string, maxAttBytes int64, fileStore *files.Store) (*Source, error) {
	if name == "" {
		name = "feishu"
	}
	if appIDEnv == "" {
		appIDEnv = "FEISHU_APP_ID"
	}
	if appSecretEnv == "" {
		appSecretEnv = "FEISHU_APP_SECRET"
	}
	appID := os.Getenv(appIDEnv)
	appSecret := os.Getenv(appSecretEnv)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("feishu: env %s and %s must both be set for source %q", appIDEnv, appSecretEnv, name)
	}
	// The SDK's default (no WithHttpClient) is http.DefaultClient with NO
	// timeout at all: a server that accepts the connection but never responds
	// would hang the inbound paths forever (they run on the WS pump's
	// process-lifetime ctx, so one wedged call = a leaked goroutine + a
	// silently dropped message). ResponseHeaderTimeout — rather than a whole-
	// request Timeout — bounds exactly that wedge without capping how long a
	// large attachment upload/download body may legitimately take.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	s := &Source{
		name:      name,
		appID:     appID,
		appSecret: appSecret,
		// LogLevelError: keep the REST client off stdout at Info (parity with the
		// WS client below) so SDK chatter stays out of bob's structured logs.
		api: lark.NewClient(appID, appSecret, lark.WithLogLevel(larkcore.LogLevelError),
			lark.WithHttpClient(&http.Client{Transport: tr})),
		files:       fileStore,
		maxAttBytes: maxAttBytes,
		gate:        sendgate.New(feishuRateLimited),
	}
	return s, nil
}

func (s *Source) Name() string { return s.name }

// Caps: ReplyTo is on — the gateway stamps the inbound message id into
// Target.ReplyToID, and the wire anchors the turn's first message to it via the
// reply endpoint (20-stream.go). Screened by default (not Trusted) — sender screening (deny / redeem /
// allow) runs in the gate. RedeemConfirmAll — confirm a group wire-code / claim-code
// redeem in the group too (aligned with telegram + discord); the code was already
// typed publicly in the group, so the "✅ bound" reply leaks nothing further.
func (s *Source) Caps() contract.Caps {
	return contract.Caps{ReplyTo: true, RedeemConfirmAll: true} // screened by default (not Trusted)
}

// HealthCheck reports whether the WebSocket link is currently up. The lark WS
// client manages its own reconnect; this just surfaces the last known state to
// the bus's periodic probe.
func (s *Source) HealthCheck(ctx context.Context) error {
	if !s.connected.Load() {
		return fmt.Errorf("feishu: websocket not connected")
	}
	return nil
}

// SendButtons degrades to a plain text Send: inline-button callback routing is
// deferred (no handler registry), so buttons are dropped rather than rendered as
// dead controls. Returns "" (no tracked message id).
func (s *Source) SendButtons(ctx context.Context, t contract.Target, text string, _ []contract.Button) (string, error) {
	return "", s.Send(ctx, t, text)
}

// Run opens the WebSocket long-connection and pumps inbound events until ctx is
// cancelled. The lark WS client's Start blocks forever (it ends in select{} on
// success and has no ctx-aware return — upstream graceful-shutdown gap), so we
// run it in a goroutine. On ctx cancel we Close() the socket and return; note
// Close() tears the connection but CANNOT unblock Start's select{}, so the
// Start + pingLoop goroutines intentionally outlive Run until process exit.
// That is acceptable because the bus invokes Run serially (at most a few
// boot-window retries, then exactly one serving run) and a failed source is
// otherwise recovered by a process-level restart. A connect-time failure (bad
// credentials) surfaces as an early Start error.
//
// Serially re-invocable — the bus retries a boot-window exit (leaf/stoma
// runSource), so the closed latch is reset here and each run binds its OWN
// recvQueue to its workers (a failed run's workers drain their dead queue until
// ctx ends; nothing feeds it again).
func (s *Source) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	s.closed.Store(false) // a prior failed Run latched it; this run is serving again
	s.out = out

	// Best-effort: learn THIS bot's own open_id BEFORE the WS pump starts, so
	// mentionsBot can distinguish an @-of-me from an @-of-another-bot (the guard
	// for two bob bots sharing a group). A failure is non-fatal — mentionsBot
	// degrades to the single-bot "any bot mention = me" behaviour. Set here,
	// before the goroutine below, so onMessageReceive reads it race-free.
	if oid, err := s.fetchSelfOpenID(ctx); err != nil {
		slog.Warn("feishu: could not fetch bot open_id — @-mention matching falls back to any-bot",
			"source", s.name, "err", s.redactErr(err))
	} else {
		s.botOpenID = oid
	}

	s.recvQueue = make(chan *larkim.P2MessageReceiveV1, recvQueueDepth)
	recvQueue := s.recvQueue // this run's queue — workers + the WS handler bind it, not the field
	for i := 0; i < recvConcurrency; i++ {
		go s.recvWorker(ctx, recvQueue)
	}

	// Empty verification token + encrypt key = WS long-connection mode (the
	// SDK only needs those for webhook signature verification).
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, e *larkim.P2MessageReceiveV1) error {
			// Ack immediately. Processing inline used to defer the WS ack behind
			// synchronous attachment downloads / parent fetches — Feishu then
			// redelivers (at-least-once), and with no dedup the message was
			// reprocessed.
			//
			// (1) message_id dedup, marked BEFORE the async dispatch so two
			// near-simultaneous redeliveries can't both pass.
			if id := eventMessageID(e); id != "" && !s.markRecvSeen(id) {
				return nil
			}
			// (2) hand off to the worker pool and ack immediately; the deep buffer
			// keeps this enqueue fast so the WS pump never blocks on a download.
			select {
			case recvQueue <- e:
			case <-ctx.Done():
			}
			return nil
		})

	wsCli := larkws.NewClient(s.appID, s.appSecret,
		larkws.WithEventHandler(handler),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithOnReady(func() {
			s.connected.Store(true)
			slog.Info("feishu: websocket connected")
		}),
		larkws.WithOnDisconnected(func() {
			s.connected.Store(false)
			slog.Warn("feishu: websocket disconnected")
		}),
		// The SDK fires onReady only on the FIRST connect; a later auto-
		// reconnect fires onReconnected. Without this, connected stays false
		// forever after the first blip and HealthCheck flaps to permanent
		// failure (false admin alerts) while the link is actually healthy.
		larkws.WithOnReconnected(func() {
			s.connected.Store(true)
			slog.Info("feishu: websocket reconnected")
		}),
	)

	errc := make(chan error, 1)
	go func() { errc <- wsCli.Start(ctx) }()

	select {
	case <-ctx.Done():
		s.closed.Store(true)
		s.connected.Store(false)
		wsCli.Close()
		return ctx.Err()
	case err := <-errc:
		// Start returned before blocking → a connect-time ClientError
		// (almost always bad app_id/app_secret). Surface it; the bus retries a
		// Run exit only inside its boot window (leaf/stoma runSource), after
		// that the source is marked dead for the process lifetime.
		s.closed.Store(true)
		s.connected.Store(false)
		if err == nil {
			err = fmt.Errorf("websocket loop exited")
		}
		return fmt.Errorf("feishu: websocket start: %w", err)
	}
}

// fetchSelfOpenID best-effort resolves THIS bot's own open_id via the bot info
// endpoint (GET /open-apis/bot/v3/info, tenant token = the app's own identity).
// mentionsBot uses it to answer an @-mention only when it targets this bot.
// Returns ("", err) on any failure; the caller degrades to single-bot mention
// behaviour rather than failing startup.
func (s *Source) fetchSelfOpenID(ctx context.Context) (string, error) {
	if s.api == nil {
		return "", fmt.Errorf("feishu: no api client")
	}
	resp, err := s.api.Do(ctx, &larkcore.ApiReq{
		HttpMethod:                http.MethodGet,
		ApiPath:                   "/open-apis/bot/v3/info",
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
	})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("feishu: bot/v3/info status %d", resp.StatusCode)
	}
	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		return "", err
	}
	if body.Code != 0 {
		return "", fmt.Errorf("feishu: bot/v3/info code=%d msg=%s", body.Code, body.Msg)
	}
	if body.Bot.OpenID == "" {
		return "", fmt.Errorf("feishu: bot/v3/info returned empty open_id")
	}
	return body.Bot.OpenID, nil
}

// eventMessageID extracts the inbound message_id, or "" when the envelope is
// incomplete (the dedup is simply skipped for a malformed event).
func eventMessageID(e *larkim.P2MessageReceiveV1) string {
	if e == nil || e.Event == nil || e.Event.Message == nil {
		return ""
	}
	return str(e.Event.Message.MessageId)
}

// markRecvSeen records msgID and returns true when it is FRESH (caller should
// process), false when it was already seen — a Feishu at-least-once
// redelivery to suppress. Bounded by drop-on-cap, mirroring parentCache.
func (s *Source) markRecvSeen(msgID string) bool {
	s.recvSeenMu.Lock()
	defer s.recvSeenMu.Unlock()
	if s.recvSeen == nil {
		s.recvSeen = make(map[string]struct{})
	}
	if _, ok := s.recvSeen[msgID]; ok {
		return false
	}
	if len(s.recvSeen) >= recvSeenMax {
		s.recvSeen = make(map[string]struct{}) // simple bound (see parentCache)
	}
	s.recvSeen[msgID] = struct{}{}
	return true
}

// recvWorker drains queued message events under bounded concurrency. It recovers
// from a handler panic so one malformed event can't crash the process (the panic
// is on this worker goroutine, which the SDK dispatcher can't catch).
func (s *Source) recvWorker(ctx context.Context, queue <-chan *larkim.P2MessageReceiveV1) {
	for {
		select {
		case e := <-queue:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("feishu: message handler panicked", "panic", r)
					}
				}()
				s.onMessageReceive(ctx, e)
			}()
		case <-ctx.Done():
			return
		}
	}
}

// onMessageReceive turns an inbound Feishu message event into a
// contract.MessageEvent and emits it. Handles text plus image/file/audio/media/
// post attachments:
//
//   - non-user senders (apps / bots) are dropped — kills self-echo and
//     bot-to-bot loops.
//   - in a group, the message is ignored unless the bot was @'d OR it replies
//     to one of bob's own messages — this also gates image-only noise while
//     letting an image reply-to-bot through.
//   - a missing sender open_id drops the event (no identity fabrication).
//   - attachment-bearing types stage their media and emit even when there is no
//     accompanying text; an event with neither text nor attachments is dropped.
//   - unsupported message types are dropped with a debug log.
func (s *Source) onMessageReceive(ctx context.Context, e *larkim.P2MessageReceiveV1) {
	if e == nil || e.Event == nil || e.Event.Message == nil {
		return
	}
	msg := e.Event.Message

	// Only human messages. Feishu marks bot/app sends sender_type="app".
	if e.Event.Sender == nil || e.Event.Sender.SenderType == nil || *e.Event.Sender.SenderType != "user" {
		return
	}
	userID := senderOpenID(e)
	unionID := senderUnionID(e) // cross-app-stable; → ev.StableUID for accounts keying
	if userID == "" {
		slog.Warn("feishu: inbound message without sender open_id — dropping")
		return
	}

	chatID := str(msg.ChatId)
	if chatID == "" {
		slog.Warn("feishu: inbound message without chat_id — dropping")
		return
	}

	// Sender gating (denylist > bare-code redeem > allowlist, closed-default)
	// runs in the gate (the inbound screen). The mention-stripped ev.Text built below
	// ("@_user_N" placeholders excised) is what reaches the gate's exact-match
	// redeem.

	// Resolve reply linkage BEFORE the group gate: a reply to one of bob's own
	// messages must pass the gate even without an @-mention (mirrors telegram).
	// Detecting reply-to-bot needs the parent's sender, which is a (cached)
	// Message.Get — fetch it here ONLY when it can change the gate outcome: a group
	// message that isn't already @'d and carries a parent. That fetch is reused for
	// the quote below, so a reply costs at most one Message.Get per parent. (An @'d
	// or DM message passes the gate regardless and defers the fetch to the quote
	// step; a non-reply never fetches.)
	parentID := str(msg.ParentId)
	replyToID := parentID
	chatType := str(msg.ChatType)
	mentioned := mentionsBot(msg.Mentions, s.botOpenID)

	// Fetch the reply parent ONCE for any reply: the parent's sender drives BOTH the
	// group-noise gate (replyToBot) and the quote context below. parentInfo is memoised
	// (parentCache), so the former two-phase fetch was already ≤1 Message.Get per parent;
	// folding it to a single unconditional fetch drops the pinfoFetched bookkeeping at no
	// extra network cost. replyToBot must reflect ALL replies (incl. an @'d one) so an
	// @+reply continues the replied-to session instead of forking a new @=new one —
	// computing it here once covers that; the gate ignores it anyway when @'d (the mention
	// already passes), and a p2p chat is never group-noise-gated. (M6)
	var pinfo replyParentInfo
	replyToBot := false
	if parentID != "" {
		pinfo = s.parentInfo(ctx, parentID)
		replyToBot = pinfo.fromBot
	}
	if shouldDropAsGroupNoise(chatType, mentioned, replyToBot) {
		return // group noise: only respond when @'d or replying to bob (gates image-only group msgs)
	}

	// Quote context (text/sender) from the SAME fetched parent. Empty on a fetch error —
	// replyToID / replyToBot stand.
	var replyToText, replyToUser string
	if parentID != "" {
		replyToText, replyToUser = pinfo.text, pinfo.user
	}

	// Build the event up-front (without Text/Attachments) so collectAttachments
	// can resolve the scope-blind staging subdir from it before downloading —
	// same ordering as telegram. Reply fields are set here so they reach the
	// emitted ev regardless of message type (an image reply is the key case
	// that unblocks the group image→inbox→OCR flow).
	ct := mapChatType(chatType)
	// Resolve the sender's display name only in GROUPS — that's where the "[名字]: "
	// attribution prefix is rendered; a DM needs no prefix, so skip the Message.Get cost
	// there. Cached per sender, so at most one fetch per distinct group speaker.
	userName := ""
	if ct == contract.ChatGroup {
		userName = s.senderName(ctx, userID, str(msg.MessageId))
	}
	ev := contract.MessageEvent{
		Source:      s.name,
		ChatType:    ct,
		ChatID:      chatID,
		MessageID:   str(msg.MessageId),
		ReplyToID:   replyToID,
		ReplyToBot:  replyToBot,
		ReplyToText: replyToText,
		ReplyToUser: replyToUser,
		UserID:      userID,
		UserName:    userName,
		StableUID:   unionID, // cross-app union_id for accounts keying
		// IsAdmin is the gate's call (the inbound screen); the source doesn't know it.
		IsAdmin:    false,
		ReceivedAt: time.Now(),
	}

	var text string
	var atts []contract.Attachment
	switch str(msg.MessageType) {
	case "text":
		text = stripMentions(textContent(str(msg.Content)), msg.Mentions)
	case "image", "file", "audio", "media", "post":
		// post may also carry visible text alongside its inline images.
		text, atts = s.collectAttachments(ctx, msg, ev)
	default:
		slog.Debug("feishu: unsupported message type dropped",
			"type", str(msg.MessageType), "chat_type", chatType)
		return
	}

	// Drop only when there is nothing to act on at all. An image-only message
	// (empty text, one attachment) MUST still emit; a bare @bob with no text and
	// no media is pure noise.
	if text == "" && len(atts) == 0 {
		return
	}

	ev.Text = text
	ev.Attachments = atts
	s.safeEmit(ctx, ev)
	// No source-internal media read-ack: the gateway's universal submit react
	// (session/40-submit.go) already fires 👀 → feishu "OnIt" on EVERY inbound incl.
	// media (ev carries ChatID + MessageID), so a media-only react here just set the
	// same "OnIt" twice. Removed (review).
}

// safeEmit delivers ev to the bus, guarding the shutdown race where the bus may
// close the events channel while a WS callback is still in flight.
func (s *Source) safeEmit(ctx context.Context, ev contract.MessageEvent) {
	if s.closed.Load() {
		return
	}
	defer func() { _ = recover() }() // out closed mid-send during teardown
	select {
	case s.out <- ev:
	case <-ctx.Done():
	}
}

// mapChatType maps Feishu's chat_type to bob's contract.ChatType. p2p → dm;
// group (and topic_group) → group. Unknown values fall back to group (safer
// than dm for a multi-party context).
func mapChatType(feishu string) contract.ChatType {
	if feishu == "p2p" {
		return contract.ChatDM
	}
	return contract.ChatGroup
}

// textContent parses a Feishu "text" message's content JSON ({"text":"..."})
// and returns the text. Malformed / non-text → "".
func textContent(content string) string {
	var c struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(content), &c) != nil {
		return ""
	}
	return c.Text
}

// str safely dereferences a *string SDK field ("" when nil).
func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var _ contract.Source = (*Source)(nil)
