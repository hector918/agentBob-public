# Architecture Overview

agentbob is a self-hosted, IM-first AI agent written from scratch in Go. It connects to several chat platforms at once, turns every inbound message into a *turn*, and within that turn calls a model, runs tools, and writes results back.

The whole codebase is organised around one principle: **separate mechanism from policy**. "What the system can do" sinks into pluggable modules; "how a particular kind of conversation is orchestrated" floats up into flows that can be swapped out whole. Changing behaviour means adding a flow, not touching the mechanisms.

> **Interactive diagram** — [Mechanism and policy in five layers](../diagrams/architecture-layers.en.html) · [中文版](../diagrams/architecture-layers.zh.html)
> A standalone HTML map of the layers, the module groups and the trunk wiring, with guided
> views, search, relationship tracing and PNG/SVG export. Node evidence links back to real
> files at a pinned revision. Rendered locally, or on GitHub Pages if enabled — a raw
> `.html` link on github.com shows source rather than the page.

[![Mechanism and policy in five layers](../diagrams/architecture-layers.en.preview.png)](../diagrams/architecture-layers.en.html)

---

## Five layers

```
contract     Contracts: capability interfaces, cross-module envelope data, and
             a small set of shared grammar functions. Everyone may import it;
             it imports nothing local.
     ↑
heartwood    Shared primitives: process clock, attachment staging, prompt
             building, credential redaction. The one implementation layer
             every module may import directly — and three of its packages
             also register as trunk modules, so the tools are imported
             directly while their lifecycles are sequenced by the trunk.
     ↑
trunk        The thin spine: capability registry, lifecycle sequencer,
             housekeeping scheduler. Zero business knowledge — it does not
             know what a message, a session, or a company is.
     ↑
leaf/        Modules: behavioural subsystems with lifecycles and resources.
             Modules never import each other; they reach one another only
             through capability interfaces obtained from the trunk.
     ↑
flow/        Flows: thin orchestration scripts, selected per conversation
             type, replaceable end to end.
```

Alongside these sits `sidecars/` — dependency-heavy services running in their own processes or containers (browser engine, speech transcription, OCR, retrieval). That boundary is a **compile-time** boundary as well as a runtime one: a sidecar carries its own `go.mod` and so cannot reach `contract` or `heartwood` at all. The main process talks to them over HTTP.

Chat platform connections do not work this way. Each source holds its own long-lived link to a third-party platform — WebSocket, IMAP, SMTP — with its own reconnection and at-least-once semantics. Most of the system's liveness complexity originates there rather than in the sidecars.

---

## trunk: what the spine does

`trunk` provides exactly three things, all of them content-free:

**Capability registry** (`10-registry.go`) — maps an interface type to its single implementation. Providers register once during `Start`; consumers `Require` once during their own wiring, then hold the reference and call point-to-point. **The trunk is a match-maker, not a mediator**: it is not on the per-call hot path.

**Lifecycle sequencer** (`20-lifecycle.go`) — builds a dependency graph from each module's declared `Provides` / `Needs`, topologically sorts it, then drives `Start` in order and `Stop` in reverse, detecting cycles, missing providers, and duplicate registrations at startup.

**Housekeeping scheduler** (`30-housekeeping.go`) — a single worker draining a priority queue of periodic persistence-maintenance tasks (DB prunes, disk sweeps). Modules join lazily with `TryRequire[Housekeeper]` instead of each spinning its own timer, so storage-heavy sweeps run in one coordinated place. The dividing line is durability: sweeps that touch persistent state belong to the housekeeper, while purely in-memory hygiene stays on a module-local ticker.

One thing about startup that the naming invites you to misread: **`Optional()` does not mean "unimportant".** It is a contract on consumers — they must hold the capability on a soft edge and treat its absence as a legal state, so that a provider failing to start degrades the system instead of aborting the boot. What "degrades" means is defined per module, and deliberately differs. The authorization module, when it cannot start, **stays present** with a deny-everything matrix, which yields zero usable tools. The organisation module genuinely fails open: no organisation means no organisation flow, and everything else carries on. Security-relevant modules always take the strict reading — vanishing is not an option there, because a vanished judge reads to callers as an open door.

The counter-examples are the non-optional ones: `pgpool`, `slash`, `claimtoken`. Without them nothing works at all.

A fourth mechanism — an asynchronous signal bus — has a place reserved in the design but is not implemented. The synchronous turn-lifecycle hook chain is deliberately **not** a trunk concern; it belongs to the turn and flow modules.

---

## contract: what earns a place

`contract` is a deliberately flat single package — the vocabulary is densely interconnected, and splitting it into subpackages would invite import cycles. A type belongs there if and only if it is part of a **trunk-mediated** contract:

1. a capability interface registered on the trunk — i.e. a key in the registry, which a consumer calls through instead of importing the provider; or
2. a data type appearing in such an interface's method signatures — the payload flowing through trunk-mediated calls (message, attachment, tool call, model response, and the like).

There is a third, deliberate category worth naming, because it cuts against the "zero behaviour" impression: **shared grammar**. `ScopeFor` / `TargetForScope` / `SplitMemberSubScope` are pure functions that live here not because they appear in some signature, but because how a scope string is composed and decomposed must have exactly one spelling — let the session resolver and the inbox router each write their own and the two will eventually drift apart. The same reasoning admits a few cross-layer sentinels: the error value that distinguishes "queue full, retryable" from "backend is down" is phrased for users by a layer that sits nowhere near the pool that raises it, so the value itself has to live where both can reach it.

Everything else stays out. A type used by exactly one module and appearing in no contract signature stays in that module, even if it is "just a struct". Types shared across a **direct** coupling — a plugin and its parent module, say a source and the gateway — live in the owning module and are imported directly by the other side. Direct references do not earn a place here.

## heartwood: the one direct-import exception

Leaf modules never importing each other is a hard rule. `heartwood` is the single exception: any leaf or flow may import it directly.

The bar for membership is correspondingly high — a member must be a genuinely shared, no-upward-deps primitive that **must behave identically wherever it is produced**. There are currently four:

| Package | Why it must be identical everywhere |
|---|---|
| `prompt` | `SanitizeSpeaker` is the same `"[name]:"` injection defence everywhere; `EstTokens` must let compaction and the model pool agree byte for byte |
| `clock` | A process clock calibrated against the database's authoritative time; several hosts writing one DB need a single time scale |
| `files` | Sandbox file store plus the inbound-attachment sweeper — a shared filesystem primitive |
| `scrub` | The credential-redaction defence; redaction must be byte-identical on every path |

It is **not** a convenience home for single-consumer helpers, nor for subsystems with heavy external dependencies (ffmpeg, a network backend).

This layer also has a dual identity: `prompt`, `clock`, and the `files` sweeper each **register as a trunk module**. The tools themselves are imported directly while their lifecycles are still sequenced by the trunk — living in `heartwood` and being a module are not mutually exclusive.

A high admission bar has a price, and it is worth stating plainly: **what fails the bar gets mirrored rather than shared.** Credential redaction exists as a hand-copied fork inside `sidecars/browser`, because a sidecar is a separate Go module and cannot import `heartwood` at all. The failure-snapshot ring store exists byte-identically in `tools`, `skills`, and `agora`, because leaf modules may not import one another and a filesystem mechanism does not belong in `contract`. These duplications are not oversights — they are the rule's invoice, which is why each one has a test watching it for drift.

## arch: the rules, welded into tests

The `arch/` package contains no product code. It is the machine guard for everything above — a dozen or so test functions in several classes:

- **The connection graph.** `wantGraph` is the approved module connection graph: any module gaining or losing a `Provides` / `Needs` edge, or being added or removed, turns the test red. The red is the point — a new inter-module connection must be reviewed and approved *here* before it ships. `wantOptional` is the matching ledger for `TryRequire` soft edges, which are invisible to `wantGraph`; before that test they lived in a hand-written comment ledger that silently rotted.
- **Dynamic provision.** `wantProvides` scans real `trunk.Provide[...]` call sites, so it sees capabilities published only at runtime — the kind reflection over `Provides()` cannot detect. It also turns "provided but never consumed" dead wiring into a build failure.
- **Admission and boundaries.** `heartwoodAllowed` blocks casual additions to `heartwood/`; an import-boundary test enforces that leaf modules never import each other; another test holds the redaction module byte-identical to its sidecar fork.
- **Startup order.** A few constraints cannot be expressed as hard `Needs` because the edges are soft, so tests scan the real registration sequence and pin them there.
- **Naming conventions.** Lazy-resolution wrappers must be named `lazy*`, or a soft edge could slip past the approval gate above.

---

## Module map

**30 registered nodes**, grouped by role. Note that "registered node" is not the same as "directory under `leaf/`" — flows register too, and so do three packages under `heartwood/`:

**Ingress and identity** — `stoma` (multi-platform gateway and its source plugins) · `gate` (admission and screening) · `inbound` (edge-of-process dispatch) · `accounts` (cross-entrypoint identity) · `claimtoken` (claim tokens) · `adminline` (admin channel)

**Conversation core** — `session` (session lifecycle and message storage) · `turn` (turn drivers, tool rounds, compaction, salvage) · `model` (model pool, routing, affinity, usage) · `modelgate` (outward-facing API gateway) · `prompt` (system-prompt builder)

**Capabilities** — `tools` (tool catalog and channel pool; it also publishes browser takeover) · `warrant` (authorization, local/remote execution, spaces) · `skills` (skill catalog) · `asr` (speech transcription)

**Organisation and orchestration** — `agora` (multi-member organisation, inbox routing, delivery) · `arrangement` (task orchestration) · `learn` (experience optimiser)

**Data and foundations** — `pgpool` (database connections; the root of the whole graph) · `retrieval` (cold-memory retrieval) · `urllib` (shared URL memory) · `credentials` (credential broker) · `clock` (calibrated clock) · `files-sweeper` (attachment sweeping)

**Interfaces** — `webui` (panels and console) · `slash` (slash-command registry)

**Flows** — `flow-router` · `flow-normal` · `flow-agora` · `flow-intro`

The browser is an easy thing to get wrong: **there is no `leaf/browser` module.** The engine lives in a sidecar; the main-process capability is published by `tools` — dynamically, according to configuration, which is why it does not appear in the static connection graph at all — and the console-side takeover entry point comes from `webui`.

Worth the same warning: **an edge missing from the graph does not mean there is no coupling.** `wantGraph` draws hard `Needs` only; soft edges are ledgered separately. Reading hard edges alone badly understates how connected some modules are — `modelgate` has exactly one hard edge, to the model pool, while its entire authentication hangs off a soft one.

Per-module detail lives in [modules/](modules/); the flow layer in [flows.md](flows.md).

---

## Navigation

| Document | Contents |
|---|---|
| [core/trunk.md](core/trunk.md) | The three spine mechanisms |
| [core/contract.md](core/contract.md) | Capability interfaces and envelope data |
| [core/heartwood.md](core/heartwood.md) | The four shared primitives |
| [core/infra.md](core/infra.md) | Entrypoint, config, i18n, logging, migrations, architecture guards |
| [modules/](modules/) | Behavioural modules |
| [flows.md](flows.md) | The flow layer: router / normal / agora / intro / inbound / compose |
| [sidecars/](sidecars/) | Out-of-process services |
