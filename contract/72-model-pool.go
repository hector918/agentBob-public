package contract

import (
	"context"
	"errors"
	"time"
)

// ErrModelQueueFull is the sentinel a ModelPool returns when a request would
// have to WAIT for a backend slot but that kind's wait queue is already at
// capacity. It is a BUSY signal, not an outage: the backends are alive, there
// is simply nowhere left to stand in line. Cross-layer (unlike the pool's
// internal sentinels) because the user-facing surfaces — a media tool's error
// text, an i18n'd reply — must say "the queue is full, try again shortly"
// rather than the "backend unavailable" a real outage earns.
var ErrModelQueueFull = errors.New("contract: model pool queue full")

// Chatter is the minimal model-backend interface — one connection to one
// provider. Higher-level routing (multi-entry tag matching, liveness, fallback)
// lives in the ModelPool; a Chatter just talks to one backend. Defined over the
// contract envelope so the provider impls and the pool share one vocabulary.
//
//   - ChatStream: streaming completion — the final user-visible reply, and tool
//     rounds (when tools is non-empty, streaming backends emit ToolCallDelta
//     events). tools nil/empty → text-only stream.
//   - Chat: non-streaming completion — side-LLM callers (compression, judge).
//
// ChatStream's returned error is for pre-stream failures; once the channel is
// open, transport errors arrive on it as StreamEvent{Err:…, Done:true}.
type Chatter interface {
	ChatStream(ctx context.Context, model string, messages []Message, tools []ToolSpec) (<-chan StreamEvent, error)
	Chat(ctx context.Context, model string, messages []Message, tools []ToolSpec) (ChatResponse, error)
	// Ping is a cheap reachability check (a token-free request). nil error =
	// REACHABLE (the host answered at all, even non-2xx); it does NOT prove the
	// backend can serve completions, so the pool treats a Ping success as
	// tentative recovery only. Only a transport-level failure returns non-nil.
	Ping(ctx context.Context) error
}

// Model-entry kinds. Kind is a pool entry's routing class: a ModelRequest is
// only matched against entries of the same Kind (empty → KindLLM). Keeps a
// non-chat model out of the general chat candidate pool.
const (
	KindLLM       = "llm"       // chat-capable language model (the default)
	KindASR       = "asr"       // speech-to-text / audio transcription
	KindTranslate = "translate" // machine-translation model
	KindOCR       = "ocr"       // image-to-text / optical character recognition
	KindImage     = "image"     // text-to-image / image-to-image generation
)

// Image-generation progress stages (ImageEvent.Stage).
const (
	// ImageStageSubmitted fires the moment the backend accepts the job and names
	// it — BEFORE any pixel exists. A caller that must survive its own death (the
	// draw tool's write-ahead log) records PromptID here and nowhere else.
	ImageStageSubmitted = "submitted"
	// ImageStageProgress reports step progress. Cur/Max are per-PHASE, not global:
	// a run reports sampling steps first (x/steps) and then tiled decoding
	// (x/tiles), so Max changes mid-job and the pair must not be rendered as one
	// monotonic bar.
	ImageStageProgress = "progress"
	// ImageStageFetching means generation finished and the bytes are being pulled.
	ImageStageFetching = "fetching"
)

// ImageEvent is one progress notification from an image backend. It travels on
// the request context (see the model providers' WithImageProgress) rather than
// through Chatter, because generation is a job with a lifecycle while Chatter is
// a single call — putting the lifecycle in the return type would reshape every
// other provider for the sake of one.
type ImageEvent struct {
	Stage    string
	PromptID string // ImageStageSubmitted only
	// Entry names the pool entry that accepted the job (ImageStageSubmitted only).
	// The caller never chose it — the pool did — so this is the only way a
	// recovery path can later pin the SAME backend to ask about the job.
	Entry    string
	Cur, Max int // ImageStageProgress only
}

type imageProgressKey struct{}

// WithImageProgress tags ctx with a sink for ImageEvents. The producer (an image
// provider) and the consumer (the tool that asked for the picture) are in
// different leaf modules and may not import each other, so the hook lives here in
// contract — below both — rather than in either one.
//
// The hook is called from the provider's goroutines: it MUST be safe for
// concurrent use, and MUST NOT do anything slow — no network calls, no waiting on
// another party — because generation stalls behind it. A brief local write is
// fine and is in fact required on ImageStageSubmitted: a caller that keeps a
// durable record of in-flight jobs has to commit it BEFORE this returns, or the
// crash it is guarding against can happen in the gap.
func WithImageProgress(ctx context.Context, fn func(ImageEvent)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, imageProgressKey{}, fn)
}

// ImageProgressFrom returns the hook set by WithImageProgress, or a no-op when
// none was set. Never nil, so providers need no nil checks.
func ImageProgressFrom(ctx context.Context) func(ImageEvent) {
	if fn, ok := ctx.Value(imageProgressKey{}).(func(ImageEvent)); ok && fn != nil {
		return fn
	}
	return func(ImageEvent) {}
}

// ModelRequest is what the agent asks the pool for. The pool picks an entry of
// the requested Kind whose Tags satisfy Requires, ranked by how many of Prefer
// it has, then dispatches to that entry's Chatter. PinnedEntry forces a specific
// entry by Name (errors if not live; the pinned path does not check Kind).
type ModelRequest struct {
	Kind        string     // entry kind to route to; empty → KindLLM
	Requires    []string   // tags the chosen entry MUST have (empty → any)
	Prefer      []string   // tags to break ties among qualifying entries
	Tools       []ToolSpec // advertised to the model every round when the caller has them (turn core); side-LLM callers (compression/judge/salvage) leave it empty
	PinnedEntry string     // force this exact entry by Name; empty → tag-match
	// AffinityKey is an opaque conversation key (the turn stamps its session id)
	// for SOFT prompt-cache affinity. The pool remembers which entry last served
	// each key (sliding TTL, in-memory) and, when a pick otherwise ties (same
	// Prefer-hits, same Priority, same in-flight load), (1) keeps this key on its
	// remembered entry — its KV cache is warm there — and (2) steers traffic away
	// from entries remembered by OTHER live keys, so a stranger landing on that
	// backend doesn't evict their cache slot. Unlike PinnedEntry this is never a
	// hard requirement: load balancing outranks both directions (a busier
	// remembered entry is passed over — "about to be full" beats the soft hold),
	// and an ineligible entry is skipped as usual. Empty → this request records no
	// affinity; it still avoids entries other live keys occupy.
	AffinityKey string
}

// ModelInfo describes one pool entry for snapshot / listing. State and InFlight
// are dynamic (updated by the pool); the rest is configured at pool-build time.
// ContextSource / ConcurrencySource label where the effective values came from
// ("yaml" / "probe:<provider>" / "default") for provenance.
type ModelInfo struct {
	Name              string
	Provider          string
	Model             string
	Kind              string
	Priority          int // static tie-breaker; <-10000 = disabled
	Tags              []string
	ContextWindow     int
	ContextSource     string
	Concurrency       int // per-entry concurrency cap (0 → unlimited)
	ConcurrencySource string
	State             string // "live" | "tentative" | "paused" | "cooling" | "disabled"
	InFlight          int

	LastError     string // most recent counted backend error (the "why" behind cooling); admin-only
	DeadUntilUnix int64  // unix-sec the current cooldown ends, or 0

	// Process-lifetime in-memory usage counters (reset on restart).
	TotalCalls        int64
	TotalErrors       int64
	TotalInputTokens  int64
	TotalOutputTokens int64
}

// HeartbeatInfo / ReloadInfo / UsageInfo are the non-entry parts of a snapshot.
type HeartbeatInfo struct {
	Running bool
}
type ReloadInfo struct {
	ConfigPath string
	// LastMtime is the mtime of the config the pool is SERVING — it advances
	// only after a reload succeeds, so it answers "did my edit take effect".
	// A config that failed to load leaves this at the previous one; the
	// failure itself reaches the operator as an admin page, not as a stat.
	// Zero before any config has loaded, or when the mtime watch is off.
	LastMtime time.Time
}
type UsageInfo struct {
	HasStore bool
}

// QueueInfo is one kind's WAIT QUEUE readout: how many requests are blocked
// waiting for a concurrency slot of that kind. It is deliberately NOT derivable
// from InFlight — an in-flight call already HOLDS a slot, so a pool that is
// saturated and one that is saturated WITH a line behind it read identically
// there. Capacity is what would start rejecting further waiters (0 = uncapped:
// some entry of the kind declares unlimited concurrency, so nothing queues on
// it). Full mirrors the kind's queue-full latch — callers are being turned away
// right now.
type QueueInfo struct {
	Kind     string
	Waiting  int
	Capacity int
	Full     bool
}

// PoolSnapshot is the live state of the pool (the model module's StateReporter
// output — read by cli / webui).
type PoolSnapshot struct {
	Entries  []ModelInfo
	InFlight int
	// Queues holds ONLY the kinds with someone waiting — an empty slice is the
	// normal resting state, so a reader never has to distinguish "no queue" from
	// "queue of zero".
	Queues    []QueueInfo
	Heartbeat HeartbeatInfo
	Reload    ReloadInfo
	Usage     UsageInfo
}

// ModelPool routes a ModelRequest to a live backend entry. Registered on the
// trunk by the model module; consumed by the agent loop, side-LLM callers, and
// the gateway preprocess path.
type ModelPool interface {
	// Chat picks an entry, does a non-streaming call — side-LLM callers
	// (compression, judge, learning) wanting a clean ChatResponse.
	Chat(ctx context.Context, req ModelRequest, messages []Message) (ChatResponse, error)
	// ChatStreamWatch is the streaming-with-inspection entry — picks an entry,
	// opens a streaming call (with tools if req.Tools is non-empty), drains
	// events through watcher (if non-nil), and returns the assembled response.
	// The watcher can abort mid-stream by returning a non-nil error.
	ChatStreamWatch(ctx context.Context, req ModelRequest, messages []Message, watcher StreamWatcher) (ChatResponse, error)
	// Snapshot returns the live state of every entry.
	Snapshot() PoolSnapshot
	// FlushUsage persists completed-hour usage (housekeeping tick); the
	// in-progress hour keeps accumulating.
	FlushUsage(ctx context.Context)
	// FlushUsageFinal persists ALL accumulated usage including the in-progress
	// hour (graceful shutdown).
	FlushUsageFinal(ctx context.Context)
	// Close releases pool-lifetime background machinery (the liveness heartbeat).
	Close()
}

// Transcriber turns an inbound event's soundtracks into text at ingestion — the
// media→text preprocess (F165). Provided by the leaf/asr module over the model pool's
// KindASR backend; consumed OPTIONALLY by the session core (submit folds the result
// into the message text so spoken content is ordinary text downstream). A nil
// Transcriber (no asr module / no ASR backend) simply means no transcription.
type Transcriber interface {
	// Transcribe returns the spoken text split by WHOSE WORDS IT IS, because the two
	// halves belong in different places in the message:
	//
	//   instruction — the sender's own voice note with no caption. The transcript IS
	//     their request; submit puts it in the message text, exactly as if typed. Empty
	//     unless the event is captionless AND the clip came from this message (not from
	//     a replied-to one).
	//   material    — everything else with a soundtrack: a captioned clip, a clip
	//     carried in from a replied-to message, a video's audio. Third-party content
	//     that must NOT occupy the instruction position, so it arrives already framed
	//     with its source and sanitised against the prompt's own meta structure.
	//
	// Both are ordinary text (F165: no transcript field, no separate render section) —
	// the split is about POSITION, not about a new data shape. Either may be empty.
	//
	// Takes the whole event because the split needs the caption and the reply flags, not
	// just the attachments. Reads the attachments' staged LocalPath, so it runs BEFORE
	// space placement.
	Transcribe(ctx context.Context, ev MessageEvent) (instruction, material string)
}

// ImageStyleInfo is one drawable style as the catalog advertises it — everything
// a caller needs to CHOOSE, without anything it needs to RUN.
type ImageStyleInfo struct {
	Style   string
	Summary string           // one line: what this style is for
	ETA     string           // rough wall-clock, so a caller can warn before committing
	Note    string           // caveat worth showing beside the name (e.g. "slow tier")
	Sizes   map[string][]int // aspect → [w, h]; the sizes this style may be asked for
	// Changes lists the edit strengths this style accepts, sorted weakest first.
	// Advertised as well as validated against: an image-edit caller cannot guess
	// them, and a value that reaches the backend unrecognised comes back as a
	// routing-shaped failure with nothing to do with what actually went wrong.
	Changes []string
	// Guide names the manual this style is documented by. Styles sharing a name
	// share their prompt rules and are ALTERNATIVES to each other — a listing that
	// groups by it lets a caller see the trade-off (fast vs good) instead of
	// reading the same summary once per tier. It is a manual id, not a backend name.
	Guide string
}

// ImageCatalog answers "what can be drawn, and how do I write for it" — the two
// questions every consumer of image generation has and the pool alone cannot
// answer.
//
// It exists because that knowledge has MORE THAN ONE consumer: the image_create
// tool drives a conversation with it, and modelgate hands it to external callers
// (promptlib and friends) that write their own prompts. Those live in different
// leaf modules and may not import each other, so the prose has exactly one home
// that both can reach — here, behind an interface the model module provides.
//
// Keeping it in one place is the point: prompt guidance is the part that gets
// tuned most, and a second copy would drift from the first the week after it was
// made (docs/image-create-tool.md §4).
type ImageCatalog interface {
	// ImageStyles lists every declared style, sorted by name.
	ImageStyles() []ImageStyleInfo
	// ImageGuide returns the full prompt-writing manual for a style. Several
	// styles may share one manual — the tiers of a model family differ in a few
	// lines and are chosen by comparing them, which needs them in one document.
	ImageGuide(style string) (string, bool)
}
