# trunk: the thin spine

`trunk` is the process's single assembly point. It knows exactly two abstractions — a `Module` and a capability interface type — and nothing about messages, sessions, turns, or companies.

---

## Where it sits

The trunk sits above [contract](contract.md) and [heartwood](heartwood.md), below every `leaf/` module and `flow/` flow. It is the **only** place allowed to know which modules exist; the modules themselves have never heard of each other.

Everything above follows from that:

- A module cannot import another module. To use someone else's capability it takes an interface from `contract` and trades it to the trunk for an implementation.
- A module never needs to know who uses it. It declares "I provide X, I need Y"; ordering, verification, and teardown are the trunk's job.
- The trunk imports no module of its own. Its entire import list is standard library — `reflect`, `sync`, `context`, `log/slog`.

"Module" is not a directory position; it is "implements `trunk.Module` and got registered". Behavioural subsystems under `leaf/` are modules, flows under `flow/` are modules, and so are the process clock, the attachment sweeper, and the prompt factory under `heartwood/` — they are direct-import primitives that also need a lifecycle. `sidecars/` are emphatically not modules: they run in other processes, reachable only over HTTP.

---

## What it does

The trunk answers three faces of one question: **how does a pile of mutually unacquainted subsystems get assembled, run, and torn down correctly inside one process?**

| Mechanism | Answers |
|---|---|
| Capability registry | How a consumer obtains a provider without importing it |
| Lifecycle sequencer | What starts before what, and how missing pieces and cycles blow up at boot |
| Housekeeping scheduler | How a dozen modules' periodic maintenance jobs avoid each spinning its own timer |

The design reserves a fourth mechanism — an asynchronous signal bus — that is not implemented. The synchronous **turn-lifecycle hook chain is deliberately not a trunk concern**: it is an orchestration problem owned by turn and flow, and putting it here would teach the spine what a turn is.

---

## Internals

Here is where the three mechanisms fall across one process lifetime:

```
trunk.New()          build the registry; build the housekeeper and register it at once
  ↓
Register(m) × N      take the modules in; order matters only as a later tiebreak
  ↓
Start(ctx)
  ├─ topo sort       build graph → check duplicate/missing/self/cycle → order
  ├─ per module      verify Needs are present → Start → verify Provides landed
  └─ housekeeper.start()   every task is enrolled; only now drain the queue
  ↓
(runtime)            modules call each other point-to-point; the trunk is absent
  ↓
Stop(ctx)
  ├─ housekeeper.stop()    stop maintenance first, so no sweep hits a closing store
  └─ Stop in reverse       errors joined, never bailing early
```

### Capability registry

The registry is a `map[reflect.Type]any` keyed by **the interface type itself**:

```
TypeOf[contract.ModelPool]()   → reflect.Type, what modules declare in Provides/Needs
Provide[contract.ModelPool](reg, impl)
Require[contract.ModelPool](reg) / TryRequire[contract.ModelPool](reg)
```

Four hard rules, all enforced by panicking at boot rather than by an error a caller can ignore:

- **T must be an interface.** Registering a concrete type would couple every consumer to the implementation.
- **No nil implementation** (typed-nil included). A forgot-to-construct wiring bug must blow up at the `Provide` call site, not hundreds of lines later on some consumer's first method call.
- **One interface, one implementation.** A duplicate registration panics.
- **`Require` panics when nothing is registered.** A missing hard dependency is an assembly error, not a runtime condition. "Genuinely optional" has its own spelling: `TryRequire`, which returns `(T, bool)`.

The one-to-one rule is deliberate. When an interface naturally has many implementations — chat sources, tools — that is not a trunk capability but a **plugin set owned by a parent module**: `Gateway` owns N `Source`s, `ToolCatalog` owns N `Tool`s. The parent handles its own polymorphism; the registry holds only the single entry point that parent provides. That rule is what keeps resolution logic ("pick the implementation named…") permanently out of the registry.

### Lifecycle sequencer

A module exposes six methods, the first four of which are entirely content-free to the trunk:

```
Name()      identity
Provides()  pure declaration: which capabilities Start will register
Needs()     pure declaration: which capabilities must exist before Start
Optional()  whether a Start failure is tolerable
Start(ctx, reg)   acquire resources, launch goroutines, register Provides
Health()    observation point (New / Ready / Failed / Stopped)
Stop(ctx)   release resources
```

`Register` order is irrelevant to dependency correctness — at `Start` the trunk builds a graph from `Provides` / `Needs` and topologically sorts it (Kahn). The sort itself reports four classes of assembly error on the spot: **duplicate provider, missing provider, self-dependency, and cycles** (a cycle names every stuck module). The ready queue is FIFO, so registration order survives within a layer and startup order is deterministic.

Each module then passes a gauntlet, with an explicit failure matrix:

| Situation | Critical module | Optional module |
|---|---|---|
| A declared `Need` is unregistered at start time (its provider was a skipped optional module) | unwind and abort boot | skip it, log a warning |
| `Start` returns an error | unwind and abort boot | skip it, continue degraded |
| `Start` returns an error **after** it already registered one of its `Provides` | unwind and abort | **escalated to a boot abort** |
| `Start` succeeds but a declared `Provides` was never registered | unwind and abort | warn; consumers degrade |

Row three is the easy one to miss. The `Start` contract is "register your Provides **last** — after every fallible step". An optional module that fails *after* publishing a capability has left a half-initialized implementation in the registry: downstream hard `Require`s will wire to it, housekeeping tasks will fire against it, and it will never receive `Stop` (only successful starts join the unwind list). That contract used to live in a comment; the sequencer now enforces it.

Row four catches a different lie. A module declared a capability and did not register it — yet the topo sort already trusted that declaration to schedule consumers after it. Failing here, with an error naming the module, beats a `Require` panic thrown deep inside some other module's `Start`.

`Stop` runs the other way: halt housekeeping first (so no sweep hits a closing module's store), then `Stop` in reverse start order, joining errors with `errors.Join` instead of bailing on the first. A startup abort unwinds the same way, best-effort.

`Health()` is the trunk's only observation point, returning one of four states: `StateNew` (constructed, not started), `StateReady`, `StateFailed` (Start failed, or the module found itself unhealthy), `StateStopped`. The trunk currently **observes without judging** — a module reporting Failed triggers nothing. That is a deliberately reserved seam: the circuit breaker is a later mechanism in the design, and this is its input. Keeping the state machine inside the module (only the module knows when it is unhealthy) while leaving "what to do about unhealthy" to a future policy layer is the entire intent of the method.

### Housekeeping scheduler

The scheduler targets one specific class of work: **periodic, idempotent persistence maintenance**. Roughly twenty such tasks exist, all the same shape: session's stale-pending cleanup, message-index prune, cold-session archive and archive purge, and compacted-row prune; the model pool's usage flush and usage prune; the accounts usage flush; prunes for the admin channel, URL memory, the arrangement table, and the retrieval outbox; the authorization module's exec-home sweep; the tool layer's sandbox sweep and in-flight image recovery; and the attachment store's periodic sweep.

If each module spun its own `time.Ticker`, a dozen timers would hit the same database and the same disk at unpredictable, occasionally simultaneous moments. The scheduler folds them into **one worker draining a priority queue**:

- `Task{Name, Period, Priority, Run}`. `Run` must be idempotent — the contract is explicit that a skipped cycle or two back-to-back runs are both harmless.
- Each `collect` pass takes every due task and **advances its `nextDue` immediately**, so a task is never queued twice while the first copy is still waiting.
- The due batch sorts by `Priority` descending (name breaks ties, for determinism) and the single worker runs it serially — two storage-heavy sweeps never hammer the store in the same instant.
- A task that panics or errors takes down neither the scheduler nor its siblings: panics are isolated with `recover`, errors are logged.

Three less obvious details:

**The first run does not wait a full Period.** `nextDue` lives in memory only and resets on every restart. If the first run were scheduled one full Period out, any task whose Period exceeds the deployment's restart cadence would starve forever — a weekly restart resets a seven-day prune's clock every week, and it never fires. So the first run is `min(Period, a short boot grace)`: tasks are idempotent anyway, so one extra early pass costs nothing, while the grace keeps boot itself free of maintenance I/O.

**Failing tasks back off exponentially.** Consecutive failures double the interval (Period, 2×, 4×, …) up to a cap; one success resets it. A broken sweep therefore stops burning a run plus a log line every Period forever. Backoff never schedules *sooner* than the task's own Period, and tasks whose Period already exceeds the cap are unaffected. At shutdown `stop()` cancels the context, and a well-behaved in-flight `Run` returns `context.Canceled` — recognised as an orderly stop, not counted as a failure, and not backed off.

**The Housekeeper is not a `Need`.** The trunk registers it into the registry inside `New()`, before any module starts; modules resolve it lazily with `TryRequire[trunk.Housekeeper]`. That is deliberate: as a hard `Need` it would add an edge from every storage-owning module to the spine, smearing the dependency graph while expressing no real ordering constraint. The price is invisibility to the module connection graph — covered by the soft-edge ledger described below.

---

## Design rationale

### A match-maker, not a mediator

The registry does discovery only: a provider registers once, a consumer looks up once during its own wiring, and from then on **holds the reference and calls directly**. The trunk is not on the per-call hot path.

This is not primarily a performance argument, though it does delete a map lookup and a dynamic dispatch per call. The real payoff is that **the spine has no opportunity to grow business logic**: it never sees a call, so it cannot do anything to a call — no interception, no routing, no "while we're here, let's log it". A mediator-shaped bus becomes an omniscient god object within months; a match-maker registry cannot. A side benefit is honest stack traces: a panic points straight at the real provider, with no framework layer in between.

### Topo sort holds hard edges; registration order holds soft ones

`Needs` are hard edges: in the graph, enforced by the sequencer. `TryRequire` produces soft edges that are **not in the graph** — they happen inside `Start`, where reflection cannot see them.

So a real class of ordering constraint exists that the spine cannot express: a module soft-resolves another's capability inside its own `Start` and uses it immediately. Such ordering rests entirely on registration order at the entry point (the topo sort's FIFO tiebreak). Three of these exist, and each is pinned directly against the source by a test under `arch/`: the skill catalog must register before the authorization module (otherwise its at-start grant reconcile runs against an empty catalog and denies every skill), the database pool before the process clock (otherwise the clock's soft lookup misses and falls back to the host clock, decalibrating every timestamp for the run), and the learning engine before its three consumers (otherwise their registrations silently no-op and learning is off for the run).

Why not promote them to hard `Needs`? Because hard edges have collateral damage. If the authorization module hard-needed the skill catalog, a failed (optional) skills module would cause the (optional) authorization module to be skipped entirely — and an absent authorization module means tool grants are wide open. The soft-edge failure mode is "the reconcile ran late, so skill grants deny", which fails closed. **The right shape for a dependency edge depends on which side's failure is safer when the edge breaks.**

### The rules are welded into tests

All of the above — the module connection graph, the soft-edge ledger, dynamically registered providers, the three load-bearing start orders — live as machine guards in the `arch/` package, not as gentlemen's agreements in prose. Change the graph and the test goes red, and the red *is* the review gate: a new inter-module connection must be approved in the ledger before it ships. See [infra.md](infra.md).

**Dynamic provision** deserves its own note. A capability may be registered conditionally inside `Start` without appearing in the `Provides()` declaration — browser takeover works exactly this way, published only when a browser backend is configured. Such edges are entirely invisible to a reflection-based graph, so a second ledger, scanned from the real `trunk.Provide[...]` call sites, catches them. The same scan closes another blind spot for free: a capability that is **provided but never consumed** is a dead wire, and now fails the build.

### The critical-to-optional ratio

Of thirty registered nodes, only thirteen are critical: the database pool, the admission gate, the slash-command table, session, the prompt factory, the turn core, the model pool, the webui, the gateway, the token facility, plus the flow router, the default flow, and the inbound flow. The other seventeen — tools, skills, authorization, the organisation layer, arrangement, learning, retrieval, URL memory, transcription, the outward API gateway, credentials, the admin channel, accounts, the clock, the attachment sweeper — are optional.

That ratio is itself a design position: **a missing piece should make the system do one thing less, not fail to boot.** Every optional capability's interface documents what a consumer does without it — an absent tool catalog means those tools never appear in the prompt; an absent consumption reporter means spend simply is not booked; an absent retrieval feed means the flow skips the write with zero behaviour change. And an absent API-key verifier means **every request 401s**, because that edge's safe shape is fail-closed. These are not implementation details; they are part of the contract, written on the interfaces described in [contract.md](contract.md).
