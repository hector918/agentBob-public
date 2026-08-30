package skills

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// Boot-time external-scan retry: a transient external-dir read error (volume mount race /
// NFS hiccup) is retried before falling back to the built-in set.
const (
	skillReloadRetries    = 3
	skillReloadRetryDelay = time.Second
)

// Module is the skill catalog as a trunk module: it scans the two sources at Start
// and provides contract.SkillCatalog. The flow Requires it (optionally), projects +
// authorizes per identity via warrant, and injects the result; warrant reconciles
// skill:use:<name> grant rows against the catalog. Symmetric with leaf/tools.
type Module struct {
	home  string
	cat   *FileSkills
	store *skillInsightStore // learned-notes persistence (INSIGHT.md); nil → in-memory
	fail  *skillFailureStore // failure-snapshot trace source (contract.SkillFailureSink)
	state atomic.Int32       // trunk.State
}

// New returns a skills module reading built-ins from the embed and externals from
// $home/skills/external.
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "skills" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.SkillCatalog](),
		trunk.TypeOf[contract.SkillFailureSink](), // the agora flow's failure-snapshot sink (learn)
	}
}

// Needs the webui PanelRegistry so it can register its dashboard panel at Start.
// Provided by the critical webui module (always present), so this hard Need can't
// skip-trap an Optional module. Symmetric with leaf/tools.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.PanelRegistry]()}
}

// Optional is true: without skills the flow runs with none (the model just has no
// skills to draw on).
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	m.cat = NewCatalog(m.home)
	// Reload's only error is a TRANSIENT external-dir read failure (its own doc: a volume
	// mount race / NFS hiccup / permission blip — NOT a programming bug; bad individual
	// skills are skipped, not fatal). The built-in set is compiled in (zero I/O) and always
	// loads, so an external blip must never take the whole module (and learning) down for
	// the run. Retry a few times for a mount race, then FALL OPEN to built-ins; external
	// joins on the next /skills reload.
	if err := m.cat.Reload(); err != nil {
		for i := 0; i < skillReloadRetries && err != nil; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(skillReloadRetryDelay):
			}
			err = m.cat.Reload()
		}
		if err != nil {
			// Marks the catalog Degraded() — the known-incomplete List() must not be
			// read as "those skills were removed" (warrant's reconcile checks it too).
			slog.Warn("skills: external scan still failing at boot; serving built-ins only (external joins on next /skills reload)", "err", err)
			m.cat.loadBuiltinsOnly()
		}
	}
	trunk.Provide[contract.SkillCatalog](reg, m.cat)

	// Self-describe to the webui (the catalog is loaded). Hard-Need, so the registry
	// is guaranteed present.
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())

	// Persist learned skill notes as INSIGHT.md files (docs/learn.md §6) under
	// $BOB_HOME/skills/<origin>/<name>/ so accreted experience survives a restart, and
	// load any saved notes back into the catalog. Needs a writable home; without one
	// the notes stay in-memory (the catalog is unaffected).
	if m.home != "" {
		m.store = &skillInsightStore{base: filepath.Join(m.home, "skills")}
		// On a reload that removes a skill, also delete its INSIGHT.md (best-effort) so
		// an orphan internal mirror doesn't linger (B-26).
		store := m.store
		m.cat.SetAddendumPruner(func(origin contract.SkillOrigin, name string) { _ = store.delete(origin, name) })
		// all() is per-file fail-open (unreadable files warned + skipped), so one bad
		// INSIGHT.md can't blank every skill's notes for the run.
		for k, text := range m.store.all() {
			m.cat.setAddendum(k.origin, k.name, text)
		}
	}

	// The failure-snapshot trace source: the agora flow reports a member's failed-turn
	// snapshot per engaged skill here (docs/wip-skill-learn-reflect.md); the learn sweep
	// reads them through skillTarget below. Provided UNCONDITIONALLY so the registration
	// matches the static Provides() declaration (else trunk's post-Start check WARNs a
	// declared-but-unregistered capability). Best-effort: with an empty home the base is
	// cwd-relative and writes may fail (logged) — but consumers get a live sink either way.
	m.fail = &skillFailureStore{base: filepath.Join(m.home, "skills", ".failures")}
	trunk.Provide[contract.SkillFailureSink](reg, m.fail)
	// A skill removed by a reload takes its failure snapshots (old conversation
	// transcripts) with it; the boot-time orphan sweep covers skills removed while
	// the process was down, which Reload's prev/next diff can't see. The sweep is
	// SKIPPED when the external scan fell open to built-ins — in that degraded
	// state every external skill would look like an orphan, and a transient mount
	// blip must not delete their accumulated failure evidence.
	m.cat.SetRemovedPruner(func(name string) { m.fail.removeSkill(name) })
	if !m.cat.Degraded() {
		live := map[string]bool{}
		for _, info := range m.cat.List() {
			live[safeSkillName(info.Name)] = true
		}
		if n := m.fail.sweepOrphans(live); n > 0 {
			slog.Info("skills: swept failure snapshots of removed skills", "dirs", n)
		}
	}

	// Optional: register the skill learn source (one target per skill) into
	// leaf/learn. learn is registered before skills in cmd/bob, so it has started;
	// absent → skills simply don't learn.
	if lr, ok := trunk.TryRequire[contract.LearnRegistry](reg); ok {
		lr.AddSource(skillLearnSource{cat: m.cat, store: m.store, fail: m.fail})
	}

	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
