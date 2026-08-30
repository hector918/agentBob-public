# heartwood: the shared primitives

`heartwood/` is the one implementation layer any leaf or flow module may **import directly**. It holds the primitives that must behave identically wherever they are produced.

## Where it sits

heartwood sits above [contract](contract.md) and below [trunk](trunk.md) (see the [architecture overview](../architecture.md)). It is not a module layer, but three of its packages register a trunk module apiece, because they own a lifecycle:

| Node | Provides | Needs | Soft edges (`TryRequire`) |
|---|---|---|---|
| `prompt` | `contract.PromptFactory` | — | — |
| `clock` | — | — | `contract.DB` |
| `files-sweeper` | — | — | `trunk.Housekeeper` |
| `scrub` | registers no module (pure library) | | |

Neighbours: `contract.PromptFactory` is consumed by the two main flows, `flow/normal` and `flow/agora` (see [flows.md](../flows.md)); `contract.DB` comes from `pgpool`; `trunk.Housekeeper` is provided by the spine itself and never appears in the module graph.

Membership is enforced by an architecture guard (`heartwoodAllowed` in `arch/50-heartwood_test.go`): adding a package under `heartwood/` turns the test red until it is deliberately listed. The bar is three conditions at once — genuinely shared by several consumers, no upward dependencies, and **byte-identical results everywhere**. A single-consumer helper does not qualify; neither does a subsystem carrying heavy external dependencies (a transcoder, a network backend).

## What it is for

All four packages answer the same failure mode: a semantic written twice in two modules drifts apart quietly, and the consequence is not a compile error but a wrong judgement at runtime.

- Prompt hygiene and size estimation drift → the injection defence lapses on one path, or compaction and routing compute two different sizes for the same history.
- The time scale drifts → audit ordering breaks when several processes write one database.
- Attachment write rules drift → sandbox escape, or unbounded disk growth.
- Credential redaction drifts → one path feeds a key to the model verbatim.

heartwood exists to nail each of those to exactly one implementation.

## Internals

### clock — the process clock and timestamp spellings

`clock` carries two unrelated responsibilities under one package name, because both answer "how is this instant expressed".

**A calibrated process clock.** The host clock and the database clock drift apart (NTP, multi-host deployments), and several processes writing one database need a **single time scale** or audit ordering becomes meaningless. `clock.Module` reads the database's `now()` once at `Start`, takes the local midpoint of the query window to cancel out half the round trip, derives `offset = serverNow − localNow`, and resyncs hourly. `clock.Now()` / `UnixSeconds()` / `UnixEpoch()` all return local-plus-offset, landing on the database's scale, always in UTC.

Failure degrades safely: no database, or a failed calibration, leaves the offset at zero — exactly the behaviour of a bare `time.Now()`, never worse, with a WARN recording the fact.

**Two timestamp spellings.** `clock.Stamp` is for anywhere a panel might show something *old* (a config's last edit, a last-run watermark), and it carries the year on purpose — without it, today and the same date a year ago look identical, and a panel is precisely where staleness has to be legible. `clock.TimeOfDay` is only for live-only tables where every row started moments ago and seconds are the part that carries information. Both render the zero value as an em dash: "never" is a real answer and should not appear as a year-one date.

Both render in the **local** zone, while logs are pinned to UTC (see [infra.md](infra.md)). The opposition is deliberate: logs get correlated against each other, panels get read by a person standing in a timezone.

### files — attachment staging and disk sweeping

`files.Store` is a **scope-agnostic** staging bucket. A source has to write inbound bytes down before the session scope is known, so they land first in the sandbox tree under `$BOB_HOME`, bucketed as `<source>/<YYYY-MM-DD>/`; once the submit pipeline resolves the scope, accepted files are moved into that scope's space.

`Save` carries three defences: the filename is folded to a safe character set; the joined directory is re-checked to still live under the base (`filepath.Join` cleans `..` away, so the check must be explicit); and the byte limit is **hard** — `io.CopyN` reads up to the cap and then probes for a single further byte to detect overflow, so a hostile upload costs exactly the cap in disk, not the cap plus one.

`files.Sweeper` is the maintenance side, registered as a trunk module. It does not spin its own timer; it soft-resolves `trunk.Housekeeper` and registers a periodic task there, so every disk-heavy sweep queues in one coordinated place. The sweep runs two policies:

- **The staging tree**: date directories are pruned by age, plus an optional total-byte cap. **Today's directory is exempt from the cap** — a just-saved, not-yet-placed attachment lives there, and capping it away would destroy a file the user is in the middle of sending. The cap exists for long-term growth, so it accepts at most one day of bounded overage.
- **The space tree**: two policies told apart by space **type**. A shared collaboration store holds data whose lifecycle outlives any session, so only its inbound subdirectory is pruned and the shared files at the root are never touched. Every other per-scope working space is session scratch, pruned recursively by mtime — real deliverables already left via the delivery tool, and regenerable intermediates re-materialise on next use.

The sweep walks pinned directory descriptors (`os.Root`) rather than path strings. A space directory is model-writable, so a string-based walk leaves a window between the check and the removal in which a directory can be swapped for a symlink, unlinking files outside the space. Even a link pointing *inside* the space is refused — the sweep must only ever touch the real subdirectory it meant.

### prompt — prompt building, text hygiene, size estimation

`prompt` provides `contract.PromptFactory`, and the builder behind it is deliberately **dumb**: a builder is an ordered set of named layers, `SetLayer` fixes a layer's render position the first time it is set and updates content thereafter, and `Build` joins the non-empty ones. The builder knows nothing about what any layer means — all coupling to context stays in the flow. Because it holds no conversation state, the module needs neither a store nor collaborators.

The two other groups in the same package are the actual reason it lives in heartwood:

**Text hygiene.** The structured prompt carries meta-structure like `[name]:`, while display names and quoted text both come from untrusted input. `SanitizeSpeaker` strips brackets and newlines and bounds the length; `SanitizeQuoted` additionally downgrades double quotes and truncates to a budget; `StripControl` removes every control character that would break the one-line contract (C0, DEL, C1, plus U+2028/2029) — but deliberately **does not truncate**, because an attachment path must be echoed back verbatim to resolve. This defence is rendered independently in several places (the turn core's speaker prefixing, the flow layer's per-event rendering, the session's nudge fast-lane); a missing copy in any one of them is an injection hole.

`QuotedMediaNote` is the example on this line where wording *is* behaviour. When the message a user replied to carried media, the quote block needs a note about it. Measured against a real model: a bracketed marker gets stripped by the sanitiser down to a bare word sitting inside the quote's own quotes, where it reads as the parent's **prose** — and the model then hunts for a file by that name. Naming the media kind alone is *worse* than the bare word: it asserts a picture exists and sends the model looking harder. What ends it is saying plainly that the thing is out of reach. Hence the function's "is it in this turn's attachments" parameter — that clause has to be *true*, because some platforms do download a replied-to message's media into the current turn.

**Size estimation.** `EstTokens` estimates by character class: a wide character (CJK, kana, hangul, fullwidth) counts as 1, everything else as 0.25. The crudeness is fine; the agreement is the point — the turn core's compaction trigger and the model pool's context-window pre-routing **must** use the same estimate, or one compresses history down to a budget the other would reject. `EstTokensMsg` also counts an assistant row's tool-call arguments, which are often the bulk of a large call.

### scrub — credential redaction

`scrub.Scrub` runs credential-shaped redaction over any free-form text about to reach a model — tool output, historical messages, bytes read off disk — replacing every hit with one marker, so the model sees "something was here, it was dropped on purpose, do not reconstruct it".

The patterns are deliberately **conservative**: each is anchored on a high-signal token — PEM armor, a known service prefix, the OpenSSH base64 magic, the password field of a URL userinfo block, an env-var name ending in `_TOKEN` / `_SECRET` / `_PASSWORD`. False negatives are acceptable (path policy and the credential-broker boundary still defend); a false positive that redacts legitimate tool output would silently degrade the model's answers, which is worse.

A few details are load-bearing:

- Properly paired PEM blocks are matched lazily first, and only **then** does the orphan-BEGIN fallback run. Upstream head-truncation of long output can cut the END marker off, and by construction any BEGIN header still standing after the paired pass is unpaired — so redaction runs from it to the end of the string.
- All three base64 alignment rotations of the OpenSSH armor are matched: one or two foreign bytes ahead of the key shift every quantum.
- Provider-key patterns capture their left boundary and restore it in the replacement (Go's RE2 has no lookbehind), otherwise a hyphenated identifier ending in those letters is eaten mid-word — exactly the false-positive class the package forbids.
- A 200-plus-character raw base64 run is redacted only when an **additional** condition holds (the run contains armor bytes, or "PRIVATE KEY" appears within the preceding 100 characters), and each match is judged against **its own** offset — otherwise, when the same run appears twice, a real key in the second copy would dodge the anchor.

## Design rationale

**Why redaction has a fork, and why a test welds it shut.** The browser sidecar is a separate Go module (its own `go.mod`, built into its own container), so it cannot import `heartwood` and instead carries a hand-copied `scrub`. That copy once froze at an old revision while the canonical one took four rounds of redaction fixes — and the drift survived four audit rounds undetected, because each of them read only the canonical copy. `arch/51-scrub-parity_test.go` now compares the two byte for byte and fails CI on any divergence, converting "byte-identical everywhere" from a comment into an enforced invariant.

**Why clock is not a leaf module.** It has to be callable directly from nearly every leaf (`clock.Now`), and modules never importing each other is a hard rule; as a leaf it would need a trunk edge per consumer, turning a pure utility call into a capability resolution. Its **calibration lifecycle** genuinely does need the trunk (a database, a background loop), so a trunk module lives in the same package — but that module only calibrates; it is not on the call path. `files` and `prompt` have the same shape: the utility is direct-imported, only the lifecycle goes through the trunk.
