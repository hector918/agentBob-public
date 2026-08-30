// Package core holds the data types and interfaces passed between pipelines.
// Keep it small — it is the ONLY package shared across pipelines.
package core

import "time"

// ChatType describes the shape of a conversation.
type ChatType string

const (
	ChatDM      ChatType = "dm"
	ChatGroup   ChatType = "group"
	ChatChannel ChatType = "channel"
	ChatTopic   ChatType = "topic" // forum topic / thread with its own id
)

// Target identifies where a reply goes.
type Target struct {
	Source    string // "local" | "telegram" | ...
	ChatID    string
	ThreadID  string // forum topic / thread id (optional)
	ReplyToID string // reply to a specific message id (optional; groups)
}

// Attachment is a non-text part of a message (image, voice, document, …).
//
// agentbob captures attachments (downloads them to LocalPath) AND exposes a
// sandbox-relative `Path` to the agent so attachment-consuming tools (ocr,
// read_file when the user shared a doc, …) can act on them without guessing
// the filename. LocalPath is an absolute container path (infrastructure /
// agent-internal); Path is the model-visible sandbox-relative form
// ("attachments/<date>/<file>") that ResolvePath joins onto
// the per-session sandbox.
//
// The gateway also pre-extracts text from voice/audio attachments before
// the agent turn: audio → Transcript (via ASR). Transcript is rendered by
// the prompt layer's describeAtt so the agent reads the spoken content
// directly without hearing audio. Image text is NOT pre-extracted — the
// vision model reads images directly and the `ocr` tool handles
// faithful-text extraction on demand.
type Attachment struct {
	Kind      string // "image" | "voice" | "audio" | "video" | "document" | "sticker" | "animation"
	MIME      string // best-effort
	FileName  string // best-effort ("" for things like stickers)
	Size      int64  // bytes, 0 if unknown
	LocalPath string // absolute path where it was saved; "" if not downloaded (too big / error). Internal — DO NOT surface to the model (use Path).
	Path      string // sandbox-relative path the agent can pass to tools (e.g. ocr's image_path arg). Same emptiness rule as LocalPath.
	FromReply bool   // true if it came from the replied-to message rather than this one
	// Transcript is the speech-to-text transcript of a voice/audio
	// attachment. Set by the gateway (transcribe hook in dispatch) so the
	// attachment's spoken content can reach the agent as text — the agent's
	// model cannot hear audio. Empty for non-audio attachments and for the
	// user's OWN voice (that transcript becomes MessageEvent.Text directly);
	// populated for a replied-to voice/audio so the prompt layer can render
	// it as labeled replied-to context rather than the user's own words.
	Transcript string
	// OCRText is the pre-extracted text of an IMAGE attachment, set by the
	// orchestrator's forced-OCR pass when the session is染色'd with a skill
	// that declares metadata.bob.force_ocr (抽取类 skill, e.g. 超声波报告).
	// describeAtt renders it inline (parallel to Transcript) so the image's
	// text reaches the image-blind main model BY CONSTRUCTION — the model no
	// longer has to remember to call the `ocr` tool, and can't "answer" an
	// image it never read. Empty for non-image attachments and for ordinary
	// (non-force_ocr) sessions, where the on-demand `ocr` tool stays the path.
	OCRText string
}

// MessageEvent is one inbound message, normalized across platforms — the
// standardized hand-off from a Source to the rest of the pipeline. Sources fill
// in what they can; fields are best-effort unless noted. Pipeline code should
// depend only on the typed fields below — never on Raw.
type MessageEvent struct {
	Source     string   // the Source's Name() ("local" | "telegram" | …)
	BotID      string   // identity of the bot that received this event (empty in single-bot deployments)
	ChatType   ChatType // dm | group | channel | topic
	ChatID     string
	ThreadID   string // forum topic / thread id (optional); used for delivery, not session keying
	MessageID  string
	ReplyToID  string // id of the message this one replies to (optional)
	ReplyToBot bool   // true if ReplyToID points at a message the bot sent (the hub resolves which session via the message_index)
	// ReplyRefs are additional ancestor message-ids to try (after ReplyToID)
	// when routing a reply to its session — the email References chain,
	// NEWEST-first. Lets reply routing still find the thread when a client
	// dropped In-Reply-To but kept References. Only the email source sets it.
	ReplyRefs []string
	// DMReplyRouting opts a DM source into per-reply session routing: a reply
	// (ReplyToID/ReplyRefs that hits a recorded bob message) continues/revives
	// THAT session; a fresh, non-reply message opens a NEW session — instead of
	// one ever-growing active session per scope. Only the email source sets it
	// (async threaded medium); chat DMs (telegram/wechat) leave it false so a
	// plain message still continues the running conversation. See
	// docs/group-session-archive.md (same reply=continue / fresh=new model as
	// groups, applied to the email DM scope).
	DMReplyRouting bool
	ReplyToText    string // text/caption of the replied-to message (+ a "（一张图片，不是本轮附件）"-style note if it carries media); empty if not a reply
	ReplyToUser    string // display name of the replied-to message's sender (or "" if unknown)
	UserID         string
	UserName       string // display name (for the "[Name]:" speaker prefix in group sessions)
	// StableUID is the cross-app-stable identity for accounts handle-keying when
	// it differs from UserID — feishu sets it to union_id (tenant-wide, stable
	// across apps) while UserID stays open_id (what the per-app allowlist/admin
	// gate matches). Empty for sources where UserID is already stable (telegram
	// user_id, email address). Read only via AccountHandle(); the gate/allowlist
	// path still uses UserID. See docs/accounts.md §12 L3.
	StableUID string
	IsAdmin   bool // true if this user is an admin (gateway.admins; the local terminal user is always admin)
	// Lang is the resolved BCP-47-ish code for outbound i18n.T(); populated
	// at gateway-entry from i18n.PickLangFromEvent(ev) and reused downstream
	// — DO NOT recompute mid-flow.
	Lang        string
	Text        string
	Attachments []Attachment
	// ServiceUnits carries NATIVE-unit usage measured during gateway preprocess
	// (before the turn), keyed "kind:unit" → amount — e.g. "asr:s" → audio
	// seconds transcribed. The orchestrator drains it into the per-handle service
	// ledger at turn end (kept separate from token counts so units never conflate;
	// docs/accounts.md §13). nil for messages with no preprocessed media.
	ServiceUnits map[string]int64
	ReceivedAt   time.Time
	// MediaGroupID is the Telegram media-group identifier — a random
	// per-album string set by the Telegram client when the user uploads
	// 2+ photos as one album. Only the first photo of the group carries
	// the user's caption; the others arrive with empty Text. The
	// telegram source's mediaGroupBuffer (sources/telegram/
	// 35-mediagroup.go, 10s flush timeout) merges the album N-into-1
	// before Submit, copying the caption onto the siblings so every
	// photo reaches the prompt layer with the caption that semantically
	// belongs to it.
	//
	// Empty for non-Telegram sources and for Telegram messages that
	// aren't part of a media group.
	MediaGroupID string
	// Metadata is the optional agora typed envelope: kind / from / validation /
	// reply / priority / report_to / etc. Populated by the internal source
	// (send_message / report tools); ignored by telegram /
	// local-cli sources. The agora dispatcher reads this to decide routing
	// (bridge inbox forwarding, urgent priority) without widening every
	// downstream Source impl.
	//
	// Lifecycle: written by tools at Emit time, read by gateway when
	// computing the dispatch_queue row (priority, kind), then dropped
	// — not persisted to the messages table (M3b stays light; M5+ may
	// add a messages.metadata JSON column).
	//
	// CONTRACT: treat as read-only. Producer creates this map fresh per
	// Emit; no defensive clone on the read side. If a consumer ever
	// needs to derive a modified version, do maps.Clone first.
	Metadata map[string]any
	// RequiredModelTags is the source's declared model-pool tag
	// requirement for this event's agent turn. agent-orchestrator
	// appends every entry to core.ModelRequest.Requires; when the pool
	// returns model.ErrNoModelAvailable for the requested set, the turn
	// fails with core.ErrRequiredModelMissing — no fallback chain runs.
	//
	// Sources populate this for outbound surfaces where a tonally /
	// contractually wrong model is a worse outcome than no reply (e.g.
	// email, where format expectations differ from chat). Empty = source
	// has no preference; the existing pool-default + fallback behaviour
	// applies. Telegram / wechat leave it nil.
	RequiredModelTags []string
	// PreferredModelTags is the admin's per-session model-tag preference
	// for this event's agent turn (/model tag, docs/slash.md). The soft
	// sibling of RequiredModelTags: agent-orchestrator feeds it into
	// core.ModelRequest.Prefer, so it only RANKS qualifying pool entries
	// — when no live entry carries the tag the pool falls back to its
	// normal priority pick and the turn proceeds. Stamped onto the batch
	// by pipeline-session's runTurn from the Manager's per-sid tag map;
	// empty for sessions with no admin override.
	PreferredModelTags []string
}

// ReplyTarget returns a Target that replies back to this event's chat/thread.
func (e MessageEvent) ReplyTarget() Target {
	return Target{Source: e.Source, ChatID: e.ChatID, ThreadID: e.ThreadID}
}
