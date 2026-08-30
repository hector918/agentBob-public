# arrangement — task orchestration

A fixed engine running a fully flexible JSON orchestration: an ordered line of role-owned priority buckets, with work items moving forward, back, or into a parked state.

---

## Place in the architecture

**Provides**

| Capability | What it is |
|---|---|
| `contract.Arrangements` | The six actions: define / inject / pull / submit / cancel / status |

**Needs**

| Dependency | Why |
|---|---|
| `contract.DB` ([small-modules.md#pgpool](small-modules.md#pgpool)) | Its two self-managed tables |
| `contract.SlashRegistry` ([small-modules.md#slash](small-modules.md#slash)) | Registers `/arrangement` |
| `contract.PanelRegistry` ([webui.md](webui.md)) | Registers the arrangements and items tables |

**Soft edges** (`TryRequire`, degrade when absent): `contract.Agora` ([agora.md](agora.md) — role → member scopes, company liveness, the company's bridge address), `contract.Gateway` ([stoma.md](stoma.md) — emit the nudges and the terminal product), `contract.SessionManager` ([session.md](session.md) — the idle gate and the liveness gate on lease reclaim), `trunk.Housekeeper` (retention sweeps).

The module declares `Optional() = true`. Every coupling to the org layer goes through `contract` and is resolved lazily — with no org layer the dispatch loop simply idles instead of failing to start. The `arrangement_*` tools live in the tools module ([tools.md](tools.md)) and reach this capability lazily too.

**Consumers**: members call those tools inside ordinary turns ([../flows.md](../flows.md)); the webui panel reads status.

---

## What it does

**A fixed engine plus a flexible orchestration.** The engine does exactly three things: injection (input), a ticker that nudges idle members to come and pull (dispatch), and the rule "one priority bucket per role". The variable part is the arrangement's spec — a JSON the author writes: ordered role buckets, each carrying free-form content (what that stage requires). The engine reads only the role and the ordering; the content is opaque to it, written for whoever does the work.

**Six actions.** `Define` validates and stores a frozen arrangement; `Inject` drops work items into it; `Pull` atomically claims the top item of an (arrangement, role) bucket; `Submit` returns the product and routes the item; `Cancel` cancels; `Status` produces the webui snapshot.

**Three outcomes.** `forward` advances to the next bucket (or completes if this was the terminal one); `back` returns to the previous bucket (or parks as "no upstream" if there is none); `park` leaves the active flow with a descriptive status such as `blocked`.

**Cross-company authoring is intentional.** An author may define an arrangement for any company — the gate is the tool grant itself, not company membership.

**Frozen once started.** A running arrangement cannot be edited; cancel and define a new one. This is not laziness: a half-edited arrangement has no sensible meaning for the items already in flight under the old definition.

---

## Internal structure

### The spec: the one flexible surface

`10-spec.go` parses and validates the arrangement JSON. Validation is **structural only**: at least one bucket, each with a role, roles distinct. Whether those roles are **staffed** is not checked here — an unstaffed role's items park as `unmet` at dispatch time, which is a runtime state, not a definition error.

Above the spec sit only a handful of pure functions: the entry role, a role's previous and next role, a role's authored content, and the ordered role list. Everything the engine needs to know about "what this arrangement is" is those functions.

### Two tables and the atomic claim

`leaf/arrangement/store` has exactly two tables: definitions (one row per arrangement, spec frozen once started) and items (the live queue, where `role` is the bucket the item currently sits in).

The item status semantics carry a lot of weight: the engine acts **only** on `queued` (dispatchable, claimable) and `in_flight` (awaiting submit); **every other status string is a parked item** — displayed, but not driven. That is why the park status is free-form: adding a new reason for being stuck costs zero engine changes.

Correspondingly, submit carries a **reserved-word gate** refusing a park status that collides with the engine's own. It blocks specific bad outcomes: `queued` would create a park→pull loop; `in_flight` would occupy a concurrency slot forever, neither dispatched nor lease-reclaimed; `done` would fire a false terminal delivery; `cancelled` would be swept away silently; `unmet` would be re-queued by the self-heal pass.

Claiming is a **single-row conditional UPDATE**: pick the top candidate by priority (oldest first within a priority), then flip it to `in_flight` under a `status='queued'` condition. A lost race just tries the next candidate. Submitting is likewise conditional, guarded on "the claimer is the caller **and** the status is still `in_flight`" — so a non-holder submit, a duplicate submit, and a late submit after a cancel all touch zero rows and return an explicit failure.

**There are no transactions here, only conditional writes.** Two members woken at once still cannot claim the same item.

### Dispatch: selector and drainer

Two goroutines on one cancellable background context (the one handed to `Start` is transient and unusable for this), both cancelled and waited on in `Stop`.

**The selector** (30 seconds by default) only selects; it never emits. Each round does four things:

1. **Reclaim expired leases.** An item held `in_flight` past the lease is a candidate — **but only if its holder is not currently running a turn.** Turns have no wall-clock cap, and reclaiming from a slow-but-alive member would re-run side effects it already performed. A process restart clears the "who is running" record, so post-restart orphans naturally become eligible.
2. **Filter to dispatchable arrangements**: started, and belonging to an **active** company. A disabled or paused company's buckets are not dispatched — its items stay queued in place and resume when the company comes back, rather than being parked as "nobody to serve them".
3. **Scan the buckets.** One `GROUP BY` finds every non-empty queued bucket — one database round trip per round, not one per bucket. For each bucket it asks whether the role can actually drain it: live members *and* the collaborator capability (pull + submit). Either missing parks the whole bucket as `unmet` with the reason recorded. Otherwise it enqueues a wake for every **idle** member scope (a member already running a turn is skipped — that is the idle gate).
4. **Self-heal.** An unmet bucket whose role has regained members and capability has its items flipped back to queued, and the next round picks them up. This covers transient gaps: a re-staffed role, a re-activated member inbox.

**The drainer** (8 seconds by default) pops exactly one wake per interval and emits it. Selecting and emitting are split so that one selector round cannot ignite a burst of concurrent member turns. Before emitting it re-checks three things: has the member gone busy, has the bucket been drained by a colleague meanwhile, has the company just been paused. Any of those and the wake is dropped (the selector re-enqueues it if still due). A failure to read the definition also drops the wake rather than emitting one with empty content.

The drainer **never marks an item**. The flip to `in_flight` happens strictly when the member pulls, so the table never shows "picked up by someone" for an item nobody is actually holding.

The wake queue has two layers of de-duplication:

- The **pending set** dedups by (arrangement, role, scope) — not by scope alone. Deduping by scope alone would let one bucket's pending wake shadow the same member's wake for a *different* bucket.
- A **per-scope cooldown**. It exists because there is an asynchronous window between emitting the message and that turn registering as busy, during which the member still looks idle; without the cooldown they get woken twice into two concurrent pull turns. Both the enqueue and the dequeue path check it, because wakes for distinct buckets can legitimately coexist in the queue.

The wake message itself asks for a fresh session and expects no reply — it is a fire-and-forget nudge, and the member calls the pull and submit tools inside that turn. With no reply target the turn must still genuinely run, rather than being dropped the way a routing failure would be.

### Submit and terminal delivery

`Submit` (`40-impl.go`) builds the item's **accumulating payload** as it routes: each stage's product and each rejection reason is appended as a labelled section. So a downstream role — including one dedicated to verification — sees the original task plus the whole trail, not just the previous step's output.

When the terminal bucket completes, the product is delivered to the owning **company's bridge**, not to the arrangement's author — because the commander may itself be an agent, and the bridge is that company's stable outlet to a human.

A failed delivery **does not close the arrangement**. Closing it would quietly declare success with the report lost; leaving a started arrangement with no live items visible on the panel is honest and retryable.

Closing has a second condition too: only when **no live items remain**. Injection can fan one arrangement into several concurrent items, so a single terminal completion is not the end of the whole thing. The close itself is guarded on `status='started'`, so a concurrent cancel is never clobbered.

### Two permission doors and a concurrency cap

Whether a role can process a bucket comes down to holding both pull and submit. That check appears in three places, all fail-closed (an unavailable org layer means it cannot be verified, so nothing is opened):

- **At define time**, per bucket. A role that cannot process its own bucket is rejected on the spot, rather than opening an arrangement doomed to wedge.
- **At inject time**, against the target stage — the second door.
- **At runtime**, every selector round. This is the patch on the create-time door: a grant revoked *after* the definition leaves items unpullable, which is precisely the runaway root cause. The runtime check parks such a bucket as unmet, and self-heal picks it back up when the grant returns.

`Define` also enforces a per-company **cap on running arrangements**. The count includes only arrangements holding a runnable (queued or in-flight) item, not every started one: an arrangement left with nothing but parked items consumes no member capacity and never auto-completes, so counting it would eventually wedge new definitions permanently. There is no lock between count and insert, so two simultaneous defines can exceed the cap by one — acceptable for a low-frequency admin action.

Definition also kicks off immediately: on success one work item is dropped into the entry bucket, because the stage's content *is* the instruction. That step is **atomic** with the definition — if the kickoff insert fails, the definition is rolled back. Otherwise a zero-item ghost arrangement lingers forever: never running, never completing, and never pruned, since pruning only touches terminal statuses.

### Sweeps and the operator surface

Three sweeps run on the central housekeeper (the module spins no timer of its own — persistent-state sweeps are coordinated in one place):

- Terminal **items** are deleted past their retention.
- Terminal **arrangements** (cancelled or done) are deleted past theirs, taking their items with them.
- The same pass first closes **stalled** arrangements: started, holding no runnable item, and inactive for a long time — nothing left but parked items. Such arrangements neither complete on their own nor get remembered and cancelled. Closing them puts them onto the normal terminal retention path.

The `/arrangement` command (admin) adjusts the selector cadence, the drain interval and the concurrency cap at runtime, and can cancel or show status. The webui panel renders two tables: arrangements (each row expandable to its per-stage content) and live/parked items, longest-held first — that number is the headline worth seeing. Every panel action prefills a command for the operator to confirm rather than executing directly.

---

## Rationale

**Fixed engine, flexible orchestration.** Swapping in a different business process should mean writing a different JSON, not editing the engine. So the engine insists on knowing only "ordered role buckets" and parses not one word of the content — the moment it starts parsing, that text becomes an engine input format and the flexibility is gone.

**Status is an open set.** Only two statuses drive the engine; everything else is a display-only parked state. Adding a new way to be stuck therefore costs no code, at the price of one reserved-word gate keeping free text out of the engine's own vocabulary.

**Conditional writes, not transactions, for claiming.** Two tables and a single-row guard are enough to guarantee that a piece of work is taken by exactly one person and settled exactly once. This mirrors the project's standing preference: at a narrow concurrency point, a guarded single-row UPDATE is easier to read and harder to get wrong than a transaction.

**At-least-once, not exactly-once.** Gating lease reclaim on "is the holder still running" removes the common false reclaim, but a member that performs an external side effect and then dies before submitting really will have its item re-run. That semantics is accepted explicitly — member-side work should be idempotent.

**Selection separated from emission.** The selector decides *who* should be woken; the drainer decides *when*. Fused together it would be shorter, but one selection round would ignite a batch of concurrent member turns at once. Split, rate limiting is a single knob that does not touch the selection logic.

**A pause must actually stop things.** The company pause/disable gate is re-checked at dispatch, at drain, and at pull. Checking only at dispatch would make it advisory — a wake enqueued just before the pause would still let a member pull, act and submit.
