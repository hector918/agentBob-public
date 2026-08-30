# chat-retrieval sidecar (cold memory)

Long-term retrieval over the conversation corpus: messages stored in a vector-enabled Postgres partitioned by company, queried three ways — by entity, by time window, by semantics — returning **structured results**.
It never calls an LLM itself.

---

## Where it sits

A Python service (FastAPI + asyncpg) in its own container with its own Postgres; the embedding and rerank models are in turn its own downstream HTTP services, deployable on whatever machine has the compute. The main process talks to it over HTTP only.

The main-process side is the `leaf/retrieval/` module, which registers two capabilities on the trunk:

| Capability | Direction | Consumer |
|---|---|---|
| `contract.RetrievalFeed` | write | the flow calls `EnqueueTurn` as a turn closes |
| `contract.RetrievalClient` | read | the model-facing `recall_memory` tool (`leaf/tools/58-recall.go`) |

**Off by default.** `Start` registers nothing until both `retrieval.enabled` and a `base_url` are set; consumers degrade accordingly — `recall_memory` reports itself unavailable, the flow skips the enqueue. With an api key set, every request carries `X-API-Key`.

### The write path is a durable outbox, not a direct call

At the end of a turn, `40-feed.go` persists the turn's user events and assistant reply as rows in `bob_retrieval_outbox`, and a drain goroutine pushes them in batches on a tick.

- Each user event keeps its **own** speaker handle, so attribution in a group does not smear;
- corpus writing is a best-effort side channel: an enqueue failure logs and **never fails the turn**;
- the write uses a context detached from turn cancellation — closing a session and shutting down both cancel the turn context, but a corpus row that has already been computed should still land.

Several things about that outbox are design, not implementation detail:

**Only a 2xx deletes rows.** The sidecar's write endpoint is synchronous — embedding and insertion both succeed before it answers 200. It cannot answer 202, because the drainer treats a 2xx as "durably stored" and drops its own row; a background failure after a 202 is permanent, silent data loss with no retry.

**A poison batch is dead-lettered.** Content-class 4xx is recognised as a permanent rejection and the head batch is dropped, so one bad batch cannot wedge a FIFO queue forever.

**There is a hard ceiling.** With the sidecar down for a long time the outbox grows unbounded (every row carries full content), so a sweep trims to a row cap, dropping the **oldest** first. Losing the oldest cold memory beats blowing up the main database.

**Redaction happens before storage.** Text goes through `heartwood/scrub` and is length-capped. The corpus is append-only — nothing is deleted or edited, retractions and edits do not roll back — so a credential that lands there cannot be taken out again.

### Observability

`leaf/retrieval/90-panel.go` surfaces queue depth, head age and current drain state on an admin-only webui panel (the leaf address and failure text are deployment detail).

This is not decoration. The drainer logs one WARN on the up→down edge and then goes quiet, so a long outage leaves one line per restart and a slowly growing row count as its only trace. **Putting the state where someone looks beats putting it where it is logged** — which is also why an unreadable stat renders as `?` and not `0`: a queue you failed to read must never look like an empty one.

---

## What it does

### Ingestion

`POST /messages` and `/messages/batch` (a single message is just N=1; batches have a size cap). Each message carries:

- company (the partition key), session, channel / worker (routing facts under multi-member orchestration);
- the speaker's **immutable handle** — an account is mutable (claimable, mergeable), so the corpus stores only handles and the main process expands an account into a handle list at query time;
- **two timestamps**: the source's declared time (forgeable by a client) and the caller's authoritative receipt time;
- role, body, optional language tag;
- optional derived tags (sentiment / commitment-shaped / short intent) and free-form metadata.

An empty body is rejected at the door (422): an empty string makes the whole batch's embedding call fail, and retrying that same batch fails forever — a poison pill the caller must dead-letter rather than retry.

### Retrieval

One entrypoint, `POST /retrieve`, dispatched on `query_type`:

| query_type | The question it answers | How |
|---|---|---|
| `entity_mentions` | what a person / company / project said, and who mentioned them | three recall paths — author match, literal fuzzy match, vector semantics — deduplicated by message, then reranked |
| `time_window` | what happened during a period | representative messages for the window (a semantic slice when a query is given, the most recent N otherwise) plus aggregates: volume, distinct users, per-day distribution, most active users |
| `semantic` | general semantic fallback | vector recall plus rerank |

Filters are a shared set: company, user, worker, channel, session, language, role, derived tags, time range, user attributes.

**A malformed query is always a 400, never a 500.** A mode that does not match its parameters (asking for author matching without an id, say) is refused loudly rather than silently skipping all three branches and returning empty — the caller's LLM cannot tell "no data" from "that query was a no-op". By the same token a 400 tells the caller to fix the query, while a 500 would trigger its service-fault handling (fail-open, retry), which is the wrong response.

**Structured data out; the narrative is the caller's job.** Topic modelling and period narration used to live here and were removed wholesale: the main process takes the `time_window` / `entity_mentions` results and has its own LLM synthesise on the spot — better output, and no offline pipeline to operate.

### Liveness and self-check

`/health` confirms process *and* database. A dead database answers 503 + degraded so a probe can distinguish "process up, storage down". It uses the cheapest possible query rather than a count — health checks are frequent, and a full count on a large partitioned table gets slower over time while periodically hammering the database.

`/selftest` is an **interface-conformance check**: one request verifies four things at once —

1. database connectivity;
2. a new enough vector extension (older ones lack iterative scan, and filtered recall suffers);
3. **whether the embedding dimensionality matches the schema**;
4. rerank reachability.

The third is the item most likely to make a first deployment fail wholesale, since a mismatch makes every single INSERT fail. It also distinguishes two kinds of mismatch: a server returning **more** dimensions than expected is acceptable (a truncatable model family), while **fewer** means the wrong model is configured and is a hard failure — better than writing garbage.

The point of this endpoint is to **replace a hand-run pre-deploy checklist**. Checklists get skipped; an endpoint that answers 503 does not.

---

## Internal structure

```
main.py               FastAPI entrypoint: lifespan, auth dependency, /health, /selftest
config.py             all configuration read from environment variables
clients/db.py         asyncpg pool, partition management, vector formatting
clients/embedder.py   embedding-service client (OpenAI-compatible protocol)
clients/reranker.py   rerank-service client
ingestion/handler.py  write endpoints: batch embed → ensure partition → batched INSERT in one transaction
retrieval/api.py      /retrieve routing and fail-closed validation
retrieval/*.py        the three query_type handlers plus filters→SQL
schema.sql            partitioned table + indexes
```

### Partitioning is isolation

`messages` is LIST-partitioned by company. That single decision solves two problems at once:

- **Recall correctness** — "moderately selective metadata filter over a global vector index" is a classic under-recall trap (take global nearest neighbours, then filter, and little survives). A query carrying the company filter is pruned to a single partition's index, and recall is finally correct.
- **Cross-company isolation becomes structural** — a query physically only enters its own company's partition.

Ordinary indexes are created on the parent and propagate to existing and future partitions; the vector index must be built **per partition**. New partitions are created on demand by the ingest path, serialised with an advisory transaction lock — without it, two concurrent first batches for one new company both pass the existence check and the loser hits a duplicate-table error that fails the whole batch.

### But a partition key is not an identity

Cross-company non-leakage rests **first** on the main process passing that filter on every call. The router rejects a missing company filter with a 400 (fail-closed), and the partition is the second line of defence.

The `recall_memory` tool holds no identity logic of its own either: the scope is clamped from the flow-resolved caller — a DM recalls only your own history, a group only that room — and the model just supplies the query. Data-plane authentication (`X-API-Key`) is the third line: the leaf holds cross-company corpus, and without that door any process that can reach the port could read any partition, or inject "memories" into one.

### Three ordering constraints

All taught by the connection pool:

**The query vector is computed before a pool connection is taken.** Embedding is an outbound HTTP call with a timeout measured in minutes; waiting on it while holding a connection drains the pool.

**Reranking happens after the connection is returned.** And a rerank failure falls back to vector scores with a WARN rather than failing the query.

**The vector index's search parameters must be re-applied on every checkout.** The reset on return clears them, so setting them once at connection creation means they silently revert to defaults afterwards — filtered vector queries then under-recall while raising nothing at all. Silent degradation is the hardest class of bug to notice, which is why the reason is written into the code.

### Response order cannot be trusted

The OpenAI-compatible embedding protocol makes no ordering guarantee (which is why there is an `index` field). The client sorts by index and then checks the count:

- a reordering would attach A's vector to B's message — silent corpus corruption;
- a short response would make a naive zip drop the tail and still return 200.

---

## Why this belongs outside the main process

**The dependency shape is simply different.**
This is a Python stack: FastAPI, asyncpg, a Postgres with a vector extension, and two local inference services. Folding it in would mean either turning the main process into Python or rebuilding vector retrieval in Go — neither is the answer. As a sidecar, the model services move to whatever box has the compute, with zero change on the main side.

**Retrieval decoupled from generation.**
This service only retrieves; sovereignty over generative calls stays entirely with the main process. The benefit runs both ways: the sidecar needs no model pool, no credentials, no notion of a turn; and the main process is not handed a narrative it cannot audit, but exact quotes with exact timestamps that can be shown to a user as evidence.

**Fail-open.**
With the service down the main process runs normally — `recall_memory` reports unavailable, corpus rows queue in the outbox until it comes back. Cold memory is an enhancement, not a critical path, and should never be the reason a turn fails.

**The storage choice is reversible; the dimensionality is not.**
Keeping vectors in a general-purpose relational database rather than a dedicated vector store is a decision scoped to size: at moderate scale the two perform the same, while the relational side wins outright on metadata filtering, transactions and operational complexity. If scale ever forces the switch, exactly one client module is replaced.

But the **embedding model and its dimensionality are a one-time choice**: changing either invalidates every historical vector. A frozen dependency like that belongs in the documentation, the schema comment *and* the self-check endpoint at once — not quietly parked as a default in a config file.
