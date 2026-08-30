# model

The model pool: it takes a request of the form "give me a model like *this*" and routes it to a backend that is alive right now, carries the required tags, and is not too busy — leaving behind a record of that call's health, affinity, and bill.

## Where it sits

**Provides**
- `contract.ModelPool` — routing, dispatch, snapshot. Consumed by [turn](turn.md) (the round kernel and its side-LLM calls), [modelgate](modelgate.md) (the outward API), and — over soft edges — [tools](tools.md), [asr](small-modules.md#asr), [learn](small-modules.md#learn).
- `contract.ImageCatalog` — "what can be drawn, and how do I write for it". Two modules that may not import each other need the same copy (the `image_create` tool and modelgate's image endpoints), so it sinks below both as an interface.

**Needs**
- `contract.DB` ([pgpool](small-modules.md#pgpool)) — the hourly usage table.
- `contract.SlashRegistry` ([slash](small-modules.md#slash)) — `/model` is the pool's own operator command.
- `contract.PanelRegistry` ([webui](webui.md)) — the pool describes a panel of its own, including a `models.yaml` editor.

**Soft edges** (`TryRequire`; absence degrades rather than fails)
- `contract.AdminLine` ([adminline](adminline.md)) — the "an entire kind is down" / "the wait queue is full" pages; absent, the alerts are silent.
- `contract.ConsumptionReporter` ([accounts](accounts.md)) — per-consumer billing on every call; absent, spend is not booked.
- `trunk.Housekeeper` — usage persistence and the retention sweep; absent, usage lands once at shutdown.

Both cross-module soft edges resolve through a lazy getter: accounts and adminline may start *after* model, so the lookup happens the first time it is genuinely needed.

The module itself is not `Optional()`, but **a broken config is not a startup failure**: when `models.yaml` is missing, empty, or unparseable, `Start` builds an empty pool — every chat call errors while the file watch stays alive, so fixing the file hot-loads with no restart. The one thing that does abort startup is a failed database migration.

---

## What it does

- **Multi-pool routing.** One `models.yaml` declares N entries; an entry is "this model on that provider". A request is confined to its `Kind` (llm / asr / translate / ocr / image), filtered hard by required tags, then ranked by how many preferred tags each candidate carries.
- **Failover.** Within one request, candidates are tried in turn; an error excludes the entry and moves to the next. Only an exhausted candidate set surfaces `ErrNoModelAvailable`.
- **A passive breaker plus an on-demand heartbeat.** Errors accumulating past a threshold put an entry into cooldown; while any entry is cold, a probe goroutine Pings it periodically and re-admits it *tentatively* on success.
- **Prompt-cache affinity.** The pool remembers which entry last served each conversation, keeps it there when everything else ties, and steers strangers away from entries other live conversations occupy.
- **Busy is not broken.** When a backend answers "busy / just restarted" and the request has nowhere else to go, it queues and retries in place rather than declaring the kind unavailable.
- **A bounded wait queue per kind**, sized as a fixed multiple of that kind's configured concurrency. Over capacity it returns a dedicated queue-full signal, so the surface above can say "try again shortly" instead of "the backend is down".
- **Two ledgers.** Per-backend operational usage (hourly rows in `bob_model_usage`, additive UPSERT) and per-user consumption (handed to accounts).
- **Exact weighing.** It can report the token count as measured by *the tokenizer of the backend that would serve this request*, so the turn layer only ever does arithmetic on real numbers.

---

## Internal structure

### The vocabulary of a request and an entry

Both envelopes crossing the pool boundary are defined in `contract` (`70-model-msg.go` / `72-model-pool.go`), because they appear in `contract.ModelPool`'s method signatures.

A `contract.ModelRequest` has six fields, each carrying one precise piece of routing meaning:

| Field | Meaning |
|---|---|
| `Kind` | The routing class. A request only matches entries of its **own** kind; empty means `llm`. |
| `Requires` | Tags the chosen entry **must** carry (all of them). Empty means any. |
| `Prefer` | Tie-break tags among qualifiers — more hits ranks higher. |
| `Tools` | Advertised to the model each round. Side-LLM callers (compression, judging, salvage) leave it empty. |
| `PinnedEntry` | Force one entry by name. Errors if it is not live, and **does not check kind**. |
| `AffinityKey` | An opaque conversation key for **soft** cache affinity. Empty records nothing and is repelled by nothing. |

`Kind` is a closed set: `llm` (the default), `asr`, `translate`, `ocr`, `image`. It exists to keep a non-chat model out of the general chat candidate pool — an OCR backend and a conversational model sharing one candidate set is a mismatch no ranking rule can rescue.

An entry surfaces as `contract.ModelInfo`: the static half from configuration, `State` and `InFlight` maintained live by the pool. `State` is one of five mutually exclusive labels:

| State | Meaning |
|---|---|
| `disabled` | Priority below the threshold. Never routed to, never probed, never counted toward "all dead". |
| `paused` | Administratively paused. Never auto-recovers. |
| `cooling` | Killed by the breaker; the cooldown has not elapsed. |
| `tentative` | Re-admitted after a successful heartbeat Ping. Pick-eligible, but one real error re-deads it. |
| `live` | Normal. |

A snapshot also carries three non-entry readouts (is the heartbeat running, the mtime of the config actually being served, is a usage store wired) and a queue readout that reports **only the kinds with someone waiting**.

One layer below sits `contract.Chatter` — the minimal **one connection to one backend** interface, with three methods: `ChatStream` (streaming, which is where tool rounds go), `Chat` (non-streaming, for side-LLM callers that want a clean response), and `Ping` (a token-free reachability check). Everything multi-entry — tag matching, liveness, failover — lives in the pool; a `Chatter` just talks to one machine.

`Ping`'s contract carries one load-bearing sentence: **a nil error means REACHABLE only** — the host answered at all, even non-2xx — and it does **not** prove the backend can serve completions, which is why the pool treats a successful Ping as tentative recovery. That is not caution for its own sake; a lazily-loading inference sidecar probes green right up until its first real request.

On the streaming side, a `StreamEvent` carries text deltas, thinking deltas, tool-call deltas (with an index), usage, and a done marker. The error `Chatter` returns covers **pre-stream** failures only; once the channel is open, transport errors arrive as events. `contract.Usage` carries cache-read and cache-creation token counts alongside input and output — the pool emits a cache-visibility log line only when the backend **reported** some reuse, and stays quiet otherwise (spamming a zero hit rate for every backend without prompt caching says nothing).

### The entry table, swapped atomically

`10-multipool.go` holds the body. `MultiPool` keeps the whole entry table behind an `atomic.Pointer[poolState]`: a config reload builds an entirely new table and swaps it in with one store. In-flight calls keep referencing the old `*entryRow` values (Go keeps them alive); new requests `Load()` the fresh one.

An `entryRow` is everything about one entry: its static `contract.ModelInfo` (name / provider / model / kind / priority / tags / context window / concurrency cap), a `contract.Chatter`, a concurrency semaphore, and — behind a per-row mutex — the dynamic half: in-flight count, an error sliding window, a consecutive-failure counter, a cooldown deadline, a backoff exponent, an admin pause flag, a tentative flag, the most recent error text, and hourly usage buckets. That dynamic state is **deliberately not persisted**: it is in-process observation, and it resets on restart.

The row also keeps the resolved connection facts (base URL and key), because some **side channels** talk to the same server outside the `Chatter` — exact tokenization is one. They are set at build time and immutable afterwards, so they read lock-free.

### Reload: two paths, two postures

`ConfigReloader` (`60-reload.go`) owns the reload lock, the mtime baseline, and the swap. It knows nothing about `MultiPool` — it talks back through three narrow hooks.

- **Explicit reload** (`/model reload`, or a panel save) does *not* carry liveness over: an entry the operator just edited deserves a clean slate.
- **The mtime watch** reloads asynchronously and **does** preserve liveness — an unrelated config edit must not resurrect an entry mid-cooldown. Every `pick` does a throttled `stat` in passing (at most one per interval) and rebuilds in the background when the file changed.

`buildEntry` is the construction both paths share: normalise the provider name, fill base URL and key-env-var name from the preset, probe the backend when appropriate, resolve the context window and concurrency, clamp priority, build the `Chatter` and the semaphore. The context-window chain is **yaml declaration > live probe > provider preset > global default** — the declaration is truth, because some inference servers report the machine's *total* window rather than the per-request capacity a caller actually gets. When the probe reads *smaller* than the declaration, that gets a warning. A disabled entry **skips the probe entirely**: an operator may import hundreds of disabled models, knocking on each door is an unacceptable startup cost, and the pool will never route to them anyway.

`servedMtime` and the watcher's own dedup baseline are two separate fields: the former advances only after a swap **succeeds**, so it answers the question an operator is actually asking — "did my edit take effect".

### Picking: two pickers, one window-blind rule

`30-pick.go`.

`pick` is the **serving** picker: a pinned entry short-circuits (a pin that points at a disabled / paused / cooling entry errors rather than quietly substituting another — the caller asked for that one), otherwise filter by kind, then by strict all-of matching on `Requires`, then drop what this request already tried and what is not eligible, then rank:

```
Prefer hits ↓ → Priority ↓ → in-flight load ↑ → own affinity entry first → foreign occupancy ↑ → Name ↑
```

**Picking is window-blind.** Context capacity plays no part in routing, and shouldn't: the turn asks `WindowFor` for the window of the entry a request ranks first *on configuration alone* and sizes its prompt to that. If it overflows anyway, the backend returns a recognisable 400, the pool translates it into `ErrContextExceeded` and hands it straight back, and the turn compacts and retries **on the same winner**. Routing around windows would only make every candidate collect the identical 400 in turn.

`pickStatic` is the **sizing** twin: same kind / tags / fallback chain, but configuration only (Prefer → priority → name), blind to health, load, and the tried set. Sizing decisions must be stable across loop-tops — a cooling blip that shrinks the budget triggers an **irreversible** over-compaction of healthy history. It also takes a structural capability filter (e.g. "does this provider have a tokenize endpoint"), which is what `CountTokens` uses.

`WindowFor` is what this window-blind design owes the turn layer: given a request, it answers "how big is the mouth of the entry this request ranks first **on configuration alone**". It goes through `pickStatic` and is therefore blind to health and load. Where serving actually lands may differ mid-failover, but that direction is covered by the context-400 → compact → retry net. The other direction has no net: a reading made smaller by a transient cooling blip would have the turn over-compact healthy history, irreversibly.

**Tag fallback** (`defaults.fallback`) expands a request's required-tag set into an ordered chain of sets to try: `[primary, hop1, hop2, …]`. A hop is reached only when the previous set yielded *no eligible entry at all* — it is graceful degradation, not a preference. A rule rewrites the set as `(current − rule.Tags) ∪ rule.To`, so unrelated required tags (a language tag, say) survive the swap. The chain is hop-capped and cycle-guarded.

### Cache affinity

`31-affinity.go` is an in-memory ledger with a sliding TTL: `AffinityKey` (the turn stamps its session id) → the entry that last served it. It acts **both ways** in the tie-break region of the ranking: a key's own requests prefer their remembered entry (its KV cache is warm there), and everyone else's are steered *away* from entries other live keys occupy (a stranger landing there could evict their cache slot).

Both directions sit **below the in-flight load comparison** — "about to be full" always beats the soft hold. The ledger is not persisted, because it mirrors the backends' KV caches, which do not survive a restart either. A **keyless request gets no view at all**: a conversation's side-LLM calls (compression, salvage, judging) share no token prefix with the main thread, so there is no cache win to steer toward — and worse, counting the requesting session's own record as "foreign" would actively repel its salvage call off the very entry its history was sized for.

### Concurrency, queueing, and busy retry

`failover` in `40-chat.go` is the single candidate-iteration driver behind both `Chat` and `ChatStreamWatch`: pinned short-circuit, pick/exclude loop, saturation spill, the saturated-primary wait, the all-saturated last resort, first-content deadline arming, per-attempt failover, and the terminal wrap.

Three ways to take a slot:

- `tryAcquire` never blocks. A full entry is recorded as saturated and **spills** to the next same-tag entry — failing over to a healthy peer is both faster and healthier than queueing behind a sick one.
- `acquire` blocks, and blocking *means queueing*, so it goes through that kind's **bounded wait queue** first (`enterQueue`). Over capacity it returns `contract.ErrModelQueueFull` — a cross-layer sentinel, because the user-facing wording has to say "the queue is full, try again shortly" rather than the "backend unavailable" a real outage earns — and pages the admin once per kind, latched.
- `acquireAnyOf` is the all-saturated last resort: wait on every saturated entry's semaphore at once and take whichever frees first.

One policy is worth calling out. If `pick` hands back an entry from a **fallback hop**, and the only reason the primary tag set came up empty is that its entries are *saturated* (not dead, paused, or errored), the driver **waits on the primary** instead of quietly degrading — the request asked for that tier, and a queue beats a silent downgrade. Only when the primary's queue is also full does it accept the degradation, and it logs it loudly.

`withBusyRetry` handles the busy class. `isTransientBackendErr` recognises 429 / 502 / 503 / 504, plus connection-level failures carrying no status (refused, reset, EOF, unresolvable host…). Sidecar backends are **deliberately** restart-happy, so a few seconds where the fronting proxy has no upstream is designed in, not an incident. Only the **last** candidate is allowed to queue and retry (while a peer remains, going there is better); across the retries the entry's concurrency slot is **held** — the retrying request keeps its place in line and others stack up behind it in the kind's queue, which is exactly what "the backend is busy" should look like from outside. Health accounting sees **one** error for the whole sequence.

"Is this the end of the line" is itself computed: `hasFallbackCandidate` asks, before each attempt, whether — with the current entry also excluded — `pick` can still return something, or an eligible saturated entry remains to wait on. It decides two things: whether a stalled stream is cut at the first-content deadline, and whether a busy backend is queued on and retried rather than abandoned.

### Streaming and watchers

`45-chat-stream-watch.go` is the tool-round entry. On top of the shared failover driver it adds three things:

1. A `StreamAccumulator` builds the eventual `ChatResponse` incrementally from events (text concatenation, tool-call reconstruction per index).
2. An optional `contract.StreamWatcher` inspects each event against the running accumulator; a non-nil return aborts the stream.
3. The pool's own structural protection, `ToolRoundWatcher`, composed **ahead of** the caller's watcher.

Watcher trips come in two levels, dispatched by sentinel:

- **Entry-level** (malformed tool-call markup leaking as text) — a different backend won't do that, so it is recorded against the entry's health and the pool fails over.
- **Conversation-level** (one tool call's argument JSON growing past the threshold without closing) — every candidate would bloat identically, so it is **not** a health signal: it returns to the caller, carrying the name of the entry that tripped, and the turn runs its own chunk-retry ladder.

Three invariants govern the streaming path:

- **First-content watchdog.** An entry that accepts the request and then produces no first content event — text, a tool delta, **or a thinking token** — is cut and failed over. It is armed only while **another candidate remains**; the last one runs to the provider's hard timeout, because a slow-but-live model beats failing the turn outright. Once first content arrives the timer is disarmed and later stalls belong to the hard timeout.
- **Committed stream.** Once user-**visible** content has reached the sink, no later failure fails over — doing so would stream a second response into the same sink and leave the user a garbled splice. A pure tool round shows nothing, so it stays retriable.
- **Thinking-only.** The backend billed output tokens, `reasoning` is non-empty, and the visible content is empty after a trim. This used to be indistinguishable from "the model truly returned nothing", so the round kernel read it as an empty reply and looped again on an **unchanged history** until it burned its fuse — with every leg a clean HTTP 200. It now **fails** the attempt (a peer with thinking off answers the same prompt fine, which is what failing over reaches for) without touching the health breaker (the backend streamed tokens; it is plainly alive), while still **booking the tokens** — it is the one failure class whose trigger condition guarantees a bill.

### Error classification

`15-errors.go` is the pool's "whose fault was that" table, because nearly every routing decision hangs off it. Three classes:

- **Prompt-level** (content 400/413/422, truncated tool arguments, context overflow) — every candidate would fail identically, so none of it counts toward any entry's breaker. Context overflow is narrowed further into `ErrContextExceeded`, the subset that compaction can actually fix.
- **Entry-health** (401/403/404/405/429, 5xx, genuine transport timeouts, a first-content stall) — this entry's own problem, and it *should* cool it.
- **Caller-initiated** (ctx cancel, caller deadline) — not the backend's doing. There is a subtlety here: `http.Client` wraps "the caller's deadline fired" and "the provider's own hard cap fired" in the same error shape, so the pool disambiguates **at the call site** by whether the caller's ctx is still alive, and stamps the latter with `errHardTimeout` so it passes the filter and counts toward cooling. Without that, a wedged backend would never trip the breaker and would burn a full hard timeout on every pick.

Classification leans on substring matching rather than `errors.Is`, under duress: backends wrap statuses and bodies as opaque strings, and there is no typed chain to reach into. The compensation is that the pattern tables are multi-entry and case-insensitive, so a provider rewording one phrase cannot silently disable a guard.

### Health: the passive breaker and the on-demand heartbeat

An entry dies on either of two independent rules: enough failures inside a sliding window, **or** enough consecutive failures with no success between. The second exists for low-traffic deployments, where a backend that fails every call has its errors minutes apart and never fills the window. Cooldown is exponentially backed off per consecutive dead event, capped.

`HeartbeatRunner` (`50-heartbeat.go`) is **on demand**: the goroutine exists only while at least one entry is dead, and stops itself the first tick that finds none. A CAS keeps it to one instance, and the exit path deliberately clears the flag *before* re-checking, closing the race window between "no dead entries" and a fresh death.

A successful Ping buys only a **tentative** recovery: the entry is pick-eligible again, but one real error re-deads it immediately — a cheap Ping succeeding does not prove the backend can serve completions.

One hard-won lesson is written into `nextProbeAt`. A fronting proxy's 502 still counts as "reachable" for Ping purposes (deliberately — gating on 2xx would strand a backend whose model-list endpoint 4xxs), so a permanently-broken backend cycled dead→tentative→dead, its backoff exponent climbing while the nominal cooldown elapsed for exactly none of the cycles, and every cycle burned one real request on a dead backend. Now only a recovery that **proved false** buys silence, and it does so on the backoff curve the entry is already keeping — no second ladder.

Alerting keeps **two independent latches**, held per kind:

- **All-dead** — a configured kind with zero usable entries. The kind scoping matters: a healthy ocr entry must not make "every llm entry is dead" look like a live pool.
- **Kind exhausted** — one request burned through every candidate of a kind and all of them errored. It has to exist separately, because the breaker counts **calls**: a rarely-called single-entry kind can fail 100% of the requests in its entire history without reaching any threshold, so the entry stays "usable", no kind ever goes to zero, and nothing pages. Alerting is therefore **decoupled from cooling**. Cooling still owns routing — counting calls is right there — it simply no longer gates the page.

### Usage and billing

Two ledgers, deliberately not merged.

`ModelUsageRecorder` (`70-usage.go`) drains the **operational** one: per-entry hourly buckets, drained by the housekeeping tick into `bob_model_usage` (`85-usagestore.go`, an additive UPSERT — which is what makes a partial-hour row correct). A failed write puts the bucket back for the next pass, *unless* those rows are leaving flush reach (shutdown, or the last pass of a batch displaced by a reload), where the failure is accepted as an explicit loss — a silent re-add there would only pretend the bucket was kept. In-memory bucket count is capped; overflow drops the oldest.

Both the flush cadence and the retention window go through the trunk's housekeeping scheduler rather than a module-owned timer: one periodic task drains completed hours (so an unclean exit loses at most the in-progress hour, which the final flush at shutdown takes with it), another prunes rows past the retention window. `bob_model_usage` is telemetry — one row per entry per wall-clock hour, only ever growing through the additive UPSERT — so something has to sweep it. Concentrating **persistent** maintenance in one coordinated place is the trunk's design intent.

Rows displaced by a swap cannot be drained immediately: in-flight calls still hold them and keep writing usage onto them. So they are **retired** onto the recorder's list and survive a couple of flush passes before being dropped.

`recordChatTokens` also reports the call's real tokens to the optional `contract.ConsumptionReporter` — **per call**, not at turn end, so side-LLM calls buried deep inside a turn are attributed for free. Attribution uses the kind of the entry that **actually served**.

`CountTokens` (`42-count-tokens.go`) is the pool half of the weigh-don't-guess model: window truth comes from `models.yaml`, token truth from the serving tokenizer, and the turn only does arithmetic on the two. Readings are cached by `(entry, model, content hash)`, so a count can **never** be served across tokenizers, and swapping an entry's backing model naturally invalidates its lines. The ruler is chosen with `pickStatic`, not `pick` — a live pick's health/load ranking would flap the scale between siblings and re-key the whole cache.

### Snapshot and the operator surface

`20-snapshot.go` is the read-side aggregator: one `state.Load()`, one row lock per entry, plus a `SnapshotInfo()` from each sub-component. Entry state folds by priority: `disabled > paused > cooling > tentative > live`. The last-error text is surfaced **only for a non-live entry** — it explains why an entry is unwell, and a recovered entry's stale error should not stay warn-coloured forever.

The queue readout is deliberately not derivable from the in-flight count: an in-flight call already *holds* a slot, so a saturated pool and a saturated pool with a line behind it read identically there. Kinds at rest are omitted — a row of zeros for every configured kind would bury the one that matters.

`80-slash.go` provides `/model` (admin): a bare listing, plus `pause` / `resume` / `reload`. The listing does not print the raw `State` word — a cooling entry is shown with **how long is left**. Chat is the pool's only surface when the operator is away from the LAN webui, and "cooling" without the remaining cooldown withholds exactly the answer they came for: usable in three seconds, or in five minutes.

 The read path goes through the contract `Snapshot`; the write subcommands type-assert a narrow interface, so a pool implementation without operator actions degrades to a clear notice instead of a panic. `90-panel.go` translates the same snapshot into the webui's generic field vocabulary and attaches the `models.yaml` editor, which saves through validate → backup → temp-file + rename. Validation runs the **same** `Validate` the load path runs, or a syntactically valid but semantically broken config would save "Success" and then be silently rejected by the reload.

---

## Subpackage `modelcfg`

Parsing and validating `models.yaml`, and nothing else. A `ModelsConfig` is a list of entries plus a small defaults block (global output cap, tag-fallback rules). `Validate` requires at least one entry, non-empty unique names, non-empty provider and model, and a `kind` drawn from the closed set.

That last rule earns its place: a mistyped kind would load fine, show as live, never match any request, and **suppress** the all-dead alert (that kind has a "usable" entry). So the whole config is rejected and the pool falls back to empty — a loud error beats a quiet black hole.

The output cap resolves entry → defaults → 0. On the module side, `entriesFromConfig` is the one place config and pool meet: it translates `ModelEntry` into `model.Entry` and lower-cases tag sets on the way (request-side tags are code constants, already lower-case), which is what makes tag matching case-insensitive.

## Subpackage `providers`

The backend-dialect layer. The pool only knows `contract.Chatter` (`ChatStream` / `Chat` / `Ping`); which implementation it gets is decided by provider name in `20-resolve.go`:

- **The OpenAI-compatible client** (`00-client.go`) — covers self-hosted inference servers and the compatible gateways alike. SSE parsing, tool-call deltas, the thinking channel (two spellings in the wild for the same thing; both are decoded), the per-entry output cap, and verbatim `extra_body` passthrough — with `stream` alone dropped from the merge, because the client's response handling is hard-wired to the path it was called on and letting config flip it would break the reader.
- **The native Anthropic client** (`30-anthropic.go`) — a separate implementation rather than an adapter, because the wire shape is materially different: system is a top-level field, every turn is a list of typed content blocks, tool_use carries parsed JSON, and the stream is a multi-event protocol.
- **The OCR sidecar client** (`36-rapidocr.go`) — maps `Chat` onto a sidecar's OCR endpoint. One image per message, batched across messages; failure is collect-all, so a per-image error fills that image's error field without aborting the batch. No streaming — OCR is request/response.
- **The image-generation client** (`38-comfyui.go`) — hides an **asynchronous job** API (submit / poll / fetch, plus an upload for image-to-image) behind one synchronous `Chat` call, so the pool's picking, health, and failover machinery is untouched. Progress rides a ctx hook (`contract.WithImageProgress`) rather than the return type: generation is a job with a lifecycle while `Chatter` is a single call, and reshaping every provider for the sake of one would be the wrong trade.

  Progress comes in three stages, the first with a special contract. `submitted` fires the moment the backend **accepts the job and names it** — before any pixel exists. A caller that must survive its own death (the draw tool's write-ahead log) records the job id here, and must commit it *before* the hook returns, or the crash it is guarding against can happen in exactly that gap. The event also carries **the entry that accepted the job** — the caller never chose it, the pool did, so this is the only way a recovery path can later pin the same backend and ask what became of the job. The `progress` stage's current/max are **per phase**, not global (a run reports sampling steps first, then tiled decoding), so the pair must not be rendered as one monotonic bar.

Alongside them:

- **`30-wire.go`** — the translation between `contract.Message` and the OpenAI wire shape, produced just before marshaling and never escaping the package. Keeping the wire shape out of the contract envelope is what decouples the contract layer from provider quirks: the day a native backend is added, its wire types live next to this file instead of inside `contract`. Message content is `any`, because the same field must serialise either as a plain string (the common case) or as an array of content blocks (multimodal: text, image, audio). One honest caveat lives here: the audio block uses the shape whisper-style ASR shims accept, while OpenAI proper expects a different one — **whether audio is understood depends on which ASR backend is configured**, not on any universal OpenAI-compat guarantee.
- **`10-profiles.go`** — the provider preset table: default base URL, the name of the env var holding the key, and a default context window for APIs that simply don't expose one. An unknown name is treated as custom (no defaults; base_url must be configured), so operators can name their own endpoints freely.
- **`40-probe.go`** — asks a backend, per provider, what its context window and concurrency are. Short timeout, errors swallowed: startup must not fail because a server happened to be down.
- **`45-tokenize.go`** — the tokenize-capability table. `CanTokenize` is a key lookup and `CountTokens` dispatches through the same map, so the two cannot drift — "added to one but not the other" is precisely the failure mode this table replaced.
- **`35-toolcall-extract.go`** — inline tool-call salvage. Some fine-tunes emit `<tool_call>…</tool_call>` as plain **text** instead of promoting it to the structured field; unsalvaged, that markup gets answered back to the user verbatim. Residual markup that could not be structured is classified as malformed output (an entry-level fault). The streaming and block paths do the same thing, because they have to behave identically.
- **`37-imagecaps.go`** — image capability declarations, organised by **style** rather than by endpoint. A renderer is not "the engine", it is a GPU running a generic renderer, and which model runs is decided entirely by the workflow submitted. Attaching the workflow to the entry encoded a fiction and denormalised the data — a second machine serving the same style had to copy the whole declaration. Keyed by style, an entry declares only where it is and which styles it can serve. Built-ins ship embedded; an external directory lets an operator retune a workflow or add a style this build never heard of, and it is **re-read on every reload** (a workflow tweak and a `models.yaml` tweak are the same operational gesture).
- **`50-reqlog.go`** — a JSONL log of outbound requests, for prompt-cache analysis. Two lines per request (req / res, sharing a pair id), capped in size with the oldest half dropped on overflow, so `tail -f` keeps working and the file stays parseable. **The system prompt is replaced with a length marker**: it is the large near-constant cacheable prefix, and logging it verbatim every turn would crowd out the turn-varying tail that is the point. Every record names **which entry** issued the request — a value plumbed down by tagging the ctx, the same idiom the image-progress hook uses, so the `Chatter` interface never grows a parameter for the sake of observability. Credentials go through the shared redaction primitive. An init failure self-disables and never blocks the request path.

---

## Design rationale

**A narrow interface means the implementation can be replaced whole.** The agent depends only on `contract.ModelPool`, so "the pool rolled back to a single model" needs no agent-side change.

**A broken config is not a death sentence.** An empty pool with a live file watch beats a dead sentinel object: the operator fixes the file and service resumes, without restarting a process that is serving.

**Whose fault a failure is determines what to do about it.** Nearly every branch in the pool — cool or not, fail over or not, page or not, back off or not — descends from that one classification question. So it lives in one file, and every class carries an explicit record of what a misclassification costs.

**Alerting has to be decoupled from cooling.** The breaker counts calls, which is right for routing and wrong for alerting: a rarely-called kind can fail every request it ever receives without reaching a threshold.

**Soft things always rank below hard things.** Cache affinity is a hint, not a constraint: load balancing outranks stickiness, and "about to be full" outranks "the cache is warm here".

**Sizing decisions must be stable; serving decisions may improvise.** Hence two pickers — one that reads health and load, one that reads only configuration. Letting a compaction budget follow a cooling blip costs something irreversible.
