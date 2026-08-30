# modelgate

The outward-facing API gateway: it exposes bob's model capabilities to external callers as an OpenAI-compatible HTTP surface guarded by API keys.

## Where it sits

**Provides** — nothing. It is a pure exit; no other module consumes it.

**Needs**
- `contract.ModelPool` ([model](model.md)) — the only hard dependency. Without a pool there is nothing to expose.

**Soft edges** (`TryRequire`)
- `contract.APIKeys` ([accounts](accounts.md)) — bearer-token verification, per-key reach, billing attribution. Absent, it **fails closed**: every request 401s, never open.
- `contract.ImageCatalog` ([model](model.md)) — the style catalog and prompt manuals. Absent, the image endpoints report that no style is available rather than 500ing.
- `contract.PanelRegistry` ([webui](webui.md)) — its self-description. Absent, no panel; the API serves regardless.

The module is `Optional()`, and its bind is best-effort too: a failed `listen` degrades the API without aborting the agent, and an address of `off` or empty disables the server entirely. Binding to a non-loopback address earns a warning — every request needs a valid key, but on bare HTTP that key travels in plaintext.

**It is not a chat entry point.** It never touches flow / turn / session. It forwards a `contract.ModelRequest` to the pool and translates the OpenAI JSON shape at the edge. Keep it separate from [model](model.md) in your head: one answers "how does an incoming request find a backend", the other "how does an outsider get in at all".

---

## What it does

- **OpenAI-compatible endpoints**: `GET /v1/models`, `GET /v1/models/<id>`, `POST /v1/chat/completions` (blocking JSON or SSE streaming, tools passed through), `POST /v1/images/generations`, `POST /v1/images/edits`, plus a bob-specific `GET /v1/key`.
- **Per-key admission.** A key is either **lane-form** (it may enter a set of kinds, and each request names the one it wants) or **pin-form** (an explicit entry-name allowlist that bypasses routing). Both empty means the key reaches nothing — fail-closed, not "everything".
- **Capabilities travel outward, inventory does not.** A lane-form key sees **tags** (capability names), never the pool's entry names. Entry names are bob's internal inventory.
- **Billing.** Each request stamps a consumer mark on ctx (an agreed source family plus the key id), and the pool's per-handle ledger books this call's tokens against the key's account. Producer and ledger agree through one constant in `contract`, with no leaf-to-leaf import.
- **A shared capability manual.** `GET /v1/models/<style>` returns a style's full prompt-writing manual — the **same copy** the in-conversation `image_create` tool reads. Prompt guidance is the part that gets tuned most, and a second copy would drift from the first the week after it was made.

---

## Internal structure

### Server and authentication (`10-server.go`)

A minimal `server` struct carrying three request-time dependencies: the pool, a lazy key authority, a lazy image catalog. **There is no concurrency gate here**: capacity is the pool's business (per-entry concurrency, a per-kind wait queue, busy retry). A door gate could only *guess* the ceiling, while the pool knows real occupancy.

Authentication is `Authorization: Bearer` plus one `VerifyKey`. An unresolvable key authority also 401s — unavailable never means open. Request bodies are size-capped (a coarse post-auth DoS guard). Errors always render as the OpenAI error envelope.

### The catalog keeps lanes internal (`20-models.go`)

`GET /v1/models` lists **tags** for a lane-form key and the **entry names** it may pin for a pin-form key. The body is the OpenAI list shape (`data` / `id` / `object` / `owned_by`) plus one non-standard field, `state` — the one thing a caller cannot work out for itself and does need: whether this is usable right now. It uses the pool's vocabulary verbatim, with no outward re-spelling; a translation layer would just be one more model to learn.

**Lane names are deliberately not listed.** A lane (`kind`) partitions by payload shape, key admission, queueing and accounting — four things that live entirely inside bob. Outside, the only place a lane surfaces is *which endpoint you call*, and the endpoint already says that. Requests still **accept** a lane name for compatibility; they are simply not advertised.

One filter is worth naming: an image entry may legitimately carry purely operational tags (fallback hints, capability marks) that are not styles at all. Only a **declared** style is drawable, so an undeclared image tag offered as a capability would be refused as unknown by the very next endpoint — the two pointing at each other over a name the catalog invented. Those are filtered out.

### Resolving the routing address (`30-chat.go`)

`buildModelRequest` turns a request into a `contract.ModelRequest`, and the rules depend on the key's form:

- **Lane-form**: `kind` names the lane (it must be one the key admits); `requires` and `prefer` pass through to the picker **unchanged**. With `kind` omitted, `model` is read as a capability name — resolving which lane carries it is exactly what lets `kind` stay out of every outside vocabulary. When a capability spans more than one lane the endpoint **refuses** rather than picks: a guess would silently route the request into a different payload shape, queue, and ledger.
- **Pin-form**: `model` is the entry name and becomes `PinnedEntry` directly. The routing fields are ignored — pinning *is* bypassing routing.

`requires` passes through as a **hard** requirement, spelled exactly as an internal caller spells it. One mechanism, not two: graceful degradation is the admin-declared tag-fallback chain's job, and a chain that runs dry fails loudly. Softening it here would route external traffic around the pool's own "a queue beats a quiet downgrade" guard, which keys on a non-empty `Requires`.

**The lane fence can only stand here**: the pool deliberately does not check kind on the pinned path, so a key's lane admission has nowhere else to be enforced.

### One counter-intuitive streaming decision

Text deltas stream live. **Tool-call deltas do not.**

The reason is on the pool side: a pure tool-call round carries no visible text, so the pool keeps it retriable and may fail over to another backend mid-stream — re-running this watcher and re-emitting a fresh set of index-0 tool_call deltas. A client reassembling per index by concatenation would splice two partial argument strings into invalid JSON. So tool calls are emitted **once, complete**, from the settled response after `ChatStreamWatch` returns, when any failover has already resolved.

SSE headers are written **lazily**: as long as no content event has gone out, a pre-stream pool error can still surface as a proper status plus a JSON error body. Once the stream is open, a mid-stream failure can only be reported in-band (an error chunk, then `[DONE]`).

### Translating failure (`30-chat.go`)

`laneDiagnosis` answers "why did this get no answer" using **only what modelgate can actually observe**: the lane, and the health of its entries. It deliberately does not claim a cause it cannot see — a *live* candidate means the pool reached a backend and the request itself was refused (an oversized prompt being the common one), so saying "none could serve" there would be a confident lie. It counts entries rather than naming them: a lane-form caller addresses capabilities, and entry names are internal inventory.

It is also a **sanitiser**: the pool's own error text names entries, home-directory paths, and admin CLI commands, none of which may travel outward.

One distinction on status codes: a full wait queue earns **429** (the backends are up, there is simply nowhere left to stand in line), while a genuine outage earns 503. `contract.ErrModelQueueFull` is a cross-layer sentinel precisely so this layer can tell the two apart.

### The image endpoints (`35-images.go`)

A thin pass-through: authenticate, turn the OpenAI request into a `ModelRequest`, hand it to the pool, hand the bytes back. All of the interesting work — picking a backend, synthesising the workflow, waiting out the job — already lives behind `contract.ModelPool` and is shared with the in-conversation `image_create` tool.

Several deliberate refusals in place of leniency:

- `n > 1` is **refused** rather than quietly served as one picture: a caller that asked for four and got one back with no complaint has no way to notice.
- `change` (edit strength) on a from-scratch generation is **refused**: there is nothing to change, so the value would simply be dropped — and **a dropped parameter reads exactly like an honoured one in the reply**. The refusal names the endpoint that does take it.
- Only `response_format=b64_json` is supported (bob does not host image URLs).

Ordering is designed too: **the lane check runs before the style lookup**. The style-lookup failure names every drawable style, so running it first would hand the whole catalog to a key that may not draw at all — and a 403 on a real name would then confirm that the name exists.

"Declared but not being served right now" and "unknown name" are **two different answers** (503 and 404 respectively): the catalog already lists such a style with a non-usable state, so answering "unknown model" here would contradict it and send the caller off to fix a name that was never wrong.

`model` **names a style** here, unlike `/v1/chat/completions` where it is a lane shorthand. The divergence is deliberate: for generation, what a caller means by "which model" *is* the style, and asking anyone to send `model: "image"` would be a worse fit for the same field. Styles are capability names, so the no-inventory-outward rule still holds.

### `GET /v1/key` (`50-key.go`)

A bob-specific runtime-introspection endpoint. It lets a key holder read, from their own script, **what their key can do** (its policy) and **the live state of what it can reach** (health, context window, the tags worth preferring), so they can size their requests. It deliberately exposes no usage or billing and no internal identity — no backend model id, provider, priority, or error strings.

One detail: a lane row carries **no** lane-wide context window. A kind-level minimum is the smallest window any backend in the lane declares, which one small utility model drags down for everybody, and which no caller can act on. Capability is reported per **tag** instead — that is the grain a caller actually requires on, and since `requires` is hard, the number is a guarantee. A lane whose backends carry no tags at all has nothing to break down and reports its own window directly.

The response also carries the lists of parameters this endpoint **honours** and **silently ignores**. That is a property of the endpoint, not of any single model, so it rides at the top level.

### Panel (`90-panel.go`)

A small admin-only self-description: whether the API is serving, where, and how many deliverable llm entries the pool holds. It carries **no key data** — `contract.APIKeys` is a verify-only interface, and key management and listing live on the accounts side.

---

## Design rationale

**Outsiders see capabilities, not inventory.** Entry names are operator vocabulary and get renamed or re-backed at will. Tags are stable, meaningful, and the thing a caller can actually choose between.

**Unavailable always fails closed.** No key authority → everything 401s. A key with no policy → it reaches nothing. Neither may be re-read as "unconfigured means wide open".

**Don't build a second throttle at the door.** Capacity is the pool's business; a door gate could only guess, while the pool knows real occupancy and already has a queue and a backoff. So saturation surfaces as the pool's own signal and is translated here into a retryable 429.

**Whatever the catalog lists, the endpoint accepts.** They must be the same set, or the two contradict each other over some name and the caller has no way to know which to believe.

**An honest diagnosis beats a confident guess.** With a live candidate, say "the request itself was refused — most likely the prompt exceeded the window"; only with none, say the backends are not there.
