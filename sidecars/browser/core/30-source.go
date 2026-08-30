package core

import (
	"context"
	"errors"
	"fmt"
)

// SourceNameInternal is the canonical Name() of the in-process
// "internal" Source (agora's virtual bridge). It lives in core so every
// layer can recognise "this event came from agora, not a real chat"
// without importing the concrete sources/bridge package — keeping the
// agent-turn layer free of a downward dependency on a Source impl.
const SourceNameInternal = "internal"

// ErrBridgeBusy is returned by the internal Source's Emit when its
// in-memory buffer is full. Tools (send_message / report) surface it
// through prompt.Err so the model sees a typed failure and can back off
// / retry rather than silently dropping the message. Lives in core so
// callers can errors.Is against it without importing sources/bridge.
var ErrBridgeBusy = errors.New("bridge: buffer full")

// Caps declares what a Source can do.
type Caps struct {
	ReplyTo bool // supports replying to a specific message
	// Gated opts this Source's inbound events into the hub's centralized
	// sender screen (denylist > bare-code redeem > allowlist, docs/accounts.md
	// §6.1). Chat sources (telegram / discord / feishu) set it. Sources that
	// must screen inside their own protocol phase keep it false and call the
	// shared gate themselves (email: IMAP stage-1 header PEEK, so an
	// unauthorized sender's body is never fetched). Sources with no sender
	// gating at all (local, bridge, wechat v1 filehelper) leave it false.
	Gated bool
	// RedeemConfirmAll: when a bare code is redeemed in a NON-DM chat, send
	// the confirmation reply there too (telegram: the wire-code flow pastes
	// the code into the group being wired and needs visible feedback there).
	// Default false = confirm only in DMs — broadcasting "✅ bound" to a
	// group leaks one user's bind and is noise (discord / feishu behavior).
	// Only consulted for Gated sources.
	RedeemConfirmAll bool
}

// Source is a message channel: the local terminal, Telegram, etc.
// Inbound events are pushed onto the channel given to Run; replies are rendered
// through a Sink obtained from NewSink. A Source must be safe for concurrent use.
type Source interface {
	Name() string
	Caps() Caps
	// Run blocks, delivering inbound events to out, until ctx is cancelled.
	Run(ctx context.Context, out chan<- MessageEvent) error
	// NewSink returns a sink for rendering one reply to the given target, on
	// behalf of session sessionScope. The sink's lifetime ends when Finish is
	// called (or ctx is cancelled). A Source that can recognise its own messages
	// later (e.g. for reply-to-bot routing) records the message(s) it sends here
	// against sessionScope; a Source that can't just ignores sessionScope.
	//
	// prefs come from the per-scope /trace + /stream toggles (resolved by the
	// hub via the session store). Sources that can honour Stream=false buffer
	// until Finish; Trace=false drops TraceDelta calls. Sources that can't
	// honour a pref (e.g. an SMS gateway with no streaming) treat it as
	// always-Stream=false and that's fine — Finish still gets the full content.
	//
	// sid is the session id this sink's turn belongs to (may be "" for
	// non-turn sinks: slash-command replies, restart-recovery notices).
	// Most sources ignore it; the email source uses it to coalesce a
	// session's replies into one mail (per-sid batch window).
	NewSink(ctx context.Context, t Target, sessionScope string, sid string, prefs SinkPrefs) Sink
	// SendButtons sends an independent message containing text + an inline-button
	// row group. Each button's Callback id was minted by callbacks.Manager.Register
	// and is plumbed back through CallbackResolver.Resolve when the user clicks.
	//
	// Layout: 2 buttons per row, wrapping (3 buttons → [2,1], 4 → [2,2], 5 → [2,2,1]).
	// Every Source must implement this — sources without native inline buttons
	// degrade gracefully (local-cli pops a TUI menu and resolves synchronously).
	//
	// Returns the platform's message id (for later edit/delete) or "" when the
	// concept doesn't apply (local-cli). Errors are wire-level; missing-handler
	// (callback id never registered) is the resolver's concern.
	SendButtons(ctx context.Context, t Target, text string, buttons []Button) (msgID string, err error)
	// Send posts a one-off independent text message to t (no buttons,
	// no streaming). Used for asynchronous feedback messages — e.g.,
	// after a button click resolves, the handler tells the chat what
	// happened.
	//
	// Implementations MAY be async: the returned error indicates
	// REJECTION (queue full / source not connected / ctx already
	// done), NOT delivery failure. Once accepted (nil return),
	// delivery success/failure is the source's responsibility to log;
	// callers MUST NOT rely on Send's return as a delivery
	// confirmation. Telegram queues internally with 429 retry-after
	// handling; local-cli prints synchronously.
	Send(ctx context.Context, t Target, text string) error
	// SendFile posts the file at `path` as a document/attachment to t, with an
	// optional caption. Like Send, the returned error means REJECTION (queue
	// full / source not connected / ctx done), NOT a delivery confirmation.
	// The displayed filename is the basename of path. Every Source implements
	// this — degrade gracefully where the platform has no real file channel.
	SendFile(ctx context.Context, t Target, path, caption string) error
	// IsAdmin returns whether userID is an admin in chatID per the
	// source's per-platform admin config (e.g., $BOB_HOME/sources/
	// telegram.yaml). Used by callback handlers that need to enforce
	// "only admin or original requester can resolve" — they don't have
	// the message-event-time IsAdmin flag because the click can land
	// long after the original message.
	IsAdmin(chatID, userID string) bool

	// Limits returns the source's wire-format constraints. Gateway code
	// SHOULD ask Limits() instead of hardcoding numbers from a specific
	// platform's protocol — the middle layer specifies "single message
	// max", the source answers with what its wire format actually
	// allows. Mandatory: every Source impl must return non-zero values
	// so callers can rely on them (no zero-checks scattered everywhere).
	Limits() Limits

	// HealthCheck performs a cheap upstream-liveness probe (e.g.
	// Telegram getMe, no-op for in-process sources). MUST complete in
	// under a few seconds — the Hub calls this on a periodic ticker
	// (gateway.source_health.interval_sec, default 60s) and uses a 5s
	// context timeout. Return nil iff the source is currently able to
	// receive / send messages.
	//
	// Failures after gateway.source_health.fail_threshold consecutive
	// errors are funnelled through Hub.AdminNotify (kind=admin_notify
	// in logs); recovery transitions also notify. Sources whose check
	// is structurally trivial (e.g. local-cli: if bob's running, the
	// terminal is reachable) should `return nil`.
	HealthCheck(ctx context.Context) error
}

// Limits is the per-source capability envelope for sized values that
// vary across chat platforms. Every Source returns these from Limits();
// gateway / callbacks / slash handlers consult them instead of
// hardcoding Telegram's specific numbers. Adding a new constraint? Put
// it here, default it in every Source impl, then read it where you
// need it. Never put `"telegram"` literals in middle-layer code.
//
// Mandatory: every field MUST be > 0. Call Validate() at Source
// registration so a buggy Source impl is caught loudly instead of
// silently poisoning gateway aggregates (a single zero in
// aggregateLimits' min-reduce would collapse the result).
type Limits struct {
	// MaxMessageBytes — soft cap for a single sent message before the
	// source needs to split / chunk. Telegram: 4000 chars (conservative
	// vs the 4096 UTF-16-code-unit hard cap; 96 unit headroom covers
	// emoji that count as 2 units). Local-cli: very large (no cap).
	//
	// Naming note: "Bytes" is historical — different sources measure
	// differently (Telegram: UTF-16 code units; local: runes). Treat
	// it as "single-message text budget" + consult MaxMessageChars()
	// semantics in each Source's docstring before assuming UTF-8 bytes.
	MaxMessageBytes int

	// MaxCommandNameBytes — slash command name length the source's menu
	// API accepts. Telegram BotCommand.command: 32. Local-cli: relaxed
	// (no platform menu; ad-hoc input).
	MaxCommandNameBytes int
}

// MessageReactor is an OPTIONAL capability for sources that can react
// to an inbound user message with an emoji ("seen" indicator etc).
// Callers type-assert against this interface at the dispatch entry —
// sources that don't implement it are skipped silently. Telegram is
// the only source that implements it today; wechat / local / bridge
// have no chat-side reaction concept and intentionally omit it.
//
// chatID / messageID are the source-native string ids carried on
// MessageEvent (the impl parses them back to whatever native type its
// platform API needs — Telegram parses chatID → int64, messageID →
// int). emoji is a platform-accepted reaction emoji (Telegram's
// allowed list is documented at core.telegram.org/bots/api#reactiontypeemoji;
// "👀" is on it).
//
// Best-effort by contract: a failure (parse / API / permission /
// rate-limit) is returned to the caller, but the caller is expected
// to log + drop — reaction is a UX nicety and MUST NOT block reply
// delivery.
type MessageReactor interface {
	ReactToMessage(ctx context.Context, chatID, messageID, emoji string) error
}

// Validate returns nil iff every field is > 0 (the contract above).
// Gateway-side aggregation (min-reduce) requires non-zero baselines or
// the result collapses to zero on a buggy impl; calling this at Source
// registration catches the bug loudly instead of silently breaking
// downstream output sizing.
func (l Limits) Validate() error {
	if l.MaxMessageBytes <= 0 {
		return fmt.Errorf("Source.Limits.MaxMessageBytes must be > 0, got %d", l.MaxMessageBytes)
	}
	if l.MaxCommandNameBytes <= 0 {
		return fmt.Errorf("Source.Limits.MaxCommandNameBytes must be > 0, got %d", l.MaxCommandNameBytes)
	}
	return nil
}
