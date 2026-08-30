// Package sendgate is the per-bot outbound send discipline shared by the IM
// sources — ONE implementation of "what happens when a send hits the platform
// rate limit", consumed two ways:
//
//   - Gate: the per-bot serialiser (telegram, feishu). Every REST send for one
//     bot — streaming edits, one-off notices, reactions, file uploads — funnels
//     through a single slot, honours a shared 429 cooldown floor before dialing,
//     and RELAYS a throttled call itself (bounded) instead of dropping it. The
//     platforms rate-limit per bot token, so per-bot serialisation is the
//     correct grain.
//   - Relay: the same bounded 429 retry without the serialiser, for sources
//     whose client library already paces/serialises requests internally
//     (discord: discordgo buckets per route) — an outer per-bot slot there
//     would only head-of-line-block unrelated channels.
//
// Timeliness is the CALLER's ctx deadline, not a gate knob: every wait (slot
// queue, cooldown floor, relay backoff) is ctx-aware, and an expired ctx is
// checked once more immediately before dialing — a typing indicator or
// reaction that queued past its useful life is silently dropped, never sent
// late. Callers declare urgency by bounding their ctx.
//
// Only a CLASSIFIED 429 is ever relayed: the platform explicitly rejected the
// send, so a retry cannot duplicate. Every other error (timeouts included —
// those MAY have delivered) returns to the caller untouched; the streamsink
// core's own retry keeps handling non-429 transients above this layer.
package sendgate

import (
	"context"
	"time"
)

const (
	// RelayAttempts bounds one send's total dials (initial + relays).
	RelayAttempts = 3
	// RelayClamp is the longest single retry-after honoured in-line. Gate.Do
	// hands a longer ask back instead of parking its slot (the cooldown floor
	// stays pushed, so every later send still backs off); Relay — which holds
	// no slot — clamps the wait and retries anyway (the L-D3 guarantee: a
	// one-off notice must not drop just because the server asked for a minute).
	RelayClamp = 15 * time.Second

	// ReactionTTL is the shared urgency bound for best-effort ornaments
	// (read-ack reactions): queued past this, the send is dropped, never fired
	// late. Sources wrap their reaction ctx with it before entering the gate.
	ReactionTTL = 10 * time.Second
)

// Classifier reports whether err is the platform's 429 and the retry-after to
// honour. Nil errors must report false.
type Classifier func(err error) (retryAfter time.Duration, ok bool)

// Gate is one bot's outbound serialiser: a single slot + a shared cooldown
// floor + the bounded 429 relay. Zero value is not usable — construct with New.
type Gate struct {
	classify Classifier
	slot     chan struct{}
	// notBefore is the shared cooldown floor. Only the slot holder reads or
	// writes it, so it needs no extra lock.
	notBefore time.Time
}

// New builds a gate with the platform's 429 classifier.
func New(classify Classifier) *Gate {
	return &Gate{classify: classify, slot: make(chan struct{}, 1)}
}

// Do runs one wire call through the gate. It queues for the slot (ctx-aware,
// so a bounded-ctx caller abandons the queue instead of waiting forever),
// sleeps out the shared cooldown floor, re-checks ctx immediately before
// dialing (timeliness), and on a classified 429 pushes the floor and relays —
// see the package doc for the full discipline.
func (g *Gate) Do(ctx context.Context, call func() error) error {
	select {
	case g.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.slot }()

	for attempt := 1; ; attempt++ {
		// Shared cooldown floor: an earlier 429 (this call's own relay included —
		// the loop-top wait IS the relay backoff) paces every send.
		if wait := time.Until(g.notBefore); wait > 0 {
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
		}
		// Timeliness: queued/floored past the caller's deadline → drop, don't dial.
		if err := ctx.Err(); err != nil {
			return err
		}
		err := call()
		retryAfter, throttled := g.classify(err)
		if !throttled {
			return err // success, or a non-429 the caller (or streamsink) handles
		}
		if nb := time.Now().Add(retryAfter); nb.After(g.notBefore) {
			g.notBefore = nb
		}
		if attempt >= RelayAttempts || retryAfter > RelayClamp {
			return err // budget spent (or too long to park the slot) — floor stays pushed
		}
	}
}

// Relay is the gate's 429 retry without the serialiser: same attempts budget,
// same 429-only rule, waits ctx-aware. A long retry-after is CLAMPED and still
// retried (no slot is held, so there is nothing to protect by handing back).
// For sources whose client library already paces requests internally.
func Relay(ctx context.Context, classify Classifier, call func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		wait, throttled := classify(err)
		if !throttled || attempt >= RelayAttempts {
			return err
		}
		if wait > RelayClamp {
			wait = RelayClamp
		}
		if serr := sleepCtx(ctx, wait); serr != nil {
			return serr
		}
	}
}

// sleepCtx sleeps d or returns ctx.Err() early — the one copy of the
// timer-select footgun (Stop on the ctx branch) for the package.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
