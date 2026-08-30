# browser sidecar (browserd)

The service that moves the entire browser subsystem — chromium processes, page actions, the login-state vault, human takeover — into its own container.
What is left in the main process is a thin HTTP client and one model-facing `browser` tool.

---

## Where it sits

`sidecars/browser/` is a **separate Go module**: its own `go.mod`, its own vendor tree, buildable offline, and excluded from the main module (a nested `go.mod` *is* a module boundary).

It builds its own image:

- a build stage compiles `cmd/browserd` into a static binary;
- the runtime stage is a debian slim plus chromium and fonts (including CJK), running as a non-root user;
- a persistent volume is mounted as its own data home, holding the profile vault and its own configuration;
- the entrypoint forwards signals through tini, so SIGTERM runs a graceful shutdown — every chromium closed, directories and profiles preserved, the next boot warm.

Which is why the main process's image needs no chromium at all.

### The main-process endpoint

There is exactly one: `leaf/tools/browserremote/`.

| Direction | Main-process side | Protocol |
|---|---|---|
| Model actions | `browserremote.Client` (`client.go`) backing the single model-facing `browser` tool | JSON over HTTP, control plane |
| Human takeover | `browserremote` implements `contract.BrowserTakeover` (`takeover.go`), consumed by `leaf/webui` | SSE frame stream + input POSTs, takeover face |

The two faces are **two separate listeners**. A takeover SSE is a long-lived connection; sharing a listener with the control plane would tie up its goroutines and force its timeouts to accommodate a stream that never drains. Split, half an hour of someone watching a page costs no `navigate` anything.

Addresses come from `BROWSERD_LISTEN` / `BROWSERD_TAKEOVER_LISTEN`; the main process points at the control plane with `BROWSERD_URL` and derives the takeover address from it (same host, conventional port), so a deployment configures one URL.

A takeover listener that fails to start is **not fatal**: the control plane — the reason browserd exists — keeps serving; only human takeover is off.

### Trust model

**browserd makes no authorization decisions.**

It is a trusted executor on a private network. The main process runs the warrant check, resolves *which identity, which session, which mode*, and ships the result (`scope` + `profile_key` + `mode`) with every request; browserd executes what it is handed.

- Both faces share one bearer key, `BROWSERD_API_KEY`. That key authenticates exactly **one** client, the main process, and **encodes no user business**.
- Who may take over which browser is settled on the main process before it ever calls out. The human's browser only ever connects to the main process, never to browserd.
- With no key configured the process WARNs loudly at startup — at that point network isolation is the only remaining guard, and the port must never leave the private network.

An earlier revision encoded per-takeover authorization tickets into browserd and was rolled back wholesale: that was user business pushed into a service with no business understanding it.

### The tool never disappears

The `browser` tool is **always registered**; it does not appear and vanish with backend availability. When browserd is unconfigured or down, `Run` reports the backend unavailable.

Vanishing teaches the model "this cannot be done"; an error teaches it "this cannot be done *right now*". Only the `contract.BrowserTakeover` registration is config-gated — no URL, no takeover capability.

---

## What it does

### One tool, twelve actions

The model sees a single `browser` tool with an `action` enum:

`browser_navigate` · `snapshot` · `click` · `type` · `press` · `scroll` · `back` · `console` · `get_images` · `dialog` · `vision` · `tab`

Collapsing them is deliberate:

- a shorter tool list — length is itself selection noise;
- one authorization cap instead of twelve;
- the tool can gate itself at the **action** level: an interactive action on a session that never navigated is refused locally, without troubling browserd;
- concurrent actions against the one shared remote page are serialised instead of trampling each other.

That self-gating has a history. An earlier design kept the interactive actions as separate tools and gated their **visibility** per session (they appeared only after a successful navigate), because `get_images` looks relevant the moment a user attaches a picture, and the model would reach for it to read the attachment. Collapsed into one tool, the gate moved from visibility to a runtime refusal — the tool is always there, and using it wrong says so, explicitly.

### The ref protocol

`snapshot` returns not a thicket of CSS selectors but a compact tree of interactive elements, each tagged with a ref id like `@e1` / `@e2`; `click` and `type` address those refs.

The model never has to construct or guess a selector — which matters most for small models. Refs are renumbered from `@e1` on every snapshot, so the prompt explicitly asks for a re-snapshot after any interaction that may have re-rendered the page.

The Go side also validates ref shape (only `@e` plus digits), stopping a model-constructed attribute-selector injection at parse time.

### The login vault

Each identity (member) owns a master profile directory on browserd's persistent volume.

- A worker turn runs on a **read-only copy** seeded from that master — its own chromium, its own directory. Concurrent turns for the same identity never collide, and the copy is discarded on close.
- Seeding skips cache subtrees (large, regenerable, never login-bearing) and chromium's process-singleton lock files (a copy carrying a live lock makes the fresh instance think another process owns the directory).
- Exactly one path writes back to a master: a human takes over a copy, logs in, and explicitly saves the login.
- The vault also exposes export/import endpoints (tar.gz) so the main process can keep backups. Export 409s on a profile that is currently checked out — while chromium is live its embedded database is inconsistent, and a corrupt backup is worse than no backup.

Login cookies travel a **side channel**: the full cookie set — session cookies included — is read out of the live browser's memory over the debug protocol and persisted beside the master. A disk write-back can only carry what chromium has flushed, and structurally excludes session cookies (no expiry, never persisted); without this channel you would have to close the browser to save a login and still lose part of it.

### Human takeover

The takeover face streams a scope's live chromium to the webui as SSE screencast frames and accepts mouse/keyboard events back.

This is a **different trust path from the model's**: frame subscription and input dispatch talk to the debug protocol directly and bypass the strict allowlist that constrains the model. The justification is that this path's gate is elsewhere — webui admin authentication, plus the fact that a stream only opens on an explicit takeover.

- One live takeover stream per scope. A newcomer evicts the incumbent and waits for it to fully release the pool first, so the new stream arrives as the first observer on that tab and can apply its own quality tier.
- A stream with no input for five minutes closes itself. An open stream **pins** the profile copy (exempt from idle reclaim), so an abandoned takeover would otherwise hold it forever.

### The control hold

When a human switches the takeover to "control" mode, the frontend asserts a heartbeat lease on the scope.

- For the lease's duration every model-side `browser` action returns a fixed yield message instead of fighting the human over one page.
- The lease is a **heartbeat**: not renewed, it expires silently. So no matter how the takeover dies (tab closed, network dropped, process restarted) the model is never blocked forever.
- An **explicit** hand-back additionally parks a one-shot note that the prompt layer surfaces on the next turn: the human is done, carry on.
- A TTL expiry parks **nothing** — the human may just have lost connectivity, and the next action should simply proceed.

Keeping the two endings semantically distinct is the point of this design: an explicit hand-back is an event; a timeout is only an absence.

---

## Internal structure

### The control plane (`browserd/`)

`server.go` is a mux plus one `*browser.Pool`. Every handler is thin: decode → `Pool.AcquireRouted(scope, profileKey, mode)` → call the matching action → wrap in the envelope.

The endpoints fall into three groups:

- **actions** — navigate / snapshot / click / type / press / scroll / back / dialog / console / get-images / vision / tab;
- **session management** — purge (wipe a scope's browser state), save (write a login back), live (does this scope have a takeover-able instance right now — read-only, never spawns chromium);
- **vault** — `POST /profiles` is the live-checkout snapshot (the takeover picker's feed), `GET /profiles` the persisted listing, plus export and import.

`wire.go` defines the whole request/response vocabulary. **The envelope's layering is deliberate:**

- transport problems (wrong method, undecodable body) → 4xx;
- **action-level failures** (navigation timed out, selector missing, profile busy) → HTTP 200 with `ok:false` and `error`.

The latter are ordinary, model-facing errors, not service faults. Making them 5xx would have the main process's health accounting record "the model clicked the wrong button" as "the backend is broken", and then cool a healthy backend off.

`/healthz` reports liveness *and* echoes the configuration browserd actually resolved. The two processes read separate config files; a page-timeout mismatch makes a slow action look like a transport error on the caller's side rather than a timeout. Putting the resolved value on the health endpoint makes that drift visible at deploy time — a passive diagnostic surface, with no handshake forced.

`takeover.go` holds the three takeover endpoints plus the per-stream eviction and idle-close logic. `profiles.go` is the vault's list/export/import and a per-profile I/O lock.

### The engine layer (`tools/browser/`)

Split by responsibility, this is the bulk of the sidecar:

| File | Responsibility |
|---|---|
| `pool.go` | The **single owner** of chromium instances and profile checkouts: launch, idle TTL, LRU reclaim, console ring buffer, JS dialog handling, data-dir sweeps. The only place importing the low-level debug-protocol package |
| `actions.go` | Every browser operation as a plain `(ctx, *Session, params)` Go function. Swapping engines means rewriting this one file |
| `ref_map.go` | Interactive-element discovery and the `@e<n>` ref protocol — one injected JS payload |
| `tools.go` / `specs_export.go` | The ToolSpec definitions. The main process's remote shell reuses the exported specs verbatim, so name, description and parameter schema are byte-identical on both sides, from one source of truth |
| `tabs.go` | Multi-tab / popup management. When a click or `window.open` spawns a new target the engine stays bound to the original and the model goes blind; this tracks live targets, auto-follows a lone popup, and reconciles before every listing |
| `custodian.go` | The **single-goroutine actor** owning exclusive profile checkout |
| `humanize.go` | Human-input simulation: curved, jitter-timed mouse trajectories, randomized in-element landing points, per-keystroke typing cadence |
| `screencast.go` | The debug-protocol layer behind the takeover face: frame subscription and input dispatch |
| `profile.go` / `profilecopy.go` | Master directory resolution, copy seeding, stale-singleton cleanup |
| `logincookies.go` | The login-cookie side channel |
| `hold.go` | The control-hold lease and its one-shot resume note |

Two of these deserve their own note:

**The custodian is an actor.** Who holds which profile and when to reclaim it lives entirely on one goroutine, processed serially. Every piece of nasty concurrency the feature introduces — cross-scope contention, release-versus-cancel races, idle reclaim, takeover pinning — needs no locks at all. It has two **automatic** release doors: the owning scope's last session ends, or a checkout goes untouched past its idle TTL. Releasing closes the chromium instance but leaves cookies on disk — **a release is not a logout**.

**humanize's planners are pure functions.** Geometry and timing (trajectory, landing point, inter-key delays) are driven by an injected random source and unit-test deterministically; only a few dispatch helpers touch the engine. It uses ordinary pseudo-randomness rather than a cryptographic source on purpose — this is behavioural mimicry, not security material.

### Sweeps and shutdown

The sidecar runs two periodic sweeps of its own, because the main process's housekeeping cannot reach its disk:

- **idle sweep** — instances past the idle TTL are closed, but their data directories are **preserved by contract**, so the next action re-spawns warm;
- **data-dir sweep** — precisely *because* the first sweep preserves them, without this they grow unbounded on the volume. Reaped on an mtime TTL, and run **after** the idle sweep so a just-closed scope's directory is not spared by a stale live-session record.

There is also a one-shot sweep at startup: staging directories stranded by a crash mid-import (an OOM or SIGKILL bypasses the normal cleanup), each potentially hundreds of megabytes.

Shutdown follows the same semantics as the idle sweep: stop accepting, close every chromium, keep directories and profiles.

### Helper packages

A few small packages sit under `tools/`:

- `policy` — paths and commands in three classes: **banned** (refused outright, no approval prompt — "if it will never be allowed, don't even ask"; an approval that will never arrive just pins the turn), **needs approval**, **allowed**;
- `file` — sandboxed read / search / patch;
- `neterror` — uniform detection of DNS-class failures plus a **prescriptive** hint. Abstract advice ("try another approach") does nothing for a small model; it needs a concrete next step, or it will loop through variants of the same brand name;
- `textcap` — large-text truncation with an explicit marker. Stop the bleeding at the **tool boundary**: one fat response can run to a hundred thousand tokens, and once that is in the conversation history even compaction cannot save you (the compaction prompt itself blows up).

### The carried-over support layer

`config/` and `core/` are a **frozen fork** of the pre-rebuild codebase, kept only so the engine layer compiles unchanged. They take no part in the main process's architectural rules and must not leak back into it — exactly one package here is actually guarded.

### The scrub fork and its parity guard

`scrub` (credential redaction) lives in `heartwood` on the main side, as a primitive that **must behave byte-identically everywhere**. A separate module **cannot import `heartwood`**, so this sidecar carries a hand-copied fork.

`arch/51-scrub-parity_test.go` welds that fork shut:

- every `.go` file in the canonical directory must have a **byte-identical** counterpart in the fork;
- no stray files are allowed in the other direction;
- any divergence fails CI;
- a legitimately differing file must be explicitly registered with a written reason (the register is currently empty).

The risk is not hypothetical. The fork once froze at an old revision while the canonical copy took four rounds of redaction fixes, undetected across several review passes because each one only read the canonical copy. The guard turns a promise in a comment into an enforced invariant.

---

## Why this belongs outside the main process

**Chromium is not a reasonable neighbour.**
One page can eat several gigabytes, and the OOM killer does not necessarily pick it. Once chromium runs in its own container, whatever it does to memory and CPU kills browserd, not the conversation turns; the restart policy brings it back and the login state is untouched on the persistent volume.

**The main image stays clean.**
The main process is a statically linked Go binary; it should not carry chromium, fonts and a graphics stack just because it occasionally opens a web page. Same motive as every other sidecar: keep the heavy dependencies — chromium, CUDA, a Python ecosystem — out, so a sidecar can be deployed on whatever machine suits it with zero change on the main side.

**The separate module is a cost, not a benefit.**
A separate module means no `heartwood`, which means a scrub fork, which means an architecture guard. The bill is accepted for what it buys: independent builds, independent vendoring, independent release cadence, free of the main process's dependency graph. But **a fork must come with a guard** — that is the transferable lesson here.

**Authorization does not sink down.**
The value of a dumb service is that it knows nothing about user business. browserd has no idea what a company is, what a role is, or who is an admin; it knows only whether a request carried the key and what scope and profile the request names. Every "may they?" question stays in the main process, where the warrants, the permissions and the identity resolution actually live.
