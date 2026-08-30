# Small modules

Seven single-responsibility modules. They are small because each does exactly **one** thing, not because they matter less — `pgpool` provides `contract.DB` and is the root of the entire dependency graph; the `SlashRegistry` that `slash` provides is Required by half the modules in the system. Each section follows the same miniature skeleton: what it is / where it sits / what it does / how it is built.

| Module | Provides | Needs |
|---|---|---|
| [urllib](#urllib) | `contract.URLLibrary` | `contract.DB` |
| [learn](#learn) | `contract.LearnRegistry` | `contract.PanelRegistry` |
| [pgpool](#pgpool) | `contract.DB` | — (root of the graph) |
| [credentials](#credentials) | `contract.Broker` | — |
| [slash](#slash) | `contract.SlashRegistry` | — |
| [asr](#asr) | `contract.Transcriber` | — (soft-depends on `contract.ModelPool`) |
| [claimtoken](#claimtoken) | `contract.ClaimTokens` | — |

---

## urllib

**URL memory.** One table recording the URLs the agent has successfully fetched, plus the configured search engines. Its reason to exist fits in a sentence: let URLs that have been **proven useful** surface above raw search-result noise next time.

### Where it sits

**Provides** `contract.URLLibrary` · **Needs** `contract.DB` ([pgpool](#pgpool))

`contract.DB` is a **hard** dependency — this module *is* that table, and without the database there is nothing to provide. Downstream, though, it is **purely optional**: the web tools and the flow hold it on soft edges, and in its absence every web tool keeps exactly its current behaviour. Every method is a no-op or an empty result when the library is absent, so all callers stay nil-safe.

Upstream consumers: the web search and fetch tools in [tools](tools.md) (`Record` on a successful fetch, `Recall` on a search), and the [flow layer](../flows.md) (`MarkSatisfied` when a turn ends with a clean final answer).

### What it does

The module records **two signals**, and only two. Signal ①: this URL was fetched successfully — the search query that led there is merged into that row's query list (deduped, capped) and the last-seen time is refreshed. Signal ②: this turn ended with a real answer, so the **last** URL recorded during it gets its satisfied count incremented.

Ranking uses the satisfied count, with recency as the tiebreak. There is **deliberately no visit count**: ranking by fetch frequency is a rich-get-richer trap — a portal that keeps getting fetched dominates every query it loosely matches, forever, with no regard for whether it ever solved anything. To rise, a URL must first prove useful.

It also learns two things about *how to open* a page. Search-engine rows carry their own **connection rung** (a plain request / needs rendering / needs stronger evasion) and **language scope** (global, or a language tag); engines are returned cheapest-rung first, so a caller's engine cap always drops the expensive ones first. Ordinary content rows remember, **per host**, which rung last worked — so the fetch ladder starts at the right rung instead of re-climbing from the bottom every time.

The retention sweep is **value-aware**: it removes only rows that are not engines, never satisfied anything, and have not been seen for a long time. Anything that ever proved useful, or was seen recently, is kept regardless of age — the long tail survives and only one-off noise ages out.

### Internal structure

`00-module.go` is the shell: migrate, seed, register the capability, attach the retention sweep. `10-store.go` holds all of the substance — the table and its versioned migrations, the `contract.URLLibrary` implementation, query-list merging, LIKE metacharacter escaping, host normalisation. `Record` runs in a transaction holding a per-URL advisory lock: merging the query list is read-merge-write, which no single UPSERT can express, so concurrent writers must be serialised or they clobber each other. `20-seeds.go` is the bootstrap set (a handful of search engines plus a reference site); seeding is idempotent and **value-preserving** — an existing row's counts and any hand-tuned rung survive, and only fields still holding their unset default are filled in.

---

## learn

**A text-space experience optimiser.** One engine, N "trainable text" targets: a tool description's supplementary tail, a skill document, a role playbook. The engine distils corrective lessons out of observed failures and writes them back into each target's own storage.

### Where it sits

**Provides** `contract.LearnRegistry` · **Needs** `contract.PanelRegistry` ([webui](webui.md))

It needs the panel registry so it can attach its dashboard at `Start`. That is a hard Need, but its provider is the always-present webui module, so it cannot skip-trap an Optional module. The model pool used for distillation is a **soft** dependency resolved lazily: no model, no distillation that cycle, nothing learned.

The **owning** modules of each target (the tool catalog, the skill catalog, organisational roles) each implement a `contract.LearnSource` and register it at startup. They therefore stay dumb — no model calls, no prompts of their own; the engine exclusively owns the single model call and the whole anti-rot policy.

### What it does

**Learn from failures only.** Successes carry no corrective signal. The engine reads each target's records since its own watermark and keeps only the failures.

**Learn once enough has accumulated.** A distill fires when the failure count crosses a threshold. There is also a backstop: once the *oldest* pending failure has waited long enough, a distill fires even below the count — otherwise a rarely-failing target starves forever.

**Integral rewrite, not incremental append.** Each distill produces the **complete new supplementary text**, not a delta. That is the anti-rot mechanism: stale or wrong notes disappear in the next rewrite instead of accreting layer by layer. The output is capped so it stays a compact supplement rather than becoming a second, competing manual.

**Injection defence.** The failure records fed to the distiller are **data, not instructions**, and the prompt says so explicitly; a tool-shaped record carries only the **argument shape** (key names and lengths, never raw values), so a crafted argument has no channel through which to write hostile "experience" into every later system prompt.

**One anomaly cannot destroy accumulated work.** An empty distiller response never overwrites existing text; output identical to the current text counts as no change; and a failed write-back still advances the watermark — otherwise a persistent write fault would re-read the same failure batch every cycle, paying for a real model call each time with zero progress.

### Internal structure

`10-engine.go` is the `contract.LearnRegistry` implementation plus the sweep driver: each cycle **re-enumerates** every source's targets (dynamic targets come and go with companies and roles, so they must be read fresh), distils each, then prunes the watermarks of targets that did not appear. The watermark advances to **the newest record in the consumed batch**, not to the completion time — the model call blocks for seconds, and records created during it would carry timestamps earlier than a wall-clock watermark yet not belong to this batch, so advancing by the latter would swallow them silently. `20-distill.go` holds the prompt and trace rendering. `90-panel.go` exposes how many sources are registered, how many targets exist, whether a distiller is wired, and each target's last-distilled watermark. The sweep itself rides the trunk housekeeper on a long period — learning is slow accretion, it does not belong on the hot path, and it should not spend without bound.

---

## pgpool

**The root of the dependency graph.** It owns the single database connection pool and provides the one interface every module's persistence layer runs its SQL against: `contract.DB`.

### Where it sits

**Provides** `contract.DB` · **Needs** nothing

With no Needs it is always first to `Start` and last to `Stop` in the topological order, and every persisting module sorts behind it. It is the **only place that knows the database is Postgres** — it wraps a standard-library pool and keeps the driver behind `contract.DB`. Domain modules own their own tables, migrations and SQL; they Require `contract.DB` and never import this package's internals.

`Optional() == false`: nothing persists without the database, so a failed connection must abort startup rather than degrade.

### What it does

**A SQL-generic execution surface.** Exec / Query / QueryRow / Begin, plus small interfaces for a result, a row set and a transaction. **No driver types leak** — that is what makes "Postgres stops here" an enforceable boundary. Nested transactions are explicitly unsupported: Begin on a transaction returns an error.

**Translating the driver's sentinels into the contract's.** "No rows" becomes the contract's own sentinel, so a module's store implementation can tell *not found* from *a real error* **without importing the standard library's database package**.

**Classifying lock contention.** Serialization failure, deadlock and lock-not-available map onto a single "retryable" sentinel, with the original error kept in the chain. It is the **only** store error a caller may retry — every other one must not be, because rewriting a bad write turns one bad row into two. The classification is exhaustive: the initial call, an error surfaced during iteration, a single-row scan, and a serialization failure that only appears **at commit** are all classified the same way.

**Connection hygiene.** The pool is deliberately small and recycles connections proactively: on a daemon running for days, an idle connection can be silently dropped by intervening network equipment, and the pool would hand out a dead one none the wiser. A bounded ping at startup verifies connectivity — the driver defers connection-string parsing, so both a malformed configuration and an unreachable server only surface there.

### Internal structure

`00-doc.go` *is* this module's boundary statement (read it first). `10-module.go` is the lifecycle: open, tune, ping, register. `20-adapt.go` holds all the adaptation — the error-classification table, the wrapping of the `database/sql` surface into `contract.DB`, the thin row and single-row wrappers, and the transaction implementation.

**One resilience trade-off stated outright:** pgpool deliberately has **no second backend and no fallback swap**. A sustained outage surfaces as plain errors; durability and graceful degradation are the spine's job (entry-side write-ahead plus admission control), not a backend switch. That simplification is exactly what retired an earlier primary/secondary/fallback trio.

---

## credentials

**warrant's secret backend.** It builds a **fully configured client** for a named credential — and the secret itself **never reaches the caller**.

### Where it sits

**Provides** `contract.Broker` · **Needs** nothing

Its only consumer is [warrant](warrant.md), and warrant gates authorization **before** asking for a build: tools never see this broker at all. `Optional() == true`: without the vault, remote spaces cannot be constructed, local ones still work, and the pipeline is unaffected.

### What it does

**Dispatch by kind.** Each credential carries a **kind** discriminator. The kind → builder mapping is a registrable table: SSH is built in, and other kinds are registered by the wiring layer at startup — **which is how that kind's domain code (and its type) stays in its own package**. credentials does not import it, and the built client keeps the secret shut inside itself. A builder returns an untyped value the caller asserts, so no third-party library type ever appears on the `contract.Broker` surface.

**Enumerating names by kind.** warrant needs "the one credential of this kind that this grant set admits", so the broker offers an enumeration that reads only the **kind field** — it neither parses nor holds any secret value.

**Redacted errors for callers.** Errors returned to a caller name the credential and nothing else — no path is ever echoed back. The real underlying cause (permissions, IO, a link problem) goes to the server log only; without it, a mis-permissioned vault would be undiagnosable.

**One bad entry must not take down the rest.** An unreadable or malformed entry encountered during enumeration is **warned about and skipped**, not turned into a failure of the whole resolution. Skipping *silently* would not do: that credential would vanish from its kind's candidate set, sending the operator off to fix permissions when the real problem is that one file.

### Internal structure

`00-module.go` holds the module and the broker: `Start` **freezes** a copy of the kind table, so registering a kind on the module after startup cannot mutate the live map the broker dispatches on. `10-vault.go` is the read-and-parse layer, with one internal constraint worth naming: the fast "kind only" path and the full "all fields" path **share a single scanner**, so the two can never disagree on whether a given input is accepted or rejected (otherwise a malformed entry could pass as a live candidate during enumeration and only blow up later at build time). `20-ssh.go` is the built-in SSH builder, which bounds both connect *and* handshake with a deadline — the library's own timeout covers only the TCP connect, leaving the handshake with no deadline at all, so a wedged peer would hang the calling turn forever.

---

## slash

**The slash-command registry.** A table of `/...` commands, and the dispatch that runs them.

### Where it sits

**Provides** `contract.SlashRegistry` · **Needs** nothing

`Optional() == false` — command-owning modules **Require** the registry at `Start`. **That is why half the system depends on it**: sessions, accounts, models, admission, organisation, orchestration, the console — every module with commands to expose needs it. The registry **holds only the table; the logic stays in each owning module**. The inbound flow hands a `/...` event to `Dispatch` after the gate, instead of running a model turn.

The registry owns exactly one command itself: `/help`, which reads its own table.

### What it does

**Wiring checks that fail at registration.** A duplicate name, a nil handler, or a name **dispatch could never match** (containing whitespace, `@` or `/` — the parser splits or strips those) all panic outright. These are wiring bugs and must surface at startup, the same posture as the capability registry. Names are stored lowercased to match the lowercasing lookup on the dispatch side; otherwise an upper-case registration would be permanently unreachable.

**Dispatch: parse, gate, run.** Split the text into a name and an argument string, reject an admin-only command for a non-admin sender, render a notice for an unknown name. Synchronous — a handler must be quick, no model calls. The return value only lets a caller judge success; the reply text has already been rendered through the sink either way.

**Line-structure correction.** A slash reply is **line-structured UI text** — a command list, a state dump, a receipt — assembled by handlers with bare newlines. Not every channel reads a bare newline as a line break: a markdown-rendering channel treats a single newline as a soft break and collapses the whole reply into one running paragraph. So dispatch rewrites line boundaries using the separator the channel declares for itself. **The judgement is made here**, because "this text is bob's own UI, not a model-written table" is precisely what a wire, handed pre-joined text, cannot tell. Two kinds of sink are left alone: those whose separator is already a bare newline (nothing to rewrite), and those whose output is consumed verbatim as a **product** — rewriting their line endings would edit a payload rather than fix a rendering.

### Internal structure

`00-module.go` is the shell: register the built-in first (it must exist before other modules add to the table), then provide the capability. `10-registry.go` is everything else — the table and its read-write lock, registration, dispatch, sorted listing, the `/help` rendering, command-line parsing (including stripping the `@botname` suffix some platforms append), and the sink wrapper that performs the line-boundary rewrite above.

---

## asr

**Voice preprocessing.** It turns an inbound message's voice and audio attachments into text **at ingestion**, so that spoken content is ordinary text for every downstream reader: the prompt, stored history, the corpus feed.

### Where it sits

**Provides** `contract.Transcriber` · **Needs** nothing hard; **soft-depends** on `contract.ModelPool`

The model pool is a lazily resolved soft edge — the trunk does not order the model module ahead of this one, but by the time the first inbound message arrives it has certainly registered. A deployment with no speech backend still provides a `Transcriber`; it simply never produces a transcript. The session core holds it on a soft edge, and a nil one means no transcription.

It couples to the session core **through the trunk, not a direct import**.

### What it does

**A split by whose words these are.** This is the module's most important judgement, and it is why the return value has two halves:

- **Instruction position** — the sender's own voice note on a message with no caption. That transcript **is** the request, and lands in the message text exactly as if typed. It is not sanitised: those are the user's own words in the user's own slot, and their typed text is not sanitised either.
- **Material position** — everything else with a soundtrack: a captioned clip, a clip carried in from a **replied-to** message, a video's audio. Third-party content, so it arrives explicitly framed with its source and run through the same hygiene the quoted-reply line uses.

The split is the whole point. Before it, a transcript was appended to the caption and landed in the user role unframed, unattributed and unbounded — the **one** path in the system that put somebody else's words into the instruction channel without a tool boundary (tool results are the tool role; quoted replies go through a dedicated reply line and its sanitiser).

**A long clip is *declined*, not *failed*.** Audio past the ingestion duration budget is not transcribed here; it is announced with its length instead, and the model pulls the words on demand with a tool. This is a **latency** budget, not a capability limit — ingestion runs synchronously while a human waits for the turn to start. The two must never blur: the failure note says transcription *broke* and asks the user to retype, which for a clip we merely chose not to attempt is both untrue and steers the model away from the tool that can still fetch the words. An over-long **video**, by contrast, is skipped silently — footage rarely carries intelligible speech, so advertising retrievable words on every shared video would buy a wasted tool round per clip; the file is visible in the attachment list and the audio tool accepts video, so the words stay reachable, just unadvertised.

**Everything else is best-effort.** A nil pool, an unreadable file, a backend error: log and degrade. **Transcription never fails a turn.** There is exactly one voice-specific fallback: when a voice-only turn's transcription fails, a note goes into the instruction position so the turn does not go empty.

**Say when something was transcribed.** On success the attachment itself is marked transcribed — otherwise the prompt's attachment list shows an untouched-looking file and invites a redundant fetch.

### Internal structure

`00-module.go` is the shell plus the lazy-pool adapter. `10-asr.go` is all the logic: the split decision, transcoding and the backend call, duration probing, the attachment-type test, transcript sanitisation, and stripping the wrapper markers some backends emit. Transcoding normalises the source clip to a canonical mono format at a fixed sample rate; **transcode and probe share the same concurrency semaphore** — both spawn an external subprocess per inbound clip, and the entire purpose of that gate is that a burst of messages cannot fan out into unbounded subprocesses.

---

## claimtoken

**The project-wide token authentication facility.** It owns only a token's **lifecycle**: mint a random secret, freeze a (kind, payload) behind it, store it with a TTL, hand it back on verification, burn it on consumption.

### Where it sits

**Provides** `contract.ClaimTokens` · **Needs** nothing

`Optional() == false`: the admission gate, accounts, organisation and the console all redeem through it, and without it the whole bind / wire / admit / takeover surface breaks silently — so a start failure should abort.

### What it does

**What it deliberately does not know matters more than what it does.** Who may redeem, from which channel (a chat message? an HTTP request?), and **what actually happens** once a valid token is redeemed — all three live in the owning module. The kind string is entirely opaque to the facility; the redeeming module knows its own payload type.

**Verify and consume are separate.** `Verify` authenticates **without** consuming: the caller gets the frozen (kind, payload), runs its own post-flow, and calls `Consume` on success (or on a terminal failure). A transient failure — or an ineligible redeemer — leaves the token untouched with its **original expiry** intact (no truncation), so the user can retry the same code. `Consume` is idempotent.

**Expiry has two paths.** `Verify` lazily reaps an expired entry it happens to hit, and a periodic sweep reclaims mints that were **never redeemed**. Correctness rests on the former alone; the latter only reclaims memory.

**One deliberate boundary.** Service-to-service authentication with a static key is **not** a claim token: there is no user business there, so it does not belong to this facility.

### Internal structure

A single-file module. The token table is a mutex-guarded in-memory map; tokens are cryptographically random hex strings. The sweep runs on **the module's own ticker**, not the trunk housekeeper — the housekeeper is reserved for persistent (database/file) sweeps, and pure in-memory hygiene spins its own timer, the same way console auth and the tool channel pool do.

---

See also: [Architecture overview](../architecture.md) · [contract](../core/contract.md) · [trunk](../core/trunk.md) · [Flows](../flows.md)
