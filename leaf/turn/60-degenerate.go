package turn

import (
	"errors"
	"log/slog"
	"unicode/utf8"

	"agentbob/contract"
)

// This file is degenerate-output detection + handling (spec §6.6) — the repetition
// wall a small/uncensored model falls into (a single char ×N, or a tiny alphabet
// looping). Three detection points converge on one handler: ① mid-stream (the
// watcher aborts the runaway), ② final-reply (covers non-streaming backends),
// ③ salvage's own reply (the salvage model can degenerate too — tested in
// 50-salvage.go). Plus history cleansing so an already-poisoned session self-heals.

// errDegenerate is the watcher's mid-stream trip. Returned plain from the turn's
// watcher; the pool wraps it in its non-retryable stream-watcher sentinel (no
// failover — another entry would loop the same way), and runRound errors.Is-checks
// it. I14: this aborts the STREAM, not the turn (the turn routes to salvage).
var errDegenerate = errors.New("turn: degenerate model output")

const (
	// degenMinRunes: shorter than this never counts (a short reply can't be a wall).
	degenMinRunes = 1500
	// degenMinDistinct: fewer distinct runes than this over a long string = a wall.
	degenMinDistinct = 15
	// degenMaxRun: a single rune repeated more than this consecutively = a wall.
	degenMaxRun = 600
	// degenCheckEvery: bytes of content growth between mid-stream re-tests.
	degenCheckEvery = 1500
	// degenDiscardedMarker replaces a degenerate assistant row in the replay slice.
	degenDiscardedMarker = "[degenerate model output discarded]"
)

// isDegenerate reports whether s is a repetition wall (spec §6.6): ≤ degenMinRunes
// runes never counts; otherwise < degenMinDistinct distinct runes OR a single-rune
// run > degenMaxRun. Single pass.
func isDegenerate(s string) bool {
	if utf8.RuneCountInString(s) <= degenMinRunes {
		return false
	}
	distinct := make(map[rune]bool, 64)
	var prev rune
	run, longest := 0, 0
	for i, r := range s {
		distinct[r] = true
		if i > 0 && r == prev {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
		prev = r
	}
	return len(distinct) < degenMinDistinct || longest > degenMaxRun
}

// handleDegenerate discards the round (I11: garbage never persisted or delivered),
// marks the exit, and returns Continue so the driver's loop-top sees the exit and
// routes to salvage (whose degenerate path tries the expert tier).
func (c *core) handleDegenerate(spec contract.TurnSpec, st *turnState) roundResult {
	slog.Warn("turn: degenerate model output discarded", "sid", spec.Sid, "iter_exit", "degen-salvage")
	st.exit = &turnExit{state: exitDegenSalvage}
	return roundResult{kind: roundContinue}
}

// cleanseDegenerate replaces any degenerate assistant row's content with a marker
// IN THE REPLAY SLICE only (not the store), so a history poisoned before the guard
// shipped self-heals on every rebuild (spec §6.6 历史清洗).
func cleanseDegenerate(msgs []contract.Message) []contract.Message {
	for i := range msgs {
		if msgs[i].Role == "assistant" && isDegenerate(msgs[i].Content) {
			msgs[i].Content = degenDiscardedMarker
		}
	}
	return msgs
}
