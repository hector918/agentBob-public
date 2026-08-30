// Package streamsink is the shared reply-rendering core for bob's external
// message channels. It owns ALL sink orchestration once and exposes it to
// every source through a narrow platform seam, so a new channel plugs into
// bob by writing only its own leaf — the gateway / session layers and this
// core never change.
//
// # Capability-maximal core, fallback in the source
//
// The core implements the FULLEST rendering case: a two-channel
// (trace + content) buffered renderer with a rate-limited edit cadence,
// over-cap message splitting, automatic degrade-to-block on throttle, a
// "still working" typing indicator, and final-frame delivery that survives
// a cancelled turn context. A channel that cannot do some of this DECLARES
// the gap via WireCaps and the core degrades for it — that declaration is
// the "fallback in the source" the design mandates. Two rendering
// disciplines fall out of one core:
//
//   - edit-stream (Caps.CanEdit=true): telegram, feishu — buffer, flush on a
//     ticker by send-then-edit, split over the cap, degrade to block on a
//     sustained 429.
//   - block (Caps.CanEdit=false): email, wechat — no intermediate flush; one
//     render at Finish. Splitting still applies (wechat sends multiple texts;
//     email returns a huge MaxChars so it never splits).
//
// The universal contract every source already shares is contract.Sink; this core
// is its single shared implementation for external chat channels. Dev-only
// channels with a different rendering discipline stay bespoke on purpose:
// sources/local (terminal append-print) and sources/bridge (agora-internal
// no-op) are NOT external interfaces an inbox connects to, so the core does
// not carry their disciplines.
//
// # Boundary
//
// The core imports contract (for Sink / SinkPrefs) and the standard library only.
// No platform SDK (telegram bot, lark, openwechat, net/smtp) may appear here —
// every wire call goes through Wire. A platform import in this package is a
// boundary leak.
package streamsink

import (
	"context"
	"time"
)

// WireCaps declares what a channel's wire format can do. The core reads it
// once at sink construction (New) and shapes its behaviour accordingly. The
// zero value {false,false,false} is a valid block channel that never streams,
// never types, and never renders trace — the most degraded case.
type WireCaps struct {
	// CanEdit selects the rendering discipline. true → edit-stream: the core
	// runs a flush ticker and updates a sent message in place via Wire.Edit.
	// false → block: no intermediate flush, no Edit call ever; the core sends
	// each (possibly split) chunk once at Finish via Wire.Send.
	CanEdit bool

	// CanType lets the core periodically call Wire.Typing while the user has
	// not yet seen any content, as a "still working" signal. Channels with no
	// typing concept set false and the core never calls Typing.
	CanType bool

	// TraceRender is the channel's STRUCTURAL ability to show "what bob is
	// doing" trace annotations (separate from the per-scope prefs.Trace user
	// toggle). false → the core drops TraceDelta unconditionally, no trace is
	// ever rendered (email: a thread of half-thought-out replies is worse than
	// one finished one). true → trace is rendered when prefs.Trace is also on.
	TraceRender bool

	// LineBreak is how THIS channel writes a line boundary inside bob's own
	// line-structured text (a trace run, a slash command's reply). Empty — the
	// zero value, and the right answer for every plain-text channel — means a
	// bare "\n".
	//
	// It exists because a line boundary is not written the same way everywhere.
	// A markdown-rendering channel reads a single newline as a SOFT break and
	// collapses a whole trace run (or command list) into one paragraph; there
	// the boundary must be a GFM hard break, "  \n". A future channel that
	// renders HTML would need something else again.
	//
	// Only the ENCODING lives here. WHICH text is line-structured is the
	// producer's call — the core knows its trace channel, and the slash
	// dispatcher knows a command reply is bob's own UI — because a wire is
	// handed pre-joined text and cannot tell a command list from a
	// model-written table, whose newlines must NOT be rewritten.
	LineBreak string
}

// Wire is the per-source platform leaf. The core calls these for every wire
// interaction; the source supplies the platform minutiae and, via Caps,
// declares its fallbacks. All methods are called from the core's goroutines
// (the flush loop and the Finish caller) — implementations that touch shared
// source state must lock as the source requires; the core serialises Send /
// Edit across both channels under its own flush mutex, so a Wire never sees
// two concurrent Send/Edit calls from the same sink.
type Wire interface {
	// Caps declares the channel's rendering capabilities. Read once at New.
	Caps() WireCaps

	// Send posts a NEW message and returns its platform message id (for a
	// later Edit). anchorReply is true only for the first message the core
	// sends on a given channel (trace / content each get one) — a leaf that
	// supports reply-to may anchor that first message to the inbound user
	// message; later split-overflow messages pass false. For a block channel
	// every chunk is a fresh Send. The returned id is used by the core only
	// when Caps.CanEdit is true; block leaves may return "".
	//
	// The leaf records its own outbound message in any reply-routing index it
	// keeps (the core does not call back for that). The returned error MUST be
	// safe to surface to RateLimited / BenignEdit / EditGone type checks (raw,
	// not yet redacted) — the core redacts via RedactErr before logging.
	Send(ctx context.Context, text string, anchorReply bool) (msgID string, err error)

	// Edit replaces msgID's text in place. The core calls this only when
	// Caps.CanEdit is true and a prior Send on the channel returned msgID.
	Edit(ctx context.Context, msgID, text string) error

	// WireLen counts s in the unit the channel caps messages by (telegram:
	// UTF-16 code units; wechat: runes; byte- or rune-counted elsewhere). The
	// core uses it with MaxChars to decide when to split.
	WireLen(s string) int

	// MaxChars is the per-message budget in WireLen's unit. The core splits a
	// channel's text when WireLen(text) exceeds it. A channel with no real
	// per-message cap (email: one message however long) returns a large
	// constant so the core never splits.
	MaxChars() int

	// RateLimited reports whether err is a sustained throttle. 429 pacing
	// belongs to the source-level send gate, which has already relayed the
	// call before the error surfaced here — so the core treats a throttle as
	// TERMINAL: it stops retrying immediately and degrades an edit-stream
	// channel to block mode (final render at Finish only). retryAfter is NOT
	// consumed by the core; it is kept in the signature because the same
	// classifier typically also serves the source's send-gate cooldown.
	// Channels with no per-message throttle return ok=false.
	RateLimited(err error) (retryAfter time.Duration, ok bool)

	// BenignEdit reports whether an edit error means the edit was a NO-OP —
	// the message already shows exactly the pushed text (e.g. telegram's
	// "message is not modified"). The core treats such an error as SUCCESS:
	// the content is on screen, there is nothing to retry or recover. Block
	// channels can return false.
	BenignEdit(err error) bool

	// EditGone reports whether an edit error means the edit TARGET is gone or
	// permanently uneditable (deleted by the user / locked by the platform —
	// e.g. telegram's "message to edit not found" / "message can't be
	// edited"). The core drops the dead message id and re-delivers the
	// current window as a brand-new Send — re-editing a dead id on every
	// flush would silently lose the rest of the reply. Block channels can
	// return false.
	EditGone(err error) bool

	// Typing pings a "still working" indicator. Best-effort: the core ignores
	// the outcome, so the leaf logs its own failures (once). Called only when
	// Caps.CanType is true and no content has been shown yet. No-op where the
	// channel has no typing concept.
	Typing(ctx context.Context)

	// RedactErr scrubs secrets (tokens / post-login URL params) from err
	// before the core writes it to a log. The core calls it at every log site;
	// the leaf returns a log-safe error (it need not preserve type identity —
	// the core has already done its RateLimited / BenignEdit / EditGone checks
	// on the raw error before redacting).
	RedactErr(err error) error
}

// BlockWireDefaults provides no-op implementations of the edit-stream-only Wire
// methods (Edit, RateLimited, BenignEdit, EditGone, Typing) for embedding in a
// block leaf (Caps.CanEdit=false, CanType=false). Embed it and implement only
// the methods that actually vary for a single-send channel: Caps, Send, WireLen,
// MaxChars, RedactErr. The defaults are semantically correct for block channels —
// Edit / Typing are never called (CanEdit / CanType false), and a block channel's
// Send errors are neither throttles nor edit-state outcomes.
type BlockWireDefaults struct{}

func (BlockWireDefaults) Edit(context.Context, string, string) error { return nil }
func (BlockWireDefaults) RateLimited(error) (time.Duration, bool)    { return 0, false }
func (BlockWireDefaults) BenignEdit(error) bool                      { return false }
func (BlockWireDefaults) EditGone(error) bool                        { return false }
func (BlockWireDefaults) Typing(context.Context)                     {}
