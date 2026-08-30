// Package trunk is the architecture's thin spine: a business-agnostic,
// turn-agnostic backbone that knows only two abstractions — a Module and a
// capability interface type. It has zero knowledge of turns, messages,
// sessions, or companies.
//
// M0 provides three of the four mechanisms (docs/architecture-vision.md §2):
//
//   - Capability registry (10-registry.go): interface type → single
//     implementation. Providers register once during Start; consumers Require
//     once during their own wiring and then hold the reference and call
//     point-to-point. The trunk is the match-maker, NOT a mediator — it is not
//     on the per-call hot path.
//
//   - Module lifecycle sequencer (20-lifecycle.go): from each module's declared
//     Provides/Needs, the trunk builds a dependency graph, topo-sorts it, and
//     drives Start in order / Stop in reverse, detecting cycles and missing or
//     duplicate providers at startup.
//
//   - Housekeeping scheduler (30-housekeeping.go): the Housekeeper, a single
//     worker draining a priority queue of periodic persistence-maintenance tasks
//     (DB prunes, blob/disk sweeps). Modules join it lazily with
//     TryRequire[Housekeeper] + hk.Register(Task{...}) rather than each spinning
//     its own timer, so storage-heavy sweeps run in one coordinated place.
//
// Deferred to a later milestone: the asynchronous signal bus (§2 mechanism 2).
// The synchronous turn lifecycle hook chain is NOT a trunk concern — it is owned
// by the turn/flow modules.
package trunk
