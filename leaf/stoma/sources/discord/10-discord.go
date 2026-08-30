// Package discord is the Discord Source — bob's touchpoint on Discord servers
// (guilds) and DMs. It connects through discordgo's gateway WebSocket long-
// connection (no inbound HTTP route needed), receives message events, and
// replies through the channel REST API rendered via the shared streamsink core.
//
// Scope (text + media + reply): inbound text and attachment messages (guild →
// only when the bot is @'d OR the message replies to one of bob's own messages;
// DM → always), edit-streaming replies (with a typing indicator) split at
// Discord's 2000-character cap, a "👀 seen" reaction on inbound, inbound
// attachment ingest (image/audio/video/document → staged for OCR/transcribe
// preprocess), and reply-quote context lifted from the inline referenced message
// (no extra fetch).
//
// Intent model (mention-only, no privileged MESSAGE CONTENT intent): bob reads
// message content only for DMs, messages that @-mention it, and replies that
// ping it — which is exactly the group gate (a non-@ guild message is dropped
// anyway). Running without the privileged intent avoids the developer-portal
// toggle and the 100+-server verification requirement.
//
// REBUILD scope: receive + send + streamsink rendering + reactions + attachments
// (files store). Access control (allow/deny/admin) is the gate's job
// (the inbound screen — screened by default). Inline-button callbacks and message-index
// reply-routing are deferred.
//
// Boundary: discordgo (github.com/bwmarrin/discordgo) appears ONLY in
// sources/discord/*.go. contract/ is untouched.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/files"

	"github.com/bwmarrin/discordgo"
)

// discordMaxChars is the per-message hard cap Discord enforces, counted in
// Unicode code points. The streamsink core splits a longer reply into a chain
// of sends at this boundary (see WireLen/MaxChars on discordWire).
const discordMaxChars = 2000

// Source implements contract.Source for Discord. One instance per bot (one app /
// token); a single bob process can run several (distinct tokens, each its own
// uniquely-named Source).
type Source struct {
	// name is the unique source name ("discord", "discord2", …). It flows into
	// session keys, source health, and the rejected log, so each bot routes in
	// its own namespace.
	name string

	token string
	dg    *discordgo.Session

	// dmTypeCache memoises the 1:1-vs-group-DM classification per channel id
	// (channelID → isGroupDM bool). A channel's type is immutable, so without
	// this classifyChannel does a blocking, context-less REST fetch on EVERY DM
	// message when the State cache misses (DM channels often aren't in State).
	// Capped at dmTypeCacheMax (drop-whole-map on overflow) — at most a re-fetch
	// on the rare overflow.
	dmTypeMu    sync.Mutex
	dmTypeCache map[string]bool

	// botUserID is THIS bot's own user id, fetched once at Run start (REST
	// User("@me")) BEFORE the gateway opens, so onMessageCreate reads it race-
	// free. Used by mentionsBot to answer an @-mention only when it targets THIS
	// bot, and to drop the bot's own echo. Empty when the fetch failed →
	// mentionsBot degrades to "any mention = me" (single-bot behaviour).
	botUserID string

	// files is the inbound attachment store (nil → don't capture). maxAttBytes
	// is the per-file cap.
	files       *files.Store
	maxAttBytes int64

	out chan<- contract.MessageEvent

	connected atomic.Bool // gateway link up (Connect/Disconnect/Resumed)
	closed    atomic.Bool // Run teardown began
}

// New builds a Discord source named `name` ("discord" for the primary bot). The
// bot token is read from the env var named tokenEnv (default DISCORD_BOT_TOKEN);
// it is required. fileStore may be nil to disable inbound attachment capture.
// Access control (allow/deny/admin) is NOT configured here — the gate handles
// it (the inbound screen).
func New(name, tokenEnv string, maxAttBytes int64, fileStore *files.Store) (*Source, error) {
	if name == "" {
		name = "discord"
	}
	if tokenEnv == "" {
		tokenEnv = "DISCORD_BOT_TOKEN"
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil, fmt.Errorf("discord: env %s is unset (bot token required for source %q)", tokenEnv, name)
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discord: new session for %q: %w", name, err)
	}
	// Mention-only intents: Guilds (so the State cache holds channels for group-DM
	// detection), GuildMessages + DirectMessages (the message events). NOT
	// IntentMessageContent — the privileged intent is deliberately omitted;
	// content arrives only for DMs / @-mentions / replies, which is the gate
	// model anyway (see the package doc).
	dg.Identify.Intents = discordgo.IntentGuilds | discordgo.IntentGuildMessages | discordgo.IntentDirectMessages
	// Surface 429s to the streamsink core instead of letting discordgo block-and-
	// retry internally: the core clamps the wait, retries once, and degrades an
	// edit-stream to block mode on a sustained throttle (see discordWire.RateLimited).
	// discordgo still proactively paces via its per-bucket limiter regardless.
	dg.ShouldRetryOnRateLimit = false

	return &Source{
		name:        name,
		token:       token,
		dg:          dg,
		files:       fileStore,
		maxAttBytes: maxAttBytes,
	}, nil
}

func (s *Source) Name() string { return s.name }

// Caps: screened by default (not Trusted) — sender screening (deny / redeem / allow) runs in the hub's
// inbound screen. ReplyTo — the anchor send threads the reply onto the triggering
// message via a MessageReference (see discordWire.Send / createMessage), matching
// telegram/feishu. RedeemConfirmAll — a guild redeem (wire-code / claim-code) must
// @bob (mention-only intent), so confirm the bind in the group too; without it the
// deliverRedeem group-suppression swallows the "✅ wired" reply (see flow/inbound).
// Edit-streaming / typing are declared separately via discordWire.Caps().
func (s *Source) Caps() contract.Caps {
	return contract.Caps{ReplyTo: true, RedeemConfirmAll: true} // screened by default (not Trusted)
}

// HealthCheck reports whether the gateway link is currently up. discordgo
// manages its own reconnect; this surfaces the last known state to the Hub's
// periodic probe.
func (s *Source) HealthCheck(ctx context.Context) error {
	if !s.connected.Load() {
		return fmt.Errorf("discord: gateway not connected")
	}
	return nil
}

// Run opens the gateway long-connection and pumps inbound events until ctx is
// cancelled. discordgo.Open starts its own reconnecting goroutines and returns;
// on ctx cancel we Close() the session and return.
//
// Serially re-invocable — the bus retries a boot-window exit (leaf/stoma
// runSource) on this SAME instance, so the closed latch is reset here and
// s.out is rebound each run. The unlocked writes are safe because retries are
// serial and a failed run has no handler in flight (every pre-serve exit is an
// Open failure, and the handler removal below runs before Run returns).
func (s *Source) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	// Reset the closed latch like telegram/feishu. Today no failed-run exit path
	// latches it (only the post-serve teardown does, and the bus never retries
	// after ctx cancel), but an explicit reset keeps the "Run entry ⇒ serving"
	// invariant local instead of relying on that reachability argument.
	s.closed.Store(false)
	s.out = out

	// Best-effort: learn THIS bot's own user id BEFORE opening the gateway so
	// onMessageCreate (a handler goroutine) reads it race-free. A failure is
	// non-fatal — mentionsBot degrades to single-bot "any mention = me".
	if u, err := s.dg.User("@me", discordgo.WithContext(ctx)); err != nil {
		slog.Warn("discord: could not fetch bot user id — @-mention matching falls back to any-mention",
			"source", s.name, "err", s.redactErr(err))
	} else if u != nil {
		s.botUserID = u.ID
	}

	// Handlers are removed when Run exits: the bus retries a boot-window exit
	// (leaf/stoma runSource) on this SAME persistent session, and discordgo
	// APPENDS handlers — without the removal, N failed attempts would leave N+1
	// MessageCreate handlers and every inbound message would fan out N+1 times.
	removers := []func(){
		s.dg.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) {
			s.onMessageCreate(ctx, m)
		}),
		s.dg.AddHandler(func(_ *discordgo.Session, _ *discordgo.Connect) {
			s.connected.Store(true)
			slog.Info("discord: gateway connected", "source", s.name)
		}),
		s.dg.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) {
			s.connected.Store(true)
			slog.Info("discord: gateway resumed", "source", s.name)
		}),
		s.dg.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
			s.connected.Store(false)
			slog.Warn("discord: gateway disconnected", "source", s.name)
		}),
	}
	defer func() {
		for _, remove := range removers {
			remove()
		}
	}()

	if err := s.dg.Open(); err != nil {
		// A connect-time failure (bad token / intent denied) surfaces here; the
		// gateway's source-supervision loop decides whether to retry.
		return fmt.Errorf("discord: gateway open: %w", s.redactErr(err))
	}
	s.connected.Store(true)

	<-ctx.Done()
	s.closed.Store(true)
	s.connected.Store(false)
	_ = s.dg.Close()
	return ctx.Err()
}

// selfID returns THIS bot's own user id for @-mention matching/stripping. It
// prefers the id fetched at Run start (botUserID) but falls back to the gateway
// State's user, which Ready populates before any onMessageCreate fires — so even
// when the Run-start REST fetch failed we still strip the bot's own mention
// precisely instead of degrading to "any mention = me" + leaking @BotName.
func (s *Source) selfID() string {
	if s.botUserID != "" {
		return s.botUserID
	}
	if st := s.dg.State; st != nil && st.User != nil {
		return st.User.ID
	}
	return ""
}

// onMessageCreate turns an inbound Discord message into a contract.MessageEvent
// and emits it. Handles text plus image/audio/video/document attachments:
//
//   - bot / webhook senders and the bot's own echo are dropped (kills self-echo
//     and bot-to-bot loops).
//   - in a guild, the message is ignored unless the bot was @'d OR it replies to
//     one of bob's own messages — this also gates attachment-only noise.
//   - a thread is its own channel, so a thread message keys to (and is replied
//     to in) the thread itself — not folded into its parent.
//   - an event with neither text nor attachments is dropped.
func (s *Source) onMessageCreate(ctx context.Context, mc *discordgo.MessageCreate) {
	if mc == nil || mc.Message == nil {
		return
	}
	m := mc.Message

	// Only human messages. Drop bots, webhooks, and our own echo — bob posts as a bot
	// account (Author.Bot is true), so this single check also covers our own messages.
	if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}
	userID := m.Author.ID
	if userID == "" {
		slog.Warn("discord: inbound message without author id — dropping")
		return
	}
	if m.ChannelID == "" {
		slog.Warn("discord: inbound message without channel id — dropping")
		return
	}

	// chatID is the channel the message arrived in — a Discord thread is its own
	// channel (its own scope), so a thread is NOT folded into its parent.
	chatType, chatID := s.classifyChannel(m)

	// Sender gating (denylist > bare-code redeem > allowlist) is the hub's
	// inbound screen (screened by default). The mention-stripped ev.Text below is what
	// reaches the hub's exact-match redeem — in a guild (mention-only intent)
	// the user MUST @bob, so the raw m.Content is "<@botID> CODE".

	// Reply linkage: a Discord reply carries the parent id inline AND inlines the
	// referenced message (author included — see the reply-quote lift below), so
	// reply-to-bot is detectable here WITHOUT the message_index: compare the referenced
	// author to our own id. This drives SESSION ROUTING — the resolver reconnects to the
	// replied-to message's session (reply-to-bob → continue) instead of @=new forking a
	// fresh, context-less one. (bob's outbound ids are indexed via the flows' OnAnchor →
	// RecordSent, so SessionForMessage(scope, replyToID) resolves.)
	replyToID := replyParentID(m)
	selfID := s.selfID()
	mentioned := mentionsBot(m.Mentions, selfID)
	replyToBot := replyToBotMessage(m, selfID)
	// The GROUP-NOISE gate stays @-only (pass false, NOT replyToBot): without the
	// MessageContent privileged intent a non-@ reply arrives with empty content and is
	// dropped at the text==""&&atts==0 check below anyway, so admitting it here gains
	// nothing. A reply that ALSO @-mentions passes on `mentioned`, then routes by
	// replyToBot above — which is the supported "continue an old topic" path.
	if shouldDropAsGroupNoise(chatType, mentioned, false) {
		return // group noise: only respond when @'d
	}

	// Reply quote: Discord inlines the referenced message (author + content), so
	// no extra fetch is needed (unlike feishu's Message.Get). Gate on
	// replyParentID(m) != "" so this fires ONLY for a genuine reply (Default
	// ref type) — a FORWARD also carries m.ReferencedMessage, and stamping it as
	// a reply-quote would falsely tell the agent the user "replied to" content
	// they merely forwarded.
	var replyToText, replyToUser string
	if m.ReferencedMessage != nil && replyToID != "" {
		replyToText = m.ReferencedMessage.Content
		if m.ReferencedMessage.Author != nil {
			replyToUser = displayName(m.ReferencedMessage.Author)
		}
	}

	// Build the event up-front (without Text/Attachments) so collectAttachments
	// can resolve the staging subdir from it before downloading.
	ev := contract.MessageEvent{
		Source:      s.name,
		ChatType:    chatType,
		ChatID:      chatID,
		MessageID:   m.ID,
		ReplyToID:   replyToID,
		ReplyToBot:  replyToBot,
		ReplyToText: replyToText,
		ReplyToUser: replyToUser,
		UserID:      userID,
		// Discord user ids are globally stable (no per-app split like feishu's
		// open_id/union_id), so UserID is already the cross-app identity and
		// StableUID is left empty.
		UserName: displayName(m.Author),
		// IsAdmin is the gate's call (the inbound screen); the source doesn't know it.
		ReceivedAt: time.Now(),
	}

	text := stripMentions(m.Content, m.Mentions, m.MentionRoles, selfID)
	atts := s.collectAttachments(ctx, m, ev)

	// Drop only when there is nothing to act on. An attachment-only message MUST
	// still emit; a bare @bob with no text and no media is pure noise.
	if text == "" && len(atts) == 0 {
		return
	}
	ev.Text = text
	ev.Attachments = atts
	s.safeEmit(ctx, ev)
}

// classifyChannel maps an inbound message to (chatType, chatID). chatID is the
// channel the message arrived in, used directly as the session-keying chat id —
// a Discord thread is a distinct channel (its own conversation), so it keys (and
// later delivers) to its own id, NOT folded into its parent.
//
//   - No guild → a 1:1 DM, or a group DM (multi-person), treated as a group scope.
//   - A guild channel or thread → its own group scope.
func (s *Source) classifyChannel(m *discordgo.Message) (contract.ChatType, string) {
	if m.GuildID == "" {
		// Guild-less. A CONFIRMED 1:1 DM is ungated (ChatDM); a group DM OR an UNKNOWN
		// channel (the type lookup failed transiently) is gated as a group — fail CLOSED,
		// so a missing lookup never makes bob answer an un-@'d group-DM message. The cost
		// is that a 1:1 DM whose lookup transiently fails is gated for that one message,
		// self-correcting once the type caches. (L-D4)
		if isGroup, known := s.isGroupDM(m.ChannelID); known && !isGroup {
			return contract.ChatDM, m.ChannelID
		}
		return contract.ChatGroup, m.ChannelID
	}
	return contract.ChatGroup, m.ChannelID
}

// isGroupDM reports whether a guild-less channel is a multi-person group DM (isGroup)
// and whether the type is KNOWN (known=false on a transient lookup failure). It
// memoises the immutable result so the per-message classify path doesn't re-fetch.
// Only a CONFIRMED lookup is cached — a failed (nil) lookup might be transient, so it
// stays uncached (known=false) and is retried on the next message rather than poisoning
// the cache. The caller fails CLOSED on known=false (gates an unknown guild-less channel
// as a group). (L-D4)
func (s *Source) isGroupDM(channelID string) (isGroup, known bool) {
	s.dmTypeMu.Lock()
	if v, ok := s.dmTypeCache[channelID]; ok {
		s.dmTypeMu.Unlock()
		return v, true
	}
	s.dmTypeMu.Unlock()

	ch := s.lookupChannel(channelID)
	if ch == nil {
		return false, false // transient lookup failure — unknown
	}
	isGroup = ch.Type == discordgo.ChannelTypeGroupDM

	s.dmTypeMu.Lock()
	if s.dmTypeCache == nil {
		s.dmTypeCache = make(map[string]bool)
	}
	if len(s.dmTypeCache) >= dmTypeCacheMax {
		// Simple bound: drop the whole map rather than pull an LRU dep. Rare for
		// a single-bot deploy; the cost is at most a re-fetch.
		s.dmTypeCache = make(map[string]bool)
	}
	s.dmTypeCache[channelID] = isGroup
	s.dmTypeMu.Unlock()
	return isGroup, true
}

// dmTypeCacheMax bounds dmTypeCache. classifyChannel writes a fresh entry
// per distinct DM channel id, so an unbounded map would let a stranger-DM flood
// grow memory. 512 is ample for a single-bot deploy; on overflow the whole map
// is dropped (no LRU dep).
const dmTypeCacheMax = 512

// lookupChannel resolves a channel from the discordgo State cache (populated by
// the Guilds intent) and falls back to a REST fetch. Returns nil on any failure
// (the caller treats nil as UNKNOWN and fails closed — the channel is gated as a
// group; see isGroupDM / L-D4). Used only to tell a 1:1 DM from a
// group DM (a thread needs no lookup — it is its own scope).
func (s *Source) lookupChannel(channelID string) *discordgo.Channel {
	if channelID == "" || s.dg == nil {
		return nil
	}
	if ch, err := s.dg.State.Channel(channelID); err == nil && ch != nil {
		return ch
	}
	ch, err := s.dg.Channel(channelID)
	if err != nil {
		slog.Debug("discord: channel lookup failed", "channel_id", channelID, "err", s.redactErr(err))
		return nil
	}
	return ch
}

// safeEmit delivers ev to the gateway, guarding the shutdown race where the Hub
// may close the events channel while a handler is still in flight.
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

// displayName returns a user's best human-readable name (global display name,
// falling back to the username).
func displayName(u *discordgo.User) string {
	if u == nil {
		return ""
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

var _ contract.Source = (*Source)(nil)
