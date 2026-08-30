# retrieval

The main process's half of cold memory: it feeds every finished turn to a separate retrieval service and, when asked, queries that service back.

The vector store, the embeddings and the query implementation all live in a sidecar. What this module owns is **two seams** — one write, one read — and the durable outbox between them. The outbox is the point of the module: it is what lets *no corpus is lost* and *no turn is slowed down* both be true at once.

**Off by default.** Until it is configured, `Start` registers nothing.

---

## Where it sits

**Provides**

| Capability | Who uses it |
|---|---|
| `contract.RetrievalClient` | the recall tool in [tools](tools.md) (read) |
| `contract.RetrievalFeed` | the [flow layer](../flows.md) (post-turn feed, write) |

**Needs**

- `contract.DB` — see [pgpool](small-modules.md#pgpool). The outbox is a real table.

Both consumers hold their capability on a **soft edge**. When the module is off, the declared Provides simply have no registration behind them — so the recall tool reports unavailable, the flow skips the enqueue, and behaviour is unchanged.

The retrieval service itself is documented under [sidecars](../sidecars/). The main process talks to it over HTTP and shares no in-process state with it; the outward read contract does not even contain the write method (see below).

---

## What it does

### Read

**One recall query.** The query carries a type (entity mentions / time window / semantic), that type's own parameters, a set of generic filters, and a result cap.

**Raw, unparsed JSON comes back.** The calling tool decides how to shape it; the module does not shape it for them, and so does not have to change as the service's response structure evolves.

**Fail-open.** An error just means the service is unreachable, the caller skips that step, and **the turn never fails because of it**.

**Scope clamping.** The identity constraints on a query (company, users, channel) are folded into the filters by the caller, and the service fail-closes on a query arriving with no company filter. It is a two-person gate: the caller clamps, the service refuses. Non-organisational (personal) traffic is filed under an agreed company sentinel into a pre-built partition.

### Write

**Feeding a turn.** `EnqueueTurn` is fire-and-forget: it persists this turn's rows into the outbox table and returns; pushing happens in the background. It may not fail and may not block.

**Identity comes from the flow.** Company and room are stamped by the flow and **never re-derived here** — only the flow knows its own scope semantics.

**Every message keeps its own speaker.** One turn in a group can contain several people talking, so each user event is enqueued under **its own** handle rather than lumped under "the user". In a group the speaker's name is also prefixed onto the content, so *who said it* is both readable and findable by literal match. The assistant's own replies go under an agreed bot handle — "the speaker is bob" should not be mistaken for a person.

**Hygiene before enqueue.** Every piece of content passes through credential redaction (secrets never enter the corpus) and is capped by length: a pasted file, or a large tool dump folded into a reply, must not drown the corpus. The cap is applied on rune boundaries — half a multi-byte character would make the service reject the entire batch.

**Deterministic dedup.** Each message id is derived from (session, event), and the service ingests idempotently on it. Re-pushing a batch is therefore safe — which is exactly what makes "on failure, leave the rows and retry" viable.

**Skipping rows with no value.**

| Case | Why it is skipped |
|---|---|
| an attachment-only message with no words | an uncaptioned image is noise in a corpus; pairing it against an answer means nothing |
| empty content | the service's embedding step rejects it — and it rejects the **whole batch** |
| a turn with no triggering anchor | a proactive / resumed / internally-nudged reply has no user question to pair with, and its derived id would repeat within the session, so dedup would silently swallow all but the first |

### Drain

**Tick and batch.** The background feeder ticks, pops the oldest batch, pushes it, deletes the accepted rows, and keeps going until the queue is empty.

**Transient errors are left to retry.** Network faults, a restarting service, a 5xx — the rows stay in the queue for the next tick.

**Content faults are quarantined.** Only a **content fault the service itself declares** counts as permanent, dead-lettering that batch. Otherwise one unpushable batch wedges the FIFO head forever. The classification is deliberately narrow: configuration-shaped faults of every kind (auth, path, size) are treated as transient and left to back off in the queue, so a misconfigured intermediary cannot silently discard append-only, un-backfillable corpus.

**A hard ceiling.** If the service stays down the outbox grows without bound, and every row carries full content. So a cap-trim task hangs off the housekeeper: past the row ceiling, the **oldest** rows are dropped. It is a safety bound, not a tuning knob — better to lose the oldest cold memory than to blow up the main database.

**Queue state where it is looked at.** The module registers a panel with [webui](webui.md) showing queue depth, **how long the head has been waiting**, and whether the drain is currently failing (with the reason verbatim when it is). Depth alone cannot tell a busy minute from a dead service: thirty rows constantly turning over is healthy; thirty rows whose head has sat for weeks is an outage.

---

## Internal structure

### The module shell (`00-module.go`)

The configured/not predicate, migration, registering the two capabilities, launching the feeder goroutine, attaching the prune task and the panel.

The feeder deliberately goes quiet in the log after its first failure — a dead service should not produce a warning every tick. The cost is that "is it moving right now" goes stale in the log quickly, so that state is kept separately **for the panel to read**. Log and panel have a deliberate division of labour here: the log records **changes** (it went down, it came back), the panel answers **the present**.

### The HTTP client (`10-client.go`)

The read and write endpoints of the retrieval service. `Retrieve` implements `contract.RetrievalClient`; `Ingest` is the write path and **deliberately stays on the concrete in-package type** rather than entering `contract` — no module outside this package should be able to write into the corpus directly, and keeping the method off the contract makes the compiler enforce that instead of a convention.

This is also the single place deciding which HTTP statuses are permanent. Read responses are deliberately **not** size-truncated: a recall response can be large, and truncating it would hand the tool malformed JSON.

### Config (`15-config.go`)

Its own section: the switch, the service address, an optional call key, timeout, tick, batch size. The batch size is hard-clamped to the service's own limit.

`Configured()` is the **single predicate** for "the feature is live" — module start, feeder, recall tool and flow enqueue all hang off the two registrations it gates. One switch, one decision point, no second source of truth.

### The outbox (`30-outbox.go`)

The table and its migration, plus the queue's four operations:

- **one multi-row insert per turn** — a turn is enqueued all-or-nothing (no half-turn corpus) in a single round trip;
- **pop the oldest batch** — read only; deletion happens after a successful push;
- **delete by id**;
- **trim to cap**.

Plus one statistics read (depth + head age) for the panel. **FIFO order comes solely from the auto-increment key**, never from a timestamp — the row's timestamp is for diagnostics only.

### Feed and drain (`40-feed.go`)

The `contract.RetrievalFeed` implementation, the logic that flattens one turn into rows (identity, speaker prefix, redaction, capping, id derivation and the skip rules all live here), and the full policy of one drain pass.

The enqueue runs on a context **detached from the turn's cancellation**: closing a session or receiving a shutdown signal cancels the turn context, but this best-effort corpus write should still land.

### The panel (`90-panel.go`)

Rendering of the three readings, including the threshold at which a head stops reading as "busy" and starts reading as "stuck". When the queue cannot be read it shows "?" rather than 0 — an unread queue must never render as an empty one. The panel is admin-only: the service address and the raw failure text are deployment detail.

---

## Design rationale

**Why an outbox in the middle.** Pushing synchronously at turn finalization would weld corpus durability onto reply latency: every second the service is slow becomes a second on every turn. Writing one local row costs milliseconds, and all the timing, retrying, batching and failure handling sinks into the background. The price is a table and a goroutine; the return is zero risk on the turn path.

**Why the read fails open and the write is fire-and-forget.** Cold memory is an **augmentation**, not a spine. A failed recall makes an answer slightly worse; a recall that hangs the turn makes there be no answer at all. Both failure postures follow from that single judgement: a read error is a skip, a write error is a warning saying *there is a corpus gap here*, and life goes on.

**Why identity comes from the flow and is not looked up here.** "Which company, which room does this message belong to" is scope semantics, and the single authority for scope lives in session resolution. If this module re-derived it, the system would hold a second answer capable of disagreeing with the authoritative one. Letting the flow stamp it and the module merely carry it is what the [single-authority](../architecture.md) principle looks like here.

**Why a poison batch is dead-lettered.** The queue is FIFO: a batch that can never be pushed blocks everything behind it forever, so one bad row would stop the whole corpus from updating. Identifying a genuine content fault and dropping that batch is the price of keeping the queue **moving**. The trade-off is real — the service rejects a whole request over a single bad message, so one poison row takes its batch-mates with it — which is why the drop is logged loudly, and why the most common cause (empty content) is filtered client-side before it ever gets there.

**Why the panel shows head age.** This is the one field in the module that grew out of an incident. A service that stays unavailable leaves exactly one warning in the log per restart; the row count looks like it always has "some" rows, and an observer watching depth alone reads nothing wrong. Depth *plus* head age is what makes the signal decidable: **is the queue moving, or piling up.**

---

See also: [Architecture overview](../architecture.md) · [Flows](../flows.md) · [tools](tools.md) · [webui](webui.md) · [sidecars](../sidecars/) · [pgpool](small-modules.md#pgpool)
