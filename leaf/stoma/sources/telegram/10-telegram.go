// Package telegram is the Telegram Bot API source — a thin adapter on the shared
// source layer (streamsink for rendering, files for attachment capture, common
// for redaction). It receives updates, translates them to contract.MessageEvent,
// and renders replies through streamsink.
//
// In groups the bot only responds when @-mentioned or when a message replies to
// one of its own messages. Disable the bot's privacy mode in @BotFather to
// receive non-command group messages.
//
// REBUILD scope: receive + send + streamsink rendering + reactions + attachments
// (files store) + media-group coalescing. Access control (allow/deny/admin) is
// the gate's job (the inbound screen — screened by default). Inline-button
// callbacks are deferred. Reply routing is the flow's job (streamsink's
// OnAnchor/LastSent feed the MessageIndexer); the source doesn't participate.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/files"
	"agentbob/heartwood/prompt"
	"agentbob/leaf/stoma/sources/sendgate"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Source implements contract.Source for Telegram (one instance per bot token).
type Source struct {
	name        string
	maxAttBytes int64        // per-file attachment cap; 0 → store default
	files       *files.Store // nil → don't capture attachments
	token       string

	b        *bot.Bot
	out      chan<- contract.MessageEvent
	meID     int64
	meUser   string      // lowercase @username (without @), for mention detection
	meAt     string      // original-case "@username", human-facing self-mention (SelfMention)
	closed   atomic.Bool // flips true after Run exits
	mgBuffer *mediaGroupBuffer

	// gate is the bot's outbound serialiser + shared 429 cooldown + bounded 429
	// relay (sendgate.Gate). One Source is one bot token = one Telegram rate-limit
	// unit, so a single per-bot gate is the whole story: two concurrent group turns
	// each build their own streamsink (whose flushMu only serialises within ONE
	// sink), and without this they'd fire telegram calls in parallel and back off
	// independently, overrunning the limit (the "停了下来" 429 incident). Serialising
	// bot-wide — not per-chat — fixes that without a per-chat map that grows
	// unboundedly; the only cost is cross-chat sends are paced in one queue, which
	// is fine well under Telegram's ~30 msg/s/bot global limit.
	//
	// Routed through it (gate.Do): the streaming edits AND the one-off calls —
	// Send / SendFile / SendButtons / ReactToMessage. The lone deliberate exception is
	// the typing indicator (a fire-and-forget chat action): gating it would queue it
	// behind real sends and a throttled typing ping is harmless to drop.
	gate *sendgate.Gate
}

// New builds a Telegram source. The bot token is read from tokenEnv (default
// TELEGRAM_BOT_TOKEN). fileStore may be nil to disable attachment capture. The
// token never leaves this package except to build the bot client and to redact
// it out of error strings.
func New(name, tokenEnv string, maxAttBytes int64, fileStore *files.Store) (*Source, error) {
	if tokenEnv == "" {
		tokenEnv = "TELEGRAM_BOT_TOKEN"
	}
	tok := os.Getenv(tokenEnv)
	if tok == "" {
		return nil, fmt.Errorf("telegram: env %s is unset", tokenEnv)
	}
	return &Source{name: name, maxAttBytes: maxAttBytes, files: fileStore, token: tok, gate: sendgate.New(rateLimited429)}, nil
}

func (s *Source) Name() string { return s.name }

// SelfMention is the human-typeable handle that addresses THIS bot ("@botname") —
// what a user copies in a multi-bot group to direct a message at us. "" before
// connect / a bot without a username. Implements contract.SelfMentioner.
func (s *Source) SelfMention() string { return s.meAt }
func (s *Source) attMaxBytes() int64  { return s.maxAttBytes }

// Caps: screened by default (not Trusted) → the inbound screen runs sender access control. RedeemConfirmAll
// → confirm a group wire-code redeem in the group too. ReplyTo → anchor replies.
func (s *Source) Caps() contract.Caps {
	return contract.Caps{ReplyTo: true, RedeemConfirmAll: true} // screened by default (not Trusted)
}

// HealthCheck probes Telegram with getMe (cheap). The error is redacted so the
// token never leaks to logs.
func (s *Source) HealthCheck(ctx context.Context) error {
	if s.b == nil {
		return fmt.Errorf("telegram: source not yet started (bot client nil)")
	}
	if _, err := s.b.GetMe(ctx); err != nil {
		return redactTokenErr(err, s.token)
	}
	return nil
}

// Run connects and long-polls until ctx is cancelled. Serially re-invocable:
// the bus retries a boot-window exit (leaf/stoma runSource), so every per-run
// field — the bot client, the media-group buffer, the closed latch — is (re)set
// here, never assumed fresh from New.
func (s *Source) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	s.closed.Store(false) // a prior failed Run latched it; this run is serving again
	s.out = out
	// The media-group buffer needs the out channel + ctx so it can ride the
	// source's lifetime for backpressure and shutdown flush.
	s.mgBuffer = newMediaGroupBuffer(func(ev contract.MessageEvent) {
		s.safeEmit(ctx, ev)
	})
	b, err := bot.New(s.token,
		bot.WithDefaultHandler(s.onUpdate),
		bot.WithErrorsHandler(func(err error) {
			slog.Warn("telegram polling/handler error", "err", redactTokenErr(err, s.token))
		}),
	)
	if err != nil {
		s.closed.Store(true)
		return redactTokenErr(err, s.token)
	}
	s.b = b
	me, err := b.GetMe(ctx)
	if err != nil {
		s.closed.Store(true)
		return redactTokenErr(fmt.Errorf("telegram: GetMe failed: %w", err), s.token)
	}
	if me == nil {
		s.closed.Store(true)
		return fmt.Errorf("telegram: GetMe returned nil response (bot id unavailable)")
	}
	s.meID = me.ID
	s.meUser = strings.ToLower(me.Username)
	if me.Username != "" {
		s.meAt = "@" + me.Username // original case — what a human copies to address this bot
	}
	slog.Info("telegram connected", "bot", "@"+me.Username, "id", me.ID)

	b.Start(ctx) // blocks until ctx is done
	s.closed.Store(true)
	// Stop the media-group buffer's timers (leak prevention). A partially-
	// accumulated album can NOT be delivered here: teardown is reverse order, so
	// the bus consumer already stopped — the closed latch (and the emit closure's
	// dead ctx) drops the siblings. That's consistent with best-effort shutdown:
	// an undelivered message leaves no persistent trace (the entry WAL only covers
	// a batch already in-flight in a turn, and boot recovery clears those rows
	// silently) — the sender re-sends.
	if s.mgBuffer != nil {
		s.mgBuffer.Close()
	}
	return ctx.Err()
}

// safeEmit delivers ev to the bus, guarding the shutdown race where the Hub may
// close the events channel while an SDK handler goroutine (or a media-group
// AfterFunc) is still in flight: the SDK's Start doesn't join its per-update
// handlers, so a late emit can hit an already-closed channel. Same guard as the
// discord/feishu sources.
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

func (s *Source) onUpdate(ctx context.Context, _ *bot.Bot, update *models.Update) {
	// Inline-button callbacks are deferred (no handler registry yet).
	if update.CallbackQuery != nil {
		return
	}
	m := update.Message
	if m == nil {
		return
	}
	// Channel posts carry no m.From; we don't support them. Drop bot senders.
	if m.From == nil || m.From.IsBot {
		return
	}
	text := m.Text
	if text == "" {
		text = m.Caption // a media message's caption may carry the @mention
	}
	// Drop only a message with nothing to act on — no text AND no media.
	if strings.TrimSpace(text) == "" && !hasMedia(m) {
		return
	}
	threadID := ""
	if m.MessageThreadID != 0 {
		threadID = strconv.Itoa(m.MessageThreadID)
	}
	ct := contract.ChatGroup
	switch string(m.Chat.Type) {
	case "private":
		ct = contract.ChatDM
	case "group", "supergroup":
		ct = contract.ChatGroup
	case "channel":
		// Defensive: real channel POSTS arrive as update.ChannelPost (m==nil) and are
		// dropped upstream. A channel folds to a group scope like any other multi-party
		// chat. A forum-topic thread is NOT its own ChatType — it keeps its id in
		// threadID (set below) for delivery; the chat is still a group scope.
		ct = contract.ChatGroup
	}

	mentionsBot, strippedText := mentionInfo(m, s.meUser, s.meID)

	replyToBot := false
	replyToID, replyToText, replyToUser := "", "", ""
	if m.ReplyToMessage != nil {
		rm := m.ReplyToMessage
		replyToID = strconv.Itoa(rm.ID)
		replyToBot = rm.From != nil && s.meID != 0 && rm.From.ID == s.meID
		// Whether the parent's media lands in THIS turn's attachment list — the same
		// condition the download closure below is built on, kept in one predicate so the
		// quote note and the actual ingestion can't disagree.
		replyToText, replyToUser = describeReplyTo(rm, replyToBot, s.ingestsParentMedia(rm, replyToBot))
	}

	// In groups/topics: only respond if @-mentioned or replying to the bot. EXCEPT a
	// media-group (album) sibling: telegram puts the @mention only on the captioned
	// sibling, so dropping caption-less siblings here would lose all but one photo.
	// Album siblings are deferred to the media-group buffer, which applies the gate
	// at GROUP level (the whole album is accepted iff ANY sibling addressed the bot).
	addressed := mentionsBot || replyToBot

	// A slash command explicitly addressed to ANOTHER bot ("/cmd@otherbot") is never ours.
	// In a group: if the message does NOT also address us, drop it (it's for the other bot).
	// But if it DOES address us — an @us or a reply to us, a strong "talk to me" signal we
	// must not swallow — strip the WHOLE leading foreign command word (not just the
	// "@otherbot" suffix; a bare leftover "/cmd" would be mis-handled by the slash fork as a
	// command for us) and treat the rest as ordinary turn input. (L-D2)
	if ct != contract.ChatDM && commandForOtherBot(m, s.meUser) {
		if !addressed {
			return
		}
		strippedText = stripLeadingForeignCommand(strippedText)
	}

	if ct != contract.ChatDM && !addressed && m.MediaGroupID == "" {
		return
	}
	text = strippedText // bot @mention + any leading foreign command stripped

	ev := contract.MessageEvent{
		Source:      s.name,
		ChatType:    ct,
		ChatID:      strconv.FormatInt(m.Chat.ID, 10),
		ThreadID:    threadID,
		MessageID:   strconv.Itoa(m.ID),
		ReplyToID:   replyToID,
		ReplyToBot:  replyToBot,
		ReplyToText: replyToText,
		ReplyToUser: replyToUser,
		UserID:      strconv.FormatInt(m.From.ID, 10),
		UserName:    m.From.FirstName,
		// IsAdmin is the gate's call (the inbound screen); the source doesn't know it.
		Text:         text,
		ReceivedAt:   time.Now(),
		MediaGroupID: m.MediaGroupID,
	}

	// Capture attachments (this message's + the user-authored replied-to one).
	// Skip media on a bot's own replied-to message (already upstream; re-download
	// piles duplicates) — ingestsParentMedia is that rule, shared with the quote note.
	takeParent := s.ingestsParentMedia(m.ReplyToMessage, replyToBot)
	// Build the download as a CLOSURE, don't run it yet. A non-album event runs it now
	// (it emits straight through — no group-gate drop ahead). An ALBUM sibling DEFERS it to
	// the media-group buffer's flush, which runs it only if the album is accepted — so an
	// un-@'d group album's photos are never fetched (the buffer drops the whole album, and
	// with it the deferred download). (L-D1)
	var download func() []contract.Attachment
	if s.files != nil && (hasMedia(m) || takeParent) {
		subdir := s.resolveSubdir(ev)
		download = func() []contract.Attachment {
			s.typing(ctx, m.Chat.ID, m.MessageThreadID) // about to download — show activity
			atts := s.collectAttachments(ctx, m, false, subdir)
			if takeParent {
				atts = append(atts, s.collectAttachments(ctx, m.ReplyToMessage, true, subdir)...)
			}
			return atts
		}
	}
	if ev.MediaGroupID == "" && download != nil {
		ev.Attachments = append(ev.Attachments, download()...) // non-album: download now
		download = nil
	}
	// Route through the media-group buffer (zero-latency pass-through for non-album
	// events). accepted carries the group-gate decision: a DM, or a group message that
	// @-mentions / replies to the bot. The buffer ORs it across album siblings and drops
	// the whole album on flush if none addressed the bot; download (album only) runs there.
	s.mgBuffer.Submit(ev, ct == contract.ChatDM || addressed, download)
}

// describeReplyTo returns (text, senderName) for a replied-to message.
// ingestsParentMedia reports whether the replied-to message's media will be downloaded
// into THIS turn's attachment list. Two readers: the download closure in onMessage, and
// the quote note (which asserts reachability and must not contradict it). One predicate
// so they cannot drift — the quote is built before the download is even assembled.
//
// Bot-authored parent media is skipped: it came from us, so re-downloading it just
// piles duplicates. That is also the ONE case the incident was in.
func (s *Source) ingestsParentMedia(rm *models.Message, replyToBot bool) bool {
	return s.files != nil && !replyToBot && hasMedia(rm)
}

func describeReplyTo(rm *models.Message, fromBot bool, mediaInAttachments bool) (string, string) {
	txt := rm.Text
	if txt == "" {
		txt = rm.Caption
	}
	// Note FIRST, prose after. ReplyLine cuts the quote at prompt.QuotedMax runes; a
	// note that trails a long caption gets its tail eaten, and a media note without its
	// tail clause is measurably worse than no note at all (see prompt.QuotedMediaNote).
	// Leading with it puts the cut where it belongs — in the prose.
	if media := describeMedia(rm, mediaInAttachments); media != "" {
		if txt != "" {
			txt = media + " " + txt
		} else {
			txt = media
		}
	}
	user := ""
	switch {
	case fromBot:
		user = "you"
	case rm.From != nil:
		user = rm.From.FirstName
	case rm.SenderChat != nil:
		user = rm.SenderChat.Title
	}
	return clip(strings.TrimSpace(txt), 2000), user
}

// describeMedia renders the "the replied-to message carried media" note for the quoted
// reply line. It names the kind and hands it to prompt.QuotedMediaNote, which owns the
// wording every source must share — see there for what the shape has to survive.
func describeMedia(m *models.Message, inAttachments bool) string {
	return prompt.QuotedMediaNote(mediaKind(m), inAttachments)
}

// mediaKind is the bare noun for a message's media ("" when it carries none).
func mediaKind(m *models.Message) string {
	switch {
	case len(m.Photo) > 0:
		return "一张图片"
	case m.Document != nil:
		if m.Document.FileName != "" {
			return "一个文件：" + m.Document.FileName
		}
		return "一个文件"
	case m.Video != nil:
		return "一段视频"
	case m.Voice != nil:
		return "一条语音"
	case m.Audio != nil:
		return "一段音频"
	case m.Sticker != nil:
		return "一个表情"
	case m.Animation != nil:
		return "一张动图"
	case m.Location != nil:
		return "一个位置"
	case m.Contact != nil:
		return "一张名片"
	default:
		return ""
	}
}

// typing shows the "is typing…" chat action once (while downloading attachments,
// before the per-turn sink starts its own typing loop).
func (s *Source) typing(ctx context.Context, chatID int64, threadID int) {
	if s.b == nil {
		return
	}
	p := &bot.SendChatActionParams{ChatID: chatID, Action: models.ChatActionTyping}
	if threadID != 0 {
		p.MessageThreadID = threadID
	}
	_, _ = s.b.SendChatAction(ctx, p)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
