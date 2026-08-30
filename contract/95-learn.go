package contract

import (
	"context"
	"time"
)

// This file is the learn seam: one generic text-space optimizer (leaf/learn)
// improves N "trainable text" targets — a tool's description tail, a skill doc, a
// company role's playbook — from observed run traces. Each owning module exposes
// its targets through a LearnSource; the engine owns the model call and the
// anti-rot policy. See docs/learn.md (参考 microsoft/SkillOpt).

// Rollout is one observed run datum — the raw learning material a Learnable hands
// the engine. Each target supplies its own failures from its own on-disk store: tools
// persist a shape-only record per failed call, skills/member a turn-trajectory
// snapshot. The engine reads them since a watermark, distills periodically, and the
// store sweeps the consumed rows.
type Rollout struct {
	Args string // tools: the call's argument SHAPE (key names + length, never raw values)
	// OK is a forward seam: all three production constructors set it false (failure-only
	// recording today), so failuresOnly + the len(failures)==0 branch are constant now —
	// kept for a future success-recording source (e.g. reward-shaped distill) that will
	// emit OK=true rows. Do not delete because it reads constant.
	OK    bool      // result envelope ok
	Error string    // failure message, if any
	Hint  string    // recovery hint the tool returned, if any
	At    time.Time // when it ran (Traces filters by this)
	// Snapshot is a rich, multi-line failure record — the trajectory of a failed turn
	// (user ask + action outline + degraded output) the skill distiller reflects on
	// (docs/wip-skill-learn-reflect.md). "" for the arg-shape targets (tools), whose
	// content is Args/Error/Hint; when set, the distiller renders the snapshot instead.
	Snapshot string
}

// Learnable is one trainable-text target: a piece of natural language fed to the
// frozen model that the engine improves from observed Rollouts. Current/Apply read
// and write ONLY the learned part (the tail), not the authored base it augments.
// The owning module implements this over its own storage.
type Learnable interface {
	// Key identifies the target — "tool:file_storage" / "skill:ultrasound" /
	// "role:com-one:player". Stable; used for logging and the engine's per-target
	// bookkeeping.
	Key() string
	// Current returns the target's present learned text ("" if none yet).
	Current(ctx context.Context) (string, error)
	// Apply writes a revised learned text back to the target's own storage.
	Apply(ctx context.Context, revised string) error
	// Traces returns the learning material observed strictly after since,
	// oldest-first (ascending At). The engine relies on this order: the failure
	// gate treats the first entry as the oldest, and the distiller keeps the
	// slice tail as the most recent N failures.
	Traces(ctx context.Context, since time.Time) ([]Rollout, error)
}

// LearnSource enumerates a module's current learn targets, FRESH each cycle so a
// dynamic set (per-company/role targets that come and go) is always current. Each
// owning module (tools now; skills, agora later) registers exactly ONE source.
type LearnSource interface {
	Targets(ctx context.Context) []Learnable
}

// LearnRegistry collects target sources. Provided by leaf/learn; the owning modules
// register their source at Start (TryRequire — learning off if learn is absent),
// the same optional-edge shape as the other lazy capabilities. The engine walks
// every registered source each housekeeping cycle.
type LearnRegistry interface {
	AddSource(s LearnSource)
}

// --- forward seam (skill slice) ----------------------------------------------
//
// Scorer is the OPTIONAL reward+validation half a Learnable also implements to opt
// into the replay-gated path (real SkillOpt: a candidate edit is accepted only when
// it strictly improves a held-out score). A Learnable WITHOUT a Scorer uses the
// engine's integral-rewrite guard — the whole tail regenerated each cycle, capped,
// admin-editable. The tool target does NOT implement it (low-risk, visible,
// editable); skill/role targets implement it when they land (RUBRIC judge /
// approve-reject). The engine branches on a type assertion, so adding it is
// additive — no rework of this seam, the engine's rewrite path, or the tool target.
//
// Trajectory's shape is finalized with the skill slice (it needs whatever replay
// requires); kept minimal here so the door is declared without over-committing.
type Scorer interface {
	// Validation returns held-out cases to replay a candidate text against.
	Validation(ctx context.Context) ([]Trajectory, error)
	// Score rates one replayed output (e.g. a RUBRIC judge). ok=false → unscorable.
	Score(ctx context.Context, out Trajectory) (score float64, ok bool)
}

// Trajectory is a held-out case the engine replays under a candidate text to score
// it. Opaque to the engine beyond replay+Score; shaped by the target. (Skill slice.)
type Trajectory struct {
	Sid   string
	Input string
}
