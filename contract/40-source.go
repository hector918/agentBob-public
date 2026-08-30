package contract

import (
	"context"
	"strings"
)

// SourceNameInternal is the canonical Name() of the in-process "internal"
// Source (the agora virtual bridge). It lives in contract so every layer can
// recognise "this event came from an internal tool, not a real chat" without
// importing the concrete bridge source.
const SourceNameInternal = "internal"

// Caps declares what a Source can do.
type Caps struct {
	ReplyTo bool // supports replying to a specific message
	// Trusted EXEMPTS this Source's inbound events from the inbound flow's
	// centralized sender screen (denylist > bare-code redeem > allowlist). It is
	// OPT-OUT and fail-closed by design: the zero value (false) means SCREENED, so a
	// source that forgets to declare anything is gated, not silently open — a screening
	// miss can't be a careless default. Only genuinely-trusted, non-external sources
	// set it true: local (the terminal/CLI user) and bridge (internal agora traffic).
	// Every external chat source (telegram / discord / feishu / email) leaves it false
	// and is screened.
	//
	// ⚠️ A Trusted source ALSO bypasses the gate's ev.IsAdmin re-stamp: the gate's only
	// admin authority is the per-source allowlist policy (admins.yaml), which a Trusted
	// source has no entry in — so the gate cannot re-derive admin for it and leaves
	// ev.IsAdmin exactly as the source set it. Therefore a Trusted source OWNS its
	// ev.IsAdmin and MUST stamp it deliberately: false unless the channel is an
	// operator-authenticated one (local = the terminal/CLI user, webui = the unlocked
	// admin console — both stamp true; a screened source MUST leave it false and let the
	// gate stamp it). ev.IsAdmin==true grants FULL operator control (every AdminOnly
	// slash: /gate allow, /agora, /webui, /warrant, /model, /accounts), so a Trusted
	// source that wrongly stamps true hands that to its senders with NO central check.
	// There is no enforcement of this — it is a trust-boundary contract the source author
	// is responsible for. (A forgotten stamp is the safe failure: false → AdminOnly denied.)
	Trusted bool
	// RedeemConfirmAll: when a bare code is redeemed in a NON-DM chat, send the
	// confirmation reply there too (telegram: the wire-code flow pastes the code
	// into the group being wired and needs visible feedback there). Default
	// false = confirm only in DMs. Only consulted for screened (non-Trusted) sources.
	RedeemConfirmAll bool
	// RequiredModelTags HARD-constrains which model serves a turn from this source:
	// the flow maps it to ModelRequest.Requires, so the pool only considers entries
	// carrying ALL these tags (picker then chooses among the qualifying ones); none
	// qualify → the turn fails rather than serving a generic model (UNLESS an admin
	// tag-fallback rule remaps these tags — see the model pool's fallback chain).
	// Empty → no constraint. Source-level config (e.g. email's required_model_tags)
	// read by the flow at turn time — NOT message data, so it rides Caps, not the event.
	RequiredModelTags []string
}

// Target identifies where a reply goes. Paired with MessageEvent (the inbound
// half): a source receives a MessageEvent and renders replies to a Target.
type Target struct {
	Source    string // "local" | "telegram" | ...
	ChatID    string
	ThreadID  string // forum topic / thread id (optional)
	ReplyToID string // reply to a specific message id (optional; groups)
}

// ReplyTarget returns a Target that replies back to this event's chat/thread.
func (e MessageEvent) ReplyTarget() Target {
	return Target{Source: e.Source, ChatID: e.ChatID, ThreadID: e.ThreadID}
}

// ScopeFor is the canonical session-routing key for a chat coordinate — the ONE
// grammar that maps (source, chat type, chat id, thread id) → a scope string.
// Both the session resolver (inbound: which session) and agora's inbox router
// (which inbox an inbox_source binds) compute scopes through it, so the two can
// never drift (a per-source duplicate caused the email-forwarded-alias miss). It
// lives in contract — below every consumer (leaf/session, leaf/agora, flow) — as
// a pure projection of MessageEvent's own routing fields (mirrors ReplyTarget).
//
// Grammar:
//   - internal source → the chat id verbatim (the dispatch target scope itself).
//   - DM → "<source>:dm:<chatID>", with "#<threadID>" appended when a thread
//     splits the DM into sub-scopes (email forwarded-alias).
//   - group / channel / topic → "<source>:group:<chatID>" (topics flattened).
func ScopeFor(source string, chatType ChatType, chatID, threadID string) string {
	if source == SourceNameInternal {
		return chatID // internal dispatch: ChatID IS the target scope verbatim
	}
	if chatType == ChatDM {
		key := source + ":dm:" + chatID
		if threadID != "" {
			key += "#" + threadID
		}
		return key
	}
	return source + ":group:" + chatID
}

// RoutingScope returns this event's canonical session-routing scope (see ScopeFor).
func (e MessageEvent) RoutingScope() string {
	return ScopeFor(e.Source, e.ChatType, e.ChatID, e.ThreadID)
}

// TargetForScope is the partial inverse of ScopeFor: it recovers the chat
// coordinate a scope was built from. It lives HERE, beside the grammar's write
// side, for the same reason SplitMemberSubScope does — a second parser elsewhere
// is exactly how the two sides drift apart.
//
// For the rare caller that must deliver to a chat with no live turn to inherit a
// Sink from: the image_create tool records a scope with each in-flight job and, after a
// restart, resolves it back to a Target to finish the delivery (docs/image-create-tool.md §9.5).
//
// PARTIAL by construction, because ScopeFor is lossy:
//   - group / channel / topic scopes flatten the thread, so a forum-topic chat
//     comes back as its plain group (right chat, main thread — never the wrong chat);
//   - ReplyToID was never in the scope, and an out-of-turn send is a fresh
//     message rather than a reply, so it stays empty;
//   - an internal-dispatch scope is the target scope verbatim with no source
//     prefix — not a user chat, and refused rather than guessed at;
//   - a virtual-group member sub-scope resolves to its base group chat.
//
// ok=false means "this scope does not name a deliverable chat". Callers must not
// fall back to a guess.
func TargetForScope(scope string) (Target, bool) {
	if base, _, isMember := SplitMemberSubScope(scope); isMember {
		scope = base
	}
	source, rest, found := strings.Cut(scope, ":")
	if !found || source == "" || source == SourceNameInternal {
		return Target{}, false
	}
	switch {
	case strings.HasPrefix(rest, "dm:"):
		chatID, threadID, _ := strings.Cut(strings.TrimPrefix(rest, "dm:"), "#")
		if chatID == "" {
			return Target{}, false
		}
		return Target{Source: source, ChatID: chatID, ThreadID: threadID}, true
	case strings.HasPrefix(rest, "group:"):
		chatID := strings.TrimPrefix(rest, "group:")
		if chatID == "" {
			return Target{}, false
		}
		return Target{Source: source, ChatID: chatID}, true
	}
	return Target{}, false
}

// SplitMemberSubScope splits a virtual-group member sub-scope "<group>#<member>"
// into its GROUP base scope and member name. ok is false for any other scope: no
// "#" suffix, or the base is not a group scope — a DM's "#<threadID>" suffix (the
// ScopeFor email forwarded-alias grammar) is NOT a member sub-scope. The ONE
// parser for the grammar (the write side is Agora.RouteGroupScope's base+"#"+name):
// the inbox router, the session arbiter and the flows all split through it, so a
// grammar change can never drift between them.
func SplitMemberSubScope(scope string) (base, member string, ok bool) {
	if i := strings.IndexByte(scope, '#'); i >= 0 && strings.Contains(scope[:i], ":group:") {
		return scope[:i], scope[i+1:], true
	}
	return "", "", false
}

// Source is a message channel: the local terminal, Telegram, etc. Inbound
// events are pushed onto the channel given to Run; replies are rendered through
// a Sink obtained from NewSink. A Source must be safe for concurrent use.
type Source interface {
	Name() string
	Caps() Caps
	// Run blocks, delivering inbound events to out, until ctx is cancelled.
	Run(ctx context.Context, out chan<- MessageEvent) error
	// NewSink returns a sink for rendering one reply to the given target, on
	// behalf of session sessionScope (a Source that can recognise its own
	// messages later records them against it; one that can't ignores it). prefs
	// are the per-scope /trace + /stream toggles. sid is the session id the
	// turn belongs to (may be "" for non-turn sinks); most sources ignore it.
	NewSink(ctx context.Context, t Target, sessionScope, sid string, prefs SinkPrefs) Sink
	// SendButtons sends an independent message with an inline-button row group.
	// Each button's Callback id is resolved when the user clicks. Sources
	// without native inline buttons degrade gracefully (local-cli pops a TUI
	// menu). Returns the platform's message id (or "" when N/A).
	SendButtons(ctx context.Context, t Target, text string, buttons []Button) (msgID string, err error)
	// Send posts a one-off independent text message (no buttons, no streaming),
	// for asynchronous feedback. MAY be async: the returned error indicates
	// REJECTION (queue full / not connected / ctx done), NOT delivery failure.
	Send(ctx context.Context, t Target, text string) error
	// SendFile posts the file at path as a document/attachment, with an optional
	// caption. Like Send, the error means REJECTION, not a delivery
	// confirmation. The displayed filename is the basename of path.
	SendFile(ctx context.Context, t Target, path, caption string) error
	// HealthCheck performs a cheap upstream-liveness probe (Telegram getMe, no-op
	// for in-process sources). MUST complete in a few seconds — the bus probes
	// it on a periodic ticker with a short context timeout. It MUST honor ctx
	// (return promptly on cancellation): at shutdown the bus stops waiting on a
	// still-running probe, so an implementation that ignores ctx is left running
	// on an orphaned goroutine instead of merely delaying shutdown. Return nil
	// iff the source can currently receive / send.
	HealthCheck(ctx context.Context) error
}

// SelfMentioner is an OPTIONAL Source capability: the human-typeable handle that
// addresses THIS bot (e.g. telegram "@botname"). A flow that must tell a human
// WHICH bot to address — a multi-bot group's "pick a member" prompt, where the
// member token must be prefixed by the right bot's @handle — type-asserts it.
// Sources whose self-mention isn't a copyable string (feishu/discord structured
// mentions) simply don't implement it; the caller falls back to no prefix.
type SelfMentioner interface {
	SelfMention() string // e.g. "@binserv00Bot"; "" when unknown / not applicable
}

// PhotoSender is an OPTIONAL Source capability: deliver a file the way the
// platform shows PICTURES rather than the way it shows attachments.
//
// It exists because the two are different products, not two renderings of one.
// Telegram's sendPhoto puts the image inline in the conversation, and pays for it
// by re-encoding server-side to JPEG — the original bytes, the alpha channel and
// the filename do not survive. sendDocument keeps all three and shows a file row.
//
// So the choice belongs to the CALLER, and it is a choice about intent:
//
//   - a picture bob just produced for someone to look at → SendPhoto; the
//     original is still in the space if anyone wants it;
//   - a file the user asked to be given (deliver_file) → SendFile, always. Silent
//     re-encoding of something someone asked for by name is data loss they never
//     agreed to.
//
// Sources without a distinct picture primitive simply don't implement it, and
// DeliverPicture falls back to SendFile — which on those platforms is what a
// picture looked like anyway (Discord renders image attachments inline).
//
// An implementation MUST fall back to document delivery itself when the file
// cannot go as a photo (over the platform's photo limits, not a decodable
// image): a picture that arrives as a file beats one that does not arrive.
type PhotoSender interface {
	SendPhoto(ctx context.Context, t Target, path, caption string) error
}

// DeliverPicture sends path as a picture through src when it can do that, and as
// an ordinary attachment otherwise. Callers use this rather than type-asserting
// so the fallback rule lives in one place.
func DeliverPicture(ctx context.Context, src Source, t Target, path, caption string) error {
	if ps, ok := src.(PhotoSender); ok {
		return ps.SendPhoto(ctx, t, path, caption)
	}
	return src.SendFile(ctx, t, path, caption)
}

// MessageReactor is an OPTIONAL capability for sources that can react to an
// inbound message with an emoji ("seen" indicator). Callers type-assert against
// it at dispatch; sources that don't implement it are skipped silently.
// Best-effort by contract: a failure is returned but callers log + drop —
// reaction is a UX nicety and MUST NOT block reply delivery.
type MessageReactor interface {
	ReactToMessage(ctx context.Context, chatID, messageID, emoji string) error
}

// Gateway is the source bus: it owns every Source's lifecycle, fans their
// inbound events into one stream, and probes their health. Registered on the
// trunk by the stoma module; the inbound flow consumes Events and looks up a
// Source by name to render replies. The bus does NOT know about sessions,
// screening, or admin notification — those live above it in the flow.
type Gateway interface {
	// Events is the fanned-in inbound stream from every source. The bus closes
	// it after all sources stop (on Stop), so a `for range` consumer terminates.
	Events() <-chan MessageEvent
	// SourceByName returns the registered source, or nil if absent. Lock-free
	// (the source set is fixed at construction).
	SourceByName(name string) Source
	// SourceNames lists every registered source's name, sorted. The set is fixed
	// at construction; consumers (e.g. gate's per-source policy editors) may cache
	// it. Lock-free.
	SourceNames() []string
}
