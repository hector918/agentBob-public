// Package asr is the voice-preprocess module: it provides contract.Transcriber, turning
// an inbound event's voice/audio attachments into text at ingestion (session submit,
// F165) via the model pool's KindASR backend (ffmpeg → flac → ASR). Spoken content then
// lives in the message text for every downstream reader (prompt, stored history, recall
// feed) as ordinary text, tagged only so the model knows it came from a voice note.
//
// It couples to the session core through the trunk (contract.Transcriber), not a direct
// import — session TryRequires it optionally, and a deployment with no ASR backend simply
// never folds a transcript.
package asr

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/trunk"
)

// Module provides contract.Transcriber over the model pool's KindASR backend.
type Module struct{ state atomic.Int32 }

// New returns an asr module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "asr" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.Transcriber]()}
}

// Needs nothing hard: the model pool is an OPTIONAL collaborator resolved lazily (a
// no-ASR-backend deployment still provides a Transcriber that returns "").
func (m *Module) Needs() []reflect.Type { return nil }

// Optional is true: consumers hold contract.Transcriber optionally (a nil Transcriber is
// a no-op), so a boot failure here degrades rather than aborts.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	// Resolve the pool LAZILY on first transcription — the trunk doesn't order the model
	// module before us, but by the first inbound it has registered.
	var once sync.Once
	var pool contract.ModelPool
	getPool := func() contract.ModelPool {
		once.Do(func() { pool, _ = trunk.TryRequire[contract.ModelPool](reg) })
		return pool
	}
	trunk.Provide[contract.Transcriber](reg, transcriber{pool: getPool})
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// transcriber is the contract.Transcriber impl — a thin adapter over Transcribe with a
// lazily-resolved pool.
type transcriber struct{ pool func() contract.ModelPool }

func (t transcriber) Transcribe(ctx context.Context, ev contract.MessageEvent) (string, string) {
	return Transcribe(ctx, t.pool(), ev)
}

var (
	_ trunk.Module         = (*Module)(nil)
	_ contract.Transcriber = transcriber{}
)
