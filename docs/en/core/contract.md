# contract: the contract layer

`contract` is the shared vocabulary that lets modules talk to each other **without importing each other**: capability interfaces, plus the envelope data that flows through them. Zero behaviour.

---

## Where it sits

`contract` is the bottom of the dependency graph. Every module may import it; it imports no package of this project at all, only the standard library.

It pairs with [trunk](trunk.md): the trunk supplies the discovery mechanism (interface type → single implementation), `contract` supplies **the interface types being discovered**. Without `contract` the registry has no keys; without the trunk the interfaces here are just declarations nobody registers.

Its division of labour with [heartwood](heartwood.md) runs along a different axis: heartwood is **implementation that is imported directly** (the clock, redaction, prompt helpers), `contract` is **declaration that is never implemented here**. One shares code, the other shares shape.

---

## What it does

A single membership rule decides whether a type belongs: it must be part of a **trunk-mediated** contract.

1. **A capability interface registered on the trunk** — a key in the registry, which a consumer calls through instead of importing the provider.
2. **A data type appearing in such an interface's method signatures** — the payload flowing through those calls: message, attachment, tool call, model response, turn spec.

Everything else stays out:

- A type used by exactly one module and appearing in no contract signature stays in that module, **even if it is "just a struct"**.
- Types shared across a **direct** coupling do not qualify. A plugin and its parent module (a source and the gateway, a tool and the catalog) already reference each other directly; the shared type lives in the owning module and the other side imports it. A direct reference does not earn a slot in the public vocabulary.

The rule exists to stop `contract` from becoming a dumping ground for public types. Any addition motivated by "two places use it" rather than "this call crosses the trunk" should be turned away.

The package is deliberately **flat and single**. The vocabulary is densely self-referential (one `MessageEvent` drags in `Attachment`, `ChatType`, `Target`, `Caps`); splitting it into subpackages would invite import cycles immediately.

---

## Internals

Files are numbered **along the data flow**, so reading the numbering is reading the request path: the storage face → messages and attachments → admission screening → sources and the gateway → reply rendering → sessions → model messages and the model pool → usage → turns → flows → then the individual capability seams (tokens, prompts, slash, panels, accounts, tools, authorization, channels, skills, organisation, arrangement, API keys).

By content it falls into four groups.

### 1. The capability inventory

41 capabilities are registered on the trunk, plus `trunk.Housekeeper`, which the trunk provides itself.

**Infrastructure**

| Interface | Provider | Role |
|---|---|---|
| `DB` | pgpool | The SQL-generic storage face; the fact that the backend is Postgres stops at pgpool and each module's own persistence file |
| `Housekeeper` | the trunk itself | Where periodic maintenance tasks enrol (see [trunk.md](trunk.md)) |

**Ingress and identity**

| Interface | Provider | Role |
|---|---|---|
| `Gateway` | stoma | The source bus: owns each source's lifecycle, fans inbound events into one stream, probes health |
| `Screener` | gate | The single inbound-sender screen (denylist > bare-code redeem > allowlist), and the admin authority over the same policy |
| `AccessGranter` | gate | Mutates a source's access policy: allowlist a sender, toggle their admin bit |
| `ClaimTokens` | claimtoken | Token lifecycle only: mint a secret, freeze a payload, store with a TTL, hand it back exactly once. Channel-agnostic and post-flow-agnostic |
| `Accounts` | accounts | The read side of identity: cross-entrypoint handle → account and its flow policy |
| `AccountProvisioner` | accounts | The write side, kept separate from read-only `Accounts` so the routing hot path stays read-only |
| `APIKeys` | accounts | Bearer token → the account it bills and the model policy it may use. Absent → every request 401s (fail-closed) |
| `ConsumptionReporter` | accounts | Books each model call's real token usage against the consuming user's handle — the per-user billing ledger |
| `AdminLine` | adminline | The operator-summon funnel: source health, a dead model entry, and other urgent subsystem events |

**Sessions and turns**

| Interface | Provider | Role |
|---|---|---|
| `SessionManager` | session | The session subsystem's entry point: resolve which session an event belongs to, serialise work per session (one turn at a time), persist state |
| `MessageStore` | session | The only coupling face between the turn core and history — history belongs to session; a turn is a short-lived executor over it |
| `ChatHistory` | session | A read-only browsing view addressed by chat scope, kept apart from the turn write path so one interface does not widen to serve both |
| `MessageIndexer` | session | Maps a delivered reply's platform message id → session id, so replying to that message continues the same session |
| `SessionResume` | session | Wakes a session after an external event finishes (a browser-takeover hand-back); only auto-mode sessions are woken |
| `Turn` | turn | The turn core: round iteration (model → tool dispatch → repeat), salvage, compaction, a hard round cap |
| `TurnHandler` | flow/router | The session arbiter knows *when* to run a turn; this interface knows *how* |
| `FlowRegistry` | flow/router | Collects the turn-execution flows; the router polls them by Priority for who accepts the work |
| `PromptFactory` | heartwood/prompt | Hands out a fresh layered system-prompt builder per turn; the builder carries no conversation state |

**Models**

| Interface | Provider | Role |
|---|---|---|
| `ModelPool` | model | Routes a `ModelRequest` to a live backend entry (multi-entry tag matching, liveness, fallback) |
| `ImageCatalog` | model | The single copy of the style list and its prompt guidance, shared by the image tool and the outward gateway |
| `Transcriber` | asr | Turns inbound soundtracks into text at ingestion, so spoken content is ordinary text downstream |

**Capabilities and authorization**

| Interface | Provider | Role |
|---|---|---|
| `ToolCatalog` | tools | The full registered tool set (it *is* a `ToolSet`); flows project from it |
| `ChannelPool` | tools | Keeps stateful channels (exec sessions, remote connections) alive across calls; recycles them wholesale per principal |
| `BrowserControlHold` | tools | A heartbeat-leased hold asserted while a human drives the browser, so the tool yields instead of fighting them; the lease self-expires, so a dead tab cannot wedge anything |
| `BrowserTakeover` | tools (registered conditionally) | Relays the live remote-browser handoff face (screencast + input); published only when a browser backend is configured |
| `TakeoverMinter` | webui | Mints a one-time, coverage-locked takeover token — the seam for handing off to a human at a login wall |
| `SkillCatalog` | skills | Built-in plus external skills, external overriding built-in on a name clash |
| `SkillFailureSink` | skills | Records a trajectory snapshot for skills engaged in a degraded turn, as a learning signal |
| `Warrant` | warrant | The capability judge: given a `GrantSet`, filters catalogs and the credential vault, and vends gated channels |
| `Broker` | credentials | Builds a configured client for a named credential **without the secret ever reaching the caller** |

**Organisation and orchestration**

| Interface | Provider | Role |
|---|---|---|
| `Agora` | agora | The organisation read graph: scope → inbox → member → active employments, for routing, context, and self-authorization |
| `AgoraSend` | agora | The send tool's seam: target name resolution plus send authorization, strings in and out, so the caller imports no organisation types |
| `MemberFailureSink` | agora | Records a degraded turn's snapshot under each (company, role), for failure-driven role-guidance learning |
| `Arrangements` | arrangement | The role-bucket orchestration table: define, inject, pull, submit, status |

**Learning and memory**

| Interface | Provider | Role |
|---|---|---|
| `LearnRegistry` | learn | Collects "trainable text" targets; the engine walks every registered source each maintenance cycle |
| `URLLibrary` | urllib | Shared URL memory: record successful fetches, recall URLs already proven useful |
| `RetrievalClient` | retrieval | The cold-memory read seam; **fail-open** — unreachable means the caller skips, never that the turn fails |
| `RetrievalFeed` | retrieval | The post-turn write seam, enqueuing into a durable outbox for the feeder to push |

**Interfaces**

| Interface | Provider | Role |
|---|---|---|
| `SlashRegistry` | slash | The command table and its dispatch; command-owning modules register at Start |
| `PanelRegistry` | webui | The panel table, mirroring `SlashRegistry`: every module describes its own state and settings |

Those last two embody one pattern: **the UI is a generic renderer and every module describes itself.** The webui knows nothing about any subsystem; it collects panels and renders them with a fixed vocabulary of field kinds. Because the coupling is a struct of closures, the webui imports nothing but `contract`, and each module's state stays inside its own closures.

### 2. Envelope data

The payloads flowing through those interfaces, grouped by position in the pipeline:

- **Inbound** — `MessageEvent` (everything true about one inbound message: source, chat type, sender, text, attachments, dispatch metadata), `Attachment`, `ChatType`, `Caps` (what a source can do), `Target` (where a reply is delivered).
- **Outbound** — `Sink`, the surface a reply is rendered on. A local terminal prints deltas live; an IM source buffers them and edits a message on a rate-limited cadence. Two delta channels are kept deliberately separate: user-facing reply content, and the process trace a user can switch off.
- **Model side** — `Message` / `ToolSpec` / `ToolCall` / `ImageRef` / `AudioRef` / `StreamEvent` / `StreamAccumulator` / `ChatResponse` / `Usage`, plus `ModelRequest` and the pool's snapshot types.
- **Turn side** — `UserMsg` (one speaker's contribution, with author and structured attachments), `TurnSpec` (the fully composed turn a flow hands the core: prompt builder, joined user input, model selection, output sink), `TurnResult` / `TurnOutcome` / `TurnMode`.
- **Authorization** — `GrantSet`, one resolved authorization projection for one request (`tool:use:X` / `skill:use:X` / `credential:use:X`). Several suppliers collapse their own sources into this one currency, the judge makes a single decision against it, and **membership is defined exactly once, here** (a nil set grants nothing).
- **Panels** — `Panel` / `StateField` / `Cell` / `TablePage` / `SettingSpec`, the fixed presentation vocabulary.

### 3. Role interfaces (not registry keys)

`contract` holds far more than 41 interfaces. The rest arrive via membership rule 2 — they are **dragged in by capability method signatures**, not registered themselves:

- **Plugin interfaces** — `Source` (the gateway owns N) and `Tool` (the catalog owns N). Multiplicity is the parent module's business, as described above.
- **Optional-extension probes** — extensions on `Sink` (whole-product rendering, line-break flushing, direct photo send, held image delivery) and on `Source` (self-mention recognition, photo send, message reactions). A caller type-asserts to ask "does this implementation support that?" and takes the generic path when it does not. This is capability negotiation, not inheritance: a new source implements the base face to join, and the extensions only as it needs them.
- **Projection wrappers** — `ToolSet` / `SkillSet` and their concrete `ToolSubset` / `SkillSubset`. An authorizer applies its own policy filter, then hands the survivors to one of these to expose as a set. The projection logic itself is identity-agnostic, so it lives here once — modules may not import each other, so a "shared leaf util" is not an available option.
- **Narrowed sub-faces** — `GroupRouter` is the small slice of the organisation capability the session resolver needs (`Agora` happens to satisfy it); `HistoryReader`, `AttachmentSet`, and `SubRunner` follow the same pattern. Narrowing is deliberate: a consumer should not be able to see what it does not use.
- **The data face** — `Result` / `Row` / `Rows` / `Tx` under `DB`, SQL-generic with no driver types leaking.

### 4. Pure functions and context carriers

"Zero behaviour" has a few deliberate exceptions, all sharing one property: **these must be byte-identical on both sides, so there can only be one copy.**

- **The scope grammar.** `ScopeFor` encodes (source, chat type, chat id, thread id) into a scope string, and `TargetForScope` is its inverse. Being a faithful inverse pair is the entire reason both live in one file, and a test guards it — drift means a recovered image is delivered to the wrong chat, or to none.
- **Normalisation.** `CleanFileName` is the canonical form the attachment matcher applies to **both sides** of a filename comparison.
- **Context carriers.** The billing handle, identity, member, and the image-progress callback ride `context.Context` behind unexported keys with a `WithX` / `XFrom` pair. A side model call deep inside a turn (OCR, transcription, compaction) is therefore attributed correctly with zero explicit threading.
- **Construction sugar** — `OKResult` / `ErrResult` and friends, purely so every tool need not hand-write the same struct literal.

---

## Design rationale

### "What happens when it is absent" is part of the interface

Seventeen of the thirty registered nodes are optional, so a missing provider is the normal case, not an exception. Every optional capability's interface documents what its consumers do without it — and that behaviour is a choice, not an accident:

- **Fail-open** — cold-memory retrieval unreachable means recall is skipped; an absent URL library makes every method a no-op so callers are nil-safe by construction; an absent consumption reporter means spend simply is not booked.
- **Fail-closed** — an absent API-key verifier means every request 401s; an empty authorization projection grants nothing.

Direction is chosen by which side's failure is safer, never by what is convenient to implement. It is the same criterion that decides hard versus soft edges in [trunk.md](trunk.md).

### Narrow interfaces are the unit of decoupling

One provider frequently publishes several faces of different widths rather than one wide one:

- session publishes five — the turn write hot path (`MessageStore`), the admin read-only browse (`ChatHistory`), reply indexing (`MessageIndexer`), external resume (`SessionResume`), and the entry point (`SessionManager`). Merging them would let the browsing path's needs keep widening an interface on the turn hot path.
- accounts splits read-only identity lookup from write-side provisioning, so the routing hot path can only reach the read-only face.
- The session resolver routing a fanned group takes the three methods of `GroupRouter`, not the whole `Agora` surface.

The test is simple: **if two classes of consumer will grow their demands on one provider in different directions, publish two faces.**

### A type being kept out does not mean it is unimportant

The easiest misjudgement is the type shared between a plugin and its parent module. It crosses a module boundary and looks "public", but the coupling is direct — the plugin already imports the parent. Moving such a type into `contract` inflates the public vocabulary while removing exactly zero coupling.

The test is not "how many places use it" but "**does this call cross the trunk**".
