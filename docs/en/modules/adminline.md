# adminline

The admin line: the system-level "summon the operator" funnel. Anything a subsystem must tell a human about — an ingress source going dark, a model-pool entry declared dead, a backend refusing everything — leaves through this one entry point.

It guarantees two things at once: **an alert is never lost**, and **an alert never floods**. Two independent mechanisms carry those two guarantees.

---

## Where it sits

**Provides**

| Capability | Who uses it |
|---|---|
| `contract.AdminLine` | [model](model.md) (pool health, an entry cooling or declared dead), [stoma](stoma.md) (source health), and any subsystem that needs to reach a human |

**Needs**

- `contract.DB` — see [pgpool](small-modules.md#pgpool). The alert audit stream is persisted.
- `contract.Gateway` — see [stoma](stoma.md). Used solely to **resolve a source by name**; the human outlet forwards through it.

The module is optional. That does not mean "nice to have" — it is a requirement placed on consumers: **every caller holds `contract.AdminLine` as an optional collaborator**, and a nil interface makes `Notify` a no-op. So a failure to start adminline costs the system its alerting and nothing else.

Callers do not import this package. They take the reference from the trunk and call point-to-point; `Notify` has a variadic format signature, so a call site costs about as much to write as a log line. That is deliberate — an expensive alerting API makes people quietly decide not to shout in exactly the places they should.

---

## What it does

**A non-blocking entry point.** `Notify` renders the text, builds the alert, pushes it onto a buffered queue, and returns. A full queue does **not** drop: the alert is persisted synchronously on the caller's own goroutine instead, under a noticeably shorter timeout — it is holding somebody else's thread, and a storm must not drag down the source-health checks that are raising it.

**The never-lose invariant rests on persistence alone.** It is independent of whether forwarding succeeded, and of whether an outlet was ever configured. That separation is what makes every failure path downstream simple: the forwarding side is free to fail.

**An append-only audit stream.** Every alert becomes a row: timestamp, level (info / warn / error), the calling subsystem's short origin tag, the text, and a forward state.

| Forward state | Meaning |
|---|---|
| never attached | no outlet was configured when it was persisted |
| sent | it reached the attached target |
| folded | throttled, rolled into a summary |
| failed | a target existed, but the send errored |

The last two are kept distinct from the first because "no outlet configured" and "an outlet configured and the send blew up" are entirely different conclusions when debugging.

**Fixed-window throttling.** Buckets are keyed by **(origin, level)** — errors and warnings from one origin get separate budgets, so one class flooding cannot drown the other. The first few alerts in a window are forwarded individually; anything over budget is not sent but added to that bucket's folded count, with its own row marked folded.

**Rollups.** When the window rolls over, a synthetic line goes out: *N more alerts of this level from this origin were folded*. It is emitted under a dedicated origin tag — it is a delivery, not a counted alert, so it never feeds the throttle of the bucket it summarises; otherwise the rollup would eventually throttle itself.

**Two outlets.** The outlet comes from startup configuration:

- **Human outlet** — the alert is pushed as a plain outbound message into a designated chat on a designated source. For a person to read, with no agent turn behind it.
- **Unattached** — persist only, never forward.

A bad outlet (unparseable, or naming a source that is not enabled) **does not block boot**: it is logged, degraded to unattached, and alerts keep landing in the stream.

**Retention sweep.** The stream would grow without bound, so the module registers a periodic prune task on the trunk's housekeeper that deletes rows past the retention age. It is persistence maintenance, so it rides the shared scheduler rather than a module-local timer.

---

## Internal structure

### The funnel body (`10-line.go`)

`Line` implements `contract.AdminLine` and owns the store, the deliverer, the throttle knobs and the queue. The split between `Notify` and `Run` is the module's skeleton: `Notify` only renders, enqueues, and falls back to a synchronous persist; `Run` drains on its own goroutine.

`Line` is typed-nil safe — what a consumer holds may well be an empty reference, and this channel's call sites usually sit inside another module's error-handling path, which is the last place that needs a second crash.

Time comes from the database-calibrated process clock (see [heartwood](../core/heartwood.md)): when several hosts write one database, alert timestamps must land on a single scale, or ordering the audit stream by time means nothing.

### The single worker and the per-alert pipeline (`15-worker.go`)

The worker goroutine is the **sole owner of the throttle state**, so the (origin, level) → window map needs no locking at all. Every alert runs a fixed pipeline:

1. **Persist** (best-effort) — a failure is only logged.
2. **Throttle** — the window arithmetic uses the line's own clock reading, not the timestamp carried on the alert (which is when the event was raised, possibly a while ago in the queue).
3. **Deliver** — the outcome is written back as sent or failed.

A failure in step 1 does **not** abort steps 2 and 3: "the store is down" is itself an event this line exists to surface. The cost is that this alert has no audit row, so the later state write-backs skip themselves.

The same file carries a periodic sweep doing two jobs: flushing folded counts stranded in windows that elapsed without a next alert to roll them over (otherwise a bucket that goes quiet never emits its last few folded alerts), and evicting idle buckets so the map does not grow with every origin tag ever seen.

### The delivery seam (`20-deliverer.go`)

`Deliverer` is a **package-local** interface with a single method. The real implementation is written against `contract.Source.Send`, which is how **adminline never imports a source package**. Outlet-spec parsing lives here too: turning a configuration string into "an object that can send a message" happens in one function, and a parse failure returns an error for the module layer to degrade on.

### The store subpackage (`store/`)

The alert row type, the forward-state enum, the `Store` interface and its implementation, plus its own migration and the timestamp-based prune.

These types **deliberately stay out of `contract`**, by the [contract layer's admission rule](../core/contract.md): `Line` and this implementation are their only users, and they appear in no cross-module interface signature. The module's entire outward contract is the single `contract.AdminLine` funnel.

---

## Design rationale

**Why persist before delivering.** Forwarding crosses a network: it can be slow, it can fail, there may be no outlet at all. The audit row is the only record that the event happened, so it has to land first. Symmetrically, a persist failure must not swallow the alert — that is exactly the moment somebody most needs to see it.

**Why throttling folds rather than drops.** The information in an alert storm does not grow linearly with its volume: when a backend explodes fifty times, what a human needs is "it is exploding, fifty times", not fifty identical messages. Folding preserves the frequency and flattens the forwarding cost to a constant. Every folded alert is still persisted in full — the audit stream is complete; only the push is trimmed.

**Why the throttle state takes no lock.** The worker owns it exclusively. This is not a micro-optimisation to save a mutex; it is a structural choice: collapsing "who may mutate this state" to one goroutine removes an entire class of bug — a throttle counter corrupted under concurrency — that would only ever reproduce during a storm, which is the only time this code truly runs.

**Why shutdown still drains.** On cancellation the worker does not exit immediately: it persists whatever is still buffered, then flushes the rollups still sitting on the buckets. A process shutting down is often when alerts are densest; dropping them there would erase the batch that mattered most.

**Why a bad outlet cannot block boot.** The outlet is an operational setting and typos are cheap to make. If a mistyped one could keep the process from starting, the alerting channel would have turned from a reliability facility into a new single point of failure. Degrading to unattached is the only sane failure posture: half the capability, a live system, and the degradation itself recorded in the log.

---

See also: [Architecture overview](../architecture.md) · [stoma](stoma.md) · [model](model.md) · [pgpool](small-modules.md#pgpool) · [trunk](../core/trunk.md)
