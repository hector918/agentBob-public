package contract

import (
	"strings"
	"time"
	"unicode"
)

// CleanFileName normalizes an attachment filename: control chars / newlines →
// space, trimmed, capped at 80 runes. It is NOT what the prompt shows the model —
// describeAttachments renders the on-disk Path verbatim (via prompt.StripControl).
// CleanFileName is the normalizer the attachment-set MATCHER (flow/compose's
// 45-attachset.go) applies to BOTH sides of a filename-hint comparison, so a user's
// hint and the stored name compare in one canonical form however either was written.
func CleanFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) { // covers \n \r \t (C0 controls)
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		s = strings.TrimSpace(string(r[:80]))
	}
	return s
}

// ChatType describes the shape of a conversation.
type ChatType string

const (
	ChatDM    ChatType = "dm"
	ChatGroup ChatType = "group" // ANY multi-party space — group / broadcast channel / forum topic all fold here
)

// IsGroupChat reports whether the chat is a multi-party space. Every multi-party chat —
// a plain group, a broadcast channel, a forum topic — maps to ChatGroup at the source
// boundary; the core routes only on the dm-vs-group distinction. A platform sub-thread
// (a forum topic) is NOT its own ChatType — it keeps its id in ThreadID for delivery
// only (ScopeFor flattens it out of the group scope). DM is the only non-group chat.
func (t ChatType) IsGroupChat() bool {
	return t == ChatGroup
}

// Attachment is a non-text part of a message (image, voice, document, …).
//
// Sources capture attachments (download them to LocalPath) AND expose a
// space-relative Path to the agent so attachment-consuming tools can act on
// them without guessing the filename. LocalPath is an absolute container path
// (infrastructure / agent-internal); Path is the model-visible space-relative
// form. At placement (leaf/warrant's inboxBaseName/reserveInboxName) the base is
// derived from the display name by an ASCII-only sanitizer (stem sanitised, ext
// preserved, "-N" appended on a clash), so Path's base may DIFFER from FileName;
// Path is the on-disk truth, and the prompt shows this exact string verbatim for
// the model to pass straight to a tool (a nameless attachment keeps its unique
// "<nanos>-<hex>-<name>" staged base instead).
type Attachment struct {
	Kind     string // "image" | "voice" | "audio" | "video" | "document" | "sticker" | "animation"
	MIME     string // best-effort
	FileName string // best-effort ("" for things like stickers)
	Size     int64  // bytes, 0 if unknown
	// LocalPath is the STAGED absolute path a source writes the download to. It is
	// transient and placement-internal: session submit relocates the file into the
	// scope's space and CLEARS LocalPath (Path then points at the space). Tools never
	// read LocalPath — they go through the FileChannel via Path. Never surfaced to the
	// model. json:"-": it is empty by the time an attachment is persisted, and a host
	// path must never reach the durable record anyway.
	LocalPath string `json:"-"`
	// Path is the space-relative path tools pass to the FileChannel ("inbox/<clean
	// name>" once placed — the same string the prompt shows the model). "" if not
	// downloaded / not placed. The downloaded-and-usable check.
	Path      string
	FromReply bool // true if it came from the replied-to message rather than this one
	// Transcribed marks that this attachment's spoken content IS ALREADY IN THE MESSAGE
	// TEXT — the transcriber ran at ingestion and folded the words in.
	//
	// A BIT, not the words. F165 deleted the per-attachment transcript field on purpose
	// (spoken content is ordinary message text for every downstream reader, and a second
	// copy is a second thing to keep in sync); this says only WHETHER that happened, and
	// exists because the prompt's attachment list could not otherwise tell. Without it a
	// voice note renders as 「- 音频：inbox/voice-3.ogg」 — indistinguishable from an
	// untouched file — and the model spends a tool round re-transcribing words already
	// sitting three lines above it. Observed in production: a 2-second clip
	// transcribed twice, 16 s wasted.
	//
	// Set by the Transcriber (leaf/asr) IN PLACE on the event's slice, before placement;
	// read by flow/compose when rendering the list. False also covers "declined for
	// length" and "transcription failed" — in both, the words are genuinely absent and
	// fetching them is the right move.
	Transcribed bool
}

// InboxSubdir is the per-space subdir inbound attachments are placed under (kept
// separate from model-created files at the space root). Shared vocabulary: the
// placement step writes here and the media tools list it to reach an EARLIER turn's
// attachment.
const InboxSubdir = "inbox"

// NoCaptionStandIn is the placeholder text session submit synthesises as the text
// of an attachment-only message BEFORE dispatch, so the turn has something to act
// on. Shared vocabulary: leaf/session stamps it, and ingestion-time ASR (F165)
// must recognise it as "no caption" — a caption-less voice note's Text is this
// stand-in when transcription runs, so the transcript REPLACES it rather than
// appending after it.
const NoCaptionStandIn = "（用户发送了附件，未附文字说明）"

// IsImageContent reports whether the attachment carries OCR-able image CONTENT
// (ignoring whether it is downloaded). Kind is a transport fact, not a content
// truth — a source may deliver an iPhone .HEIC as Kind="document" or a static
// sticker — so a document/sticker counts only when its MIME is image/*. The single
// content predicate shared by the prompt's attachment list (the 图片 label) and the
// ocr tool's candidate gate, so the two never disagree on "is this an image".
func (a Attachment) IsImageContent() bool {
	switch a.Kind {
	case "image":
		return true
	case "document", "sticker":
		// Covers the real-world case that matters: an iPhone photo arrives as Kind=document
		// with MIME image/heic — detectImageMIME + the OCR backend handle HEIC end-to-end.
		return strings.HasPrefix(strings.ToLower(a.MIME), "image/")
	}
	// Kind="animation" (a Telegram "GIF", delivered as MP4) is deliberately NOT image
	// content — it is VIDEO content, see IsVideoContent. That is what D40
	// recorded as an accepted gap for want of a frame-extraction step; the step now
	// exists (leaf/tools frame sampling), so the gap closed on the video side rather
	// than by widening this predicate. Static stickers / image/webp ARE covered above.
	return false
}

// IsAudioContent reports whether the attachment's content is SOUND — a voice note, an
// audio file, or an audio/* document. Same Kind-is-transport reasoning as its siblings.
//
// Note what it excludes: a VIDEO also carries a soundtrack, but it is not audio content,
// it is video content that happens to have sound. Callers that want "anything with a
// soundtrack" union this with IsVideoContent explicitly, which keeps the two questions
// — "what IS this" and "what can I get out of it" — from collapsing into one predicate.
func (a Attachment) IsAudioContent() bool {
	switch a.Kind {
	case "voice", "audio":
		return true
	case "document":
		return strings.HasPrefix(strings.ToLower(a.MIME), "audio/")
	}
	return false
}

// IsVideoContent reports whether the attachment carries VIDEO content (ignoring whether
// it is downloaded). Same shape as IsImageContent and the same reasoning: Kind is a
// transport fact, so a document/sticker counts only when its MIME says video/*. A
// Telegram "GIF" (Kind="animation") and a video sticker are both ordinary clips here —
// frame extraction does not care which transport delivered them.
//
// SEPARATE from IsImageContent on purpose. The two are unioned by the ONE caller that
// wants both (the vision tool's appetite); every other holder of IsImageContent —
// notably imagecreate's i2i init image — must keep rejecting video, which a widened
// image predicate would have silently broken.
func (a Attachment) IsVideoContent() bool {
	switch a.Kind {
	case "video", "animation":
		return true
	case "document", "sticker":
		return strings.HasPrefix(strings.ToLower(a.MIME), "video/")
	}
	return false
}

// MessageEvent is one inbound message, normalized across platforms — the
// standardized hand-off from a Source to the rest of the pipeline. Sources fill
// in what they can; fields are best-effort unless noted. Pipeline code depends
// only on the typed fields below.
type MessageEvent struct {
	Source   string   // the Source's Name() ("local" | "telegram" | …)
	ChatType ChatType // dm | group (all multi-party spaces fold to group — see ChatType)
	ChatID   string
	// ThreadID is the forum topic / thread id (optional). Used for delivery routing;
	// group/topic scopes flatten it, but a DM's thread MAY split session sub-scopes
	// via ScopeFor ("#<threadID>", the email forwarded-alias grammar).
	ThreadID   string
	MessageID  string
	ReplyToID  string // id of the message this one replies to (optional)
	ReplyToBot bool   // true if ReplyToID points at a message the bot sent
	// ReplyRefs are additional ancestor message-ids to try (after ReplyToID)
	// when routing a reply to its session — the email References chain,
	// NEWEST-first. Only the email source sets it.
	ReplyRefs []string
	// DMReplyRouting opts a DM source into per-reply session routing: a reply
	// continues/revives THAT session; a fresh message opens a NEW session.
	// Only the email source sets it; chat DMs leave it false.
	DMReplyRouting bool
	// ReplyToText is the text/caption of the replied-to message; empty if not a reply.
	// A source whose parent carried media appends prompt.QuotedMediaNote's note rather
	// than rolling its own — the wording is load-bearing and measured; see there.
	ReplyToText string
	ReplyToUser string // display name of the replied-to message's sender (or "")
	UserID      string
	UserName    string // display name (for the "[Name]:" speaker prefix in group sessions)
	// StableUID is the cross-app-stable identity for accounts handle-keying when
	// it differs from UserID. Empty for sources where UserID is already stable.
	StableUID string
	IsAdmin   bool // true if this user is an admin (stamped at ingress; the local terminal user is always admin)
	// Onboarding marks a sender the gate PASSED via a source's onboarding opt-in —
	// un-allowlisted and not yet approved (the router funnels them to intro). Stamped at
	// ingress alongside IsAdmin/Lang so the session layer can hold back a full inbound's
	// side effects for a provisional sender: no "seen" 👀 / busy notice, and a reused
	// per-sender onboarding session instead of a fresh row per group @ (F152). False for
	// an admin (never onboarded) and for any allowlisted/bound sender.
	Onboarding bool
	// Lang is the resolved BCP-47-ish code for outbound localization. The
	// inbound pipeline must stamp it once at ingress for EVERY event — including
	// a first-contact, un-allowlisted sender, whose bare-code redeem reply is
	// localized from it — and downstream code reuses it without recomputing.
	Lang        string
	Text        string
	Attachments []Attachment
	ReceivedAt  time.Time
	// MediaGroupID is the Telegram media-group identifier — a per-album string
	// set when the user uploads 2+ photos as one album. The telegram source emits
	// ONE event per album sibling (the caption shared across them), so downstream
	// sees N events carrying the same id — NOT a single merged event (see the
	// telegram source's media-group collector). Empty otherwise.
	MediaGroupID string
	// Dispatch carries the parameters of an agent→agent dispatch — set by the
	// send_message tool, consumed by the session router. Non-nil ONLY for a
	// parameterized internal dispatch; nil for wire chats AND for parameterless
	// internal wakes (cron / proactive / self-wake) and bridge return events. It is
	// the home for dispatch verbs (caller frame, force-new, future pin / credential)
	// so they never accrete onto this universal event. See MessageEventDispatchMeta.
	Dispatch *MessageEventDispatchMeta
}

// MessageEventDispatchMeta holds the parameters of an agent→agent dispatch carried on
// MessageEvent.Dispatch. Present only when an internal event carries dispatch intent;
// the session router reads it to thread the caller frame and pick fresh-vs-resume. A
// bridge return event leaves it nil — a return is the caller's inbound reply, not a
// new dispatch.
type MessageEventDispatchMeta struct {
	// CallerSessionID is the dispatching turn's session id — who the reply returns
	// to. A worker session opened by send_message records this as its
	// caller_session_id at creation, so its reply routes back along that lineage
	// edge. Mirrors how parent_session_id works for delegate.
	CallerSessionID string
	// ForceNewSession asks the router to open a FRESH session at the target scope
	// instead of resuming the active one — an agent declaring "new task". Never
	// closes the superseded session (sessions are never closed by design).
	ForceNewSession bool
	// NoReply marks a fire-and-forget wake whose VALUE is the turn's tool side-effects,
	// not a chat reply — so a flow should RUN the turn even with no deliverable reply
	// target (a discard sink), rather than dropping it. The arrangement dispatcher's
	// pull-nudge sets it (the woken member just calls arrangement_pull/submit). It is a
	// POSITIVE signal: an absent reply target alone (a routing FAILURE — lost caller,
	// store error) must still drop, so flows gate the discard-run on THIS, not on an
	// empty Target.Source.
	NoReply bool
}

// IsInternalSource reports whether a source name is the in-process internal source
// (agent dispatch, cron, proactive, self-wake) — a system delivery with no human
// recipient, as opposed to a real wire chat. The single definition behind every
// "internal vs wire" branch (queue priority, dedup/ack skip, lineage return-routing,
// trusted flow classification).
func IsInternalSource(source string) bool { return source == SourceNameInternal }

// IsInternal is the ergonomic MessageEvent form of IsInternalSource.
func (ev MessageEvent) IsInternal() bool { return IsInternalSource(ev.Source) }

// FirstHumanEvent returns the first non-internal (human-originated) event in a
// batch — the single anchor for per-batch decisions that must key on the human
// sender rather than a coalesced internal event (flow routing, billing/consumer
// handle, reply language). ok=false for an all-internal batch (a dispatch/cron
// wake), where callers either fall back to Events[0] or skip the human-keyed step.
func FirstHumanEvent(events []MessageEvent) (MessageEvent, bool) {
	for _, e := range events {
		if !e.IsInternal() {
			return e, true
		}
	}
	return MessageEvent{}, false
}

// DispatchCaller returns the dispatching caller's session id
// (Dispatch.CallerSessionID), or "" when the event carries no dispatch frame.
// Nil-safe accessor over the optional Dispatch sub-struct.
func (ev MessageEvent) DispatchCaller() string {
	if ev.Dispatch != nil {
		return ev.Dispatch.CallerSessionID
	}
	return ""
}

// AccountHandle returns the sender's cross-entry billing handle:
// (platform-family, stable-uid). The accounts ledger keys on the PLATFORM FAMILY
// (not the per-bot/per-account source name) plus a cross-app-stable uid, so the
// same person is ONE handle across all your telegram bots / email mailboxes /
// feishu apps. Source-name and UserID stay unchanged for allowlist/admin
// matching — only the accounts key normalizes here.
func (ev MessageEvent) AccountHandle() (source, uid string) {
	uid = ev.UserID
	if ev.StableUID != "" {
		uid = ev.StableUID
	}
	return platformKind(ev.Source), uid
}

// platformKind folds a per-bot / per-account source name to its platform family:
// telegram* → "telegram", email / email-* → "email", feishu* → "feishu",
// discord* → "discord". Anything else (local, internal, future singletons) is
// its own kind. The mapping that makes cross-bot / cross-mailbox dedup work.
func platformKind(source string) string {
	switch {
	case strings.HasPrefix(source, "telegram"):
		return "telegram"
	case source == "email" || strings.HasPrefix(source, "email-"):
		return "email"
	case strings.HasPrefix(source, "feishu"):
		return "feishu"
	case strings.HasPrefix(source, "discord"):
		return "discord"
	default:
		return source
	}
}
