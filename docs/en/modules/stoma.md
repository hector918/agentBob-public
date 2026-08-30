# stoma

The multi-platform chat gateway — the pore through which bob exchanges messages with the outside world. It owns every message source's lifecycle, fans N inbound platform streams into one, and hands replies back to the right platform by name.

## Where it sits

| | |
|---|---|
| **Provides** | `contract.Gateway` |
| **Needs** | `contract.PanelRegistry` |

Upstream is only configuration: `sources.Assemble` constructs the set of `contract.Source` implementations during process wiring and hands it to `stoma.New`. Downstream is the inbound flow in [flows.md](../flows.md) — it `Require`s `contract.Gateway`, consumes inbound events with `for range gw.Events()`, and calls `gw.SourceByName` to get a source back for rendering the reply.

The gateway **deliberately knows nothing** about sessions or screening. Who is allowed to speak belongs to [gate.md](gate.md); which session a message belongs to belongs to [session.md](session.md); both live above it, in the flow. The one `Needs` edge, `contract.PanelRegistry`, exists only so the bus can publish its own health view to [webui.md](webui.md) — the same "publish a panel into a registry" pattern model and slash use.

It also connects softly (`TryRequire`) to `contract.AdminLine`: source death, a failing probe, a recovery — events **only the bus can observe** — are reported by the bus itself rather than relayed through the flow. Soft edges are invisible to `wantGraph` and are booked separately in `wantOptional`.

A message's full path:

```
inbound                                 outbound
───────                                 ────────
platform SDK / IMAP / stdin             platform SDK / SMTP / stdout
   │  Source.Run                           ↑  streamsink.Wire
   ↓                                       │
MessageEvent ──┐                        Sink (one per turn)
               ↓                           ↑  Source.NewSink
     stoma: fan-in + health                │
               │  Gateway.Events()      Gateway.SourceByName(name)
               ↓                           │
       inbound flow → gate screen → session resolution → turn (model + tools)
```

The gateway occupies exactly two boxes: merging N platform streams into one, and handing a source back by name. Everything in between — who gets in, which session it belongs to, how the turn runs — sits above it.

---

## What it does

### The contract surface

`contract.Source` is the full surface of a **bidirectional** adapter; a platform must implement all of it:

| Method | Role |
|---|---|
| `Name()` | The source name — also the session-scope prefix, the health-table key, and the gate policy filename |
| `Caps()` | Self-declaration (below). Fixed after construction |
| `Run(ctx, out)` | Blocks receiving, pushing events onto the fan-in channel; returns on ctx cancel |
| `NewSink(...)` | Builds the rendering sink for one turn |
| `Send` / `SendFile` / `SendButtons` | One-off independent messages; an error means rejection |
| `HealthCheck(ctx)` | A cheap upstream-liveness probe — must return within seconds and must honour ctx |

`Run` must be **serially re-invocable**: the bus retries a boot-window exit on the *same instance*, so every per-run field (client, buffers, the closed latch) is reset inside `Run` rather than assumed fresh from `New`.

### Inbound: fan-in

Each source has its own `Run(ctx, out chan<- MessageEvent)` which blocks until ctx is cancelled, translating platform events into `contract.MessageEvent` and pushing them onto a shared channel. The bus runs one goroutine per source. The channel depth is a small constant — one slot per source is plenty in the steady state; the buffer only absorbs a burst while the flow is mid-turn. `Stop` cancels the ctx, joins every goroutine, then closes the event channel so a `for range` consumer terminates on its own.

The bus guarantees a clean channel close, **not delivery**: buffered-but-unstarted events are dropped on purpose — the sender re-sends.

### Outbound: three routes

Replies do not travel back through the event channel; they are point-to-point calls on the source:

- `NewSink(ctx, target, sessionScope, sid, prefs)` — one sink per turn, the main streaming-render path. See `streamsink` below.
- `Send` / `SendFile` / `SendButtons` — one-off independent messages (busy notices, admin alerts, file delivery). By contract the returned error means **rejection** (queue full, not connected, ctx done), not delivery failure.
- Optional capability interfaces: `contract.PhotoSender` (send a file the way the platform shows *pictures*, not attachments), `contract.MessageReactor` (the 👀 seen-it reaction), `contract.SelfMentioner` (the human-typeable handle addressing this bot), `contract.MessageEmitter` (in-process ingress). Callers type-assert; a source that doesn't implement one is silently skipped — `contract.DeliverPicture` / `DeliverPictureTo` write each fallback rule exactly once so call sites never repeat it.

### Capability declaration

`contract.Caps` is how a source declares itself upward, and `Trusted` there is a trust boundary: the zero value (`false`) means **screened**, so a source that forgets to declare anything is gated rather than quietly open. Only genuinely non-external sources set it true (the local terminal, the in-process bridge) — and the price is that they also bypass the gate's `IsAdmin` re-stamp, so they must stamp it deliberately themselves. `RequiredModelTags` lets a source hard-constrain which model may serve its turns; the flow maps it onto the model request when the turn opens.

### Attachment ingest

The four external platforms download very differently (a ready CDN URL, an SDK resource call, MIME decoding), but the ingest rules are one set:

- **Staging is scope-blind.** A source drops every file into one fixed bucket named for the source, and the inbound pipeline relocates them into the resolved session sandbox once session resolution has run. Historically the source guessed the scope itself, its heuristic disagreed with the real router, and attachments landed in the wrong sandbox — the source no longer participates in that decision.
- **A failed download still records the attachment**, with an empty local path. The prompt layer can then tell the model "the user shared something, but the bytes couldn't be fetched" instead of pretending nothing happened.
- **Capture-disabled (no file store) degrades down the same path** rather than returning early — an early return would also swallow the prose that sits alongside images in a rich-text message, dropping the whole message at the "no text and no media" gate.
- **Filenames disambiguate.** A multimodal API receives bytes and a MIME type, no filename, so three `photo.jpg` files from one album collapse into an indistinguishable heap for the model; naming them per message id keeps the text prompt's references pointing at a specific image.

### The fields that decide scope

A source never resolves a session, but the fields it stamps decide how one is resolved. `ScopeFor` is the **single** grammar mapping `(source, chat type, chat id, thread id)` to a scope string, and it lives in `contract` — below every consumer (the session resolver, the orchestration layer's inbox router), so the two can never drift. A per-source duplicate of that grammar is exactly what once caused the forwarded-alias miss. `TargetForScope` is its partial inverse: it recovers a deliverable chat coordinate from a scope, and reports failure explicitly rather than guessing when it can't.

Note `ThreadID`'s two faces: group / channel / topic scopes **flatten** it (one group is one scope), while a DM scope may be *split* by it into sub-scopes — which is exactly what email's forwarded aliases use, so one sender writing to two aliases lands in two independent sessions.

### Registration and config

`sources/10-assemble.go` is **the one place in the codebase that names platforms**: adding one is a config sub-struct in `00-config.go` plus an import and a construction block in `Assemble`. stoma itself never learns a platform's name.

Config reads the `sources:` section of the config file — only the "can we connect" half (on/off, endpoints, and the **names of the environment variables** holding credentials; credential values never live in the file). The "who may speak" half belongs to the gate and has its own policy files. Telegram / Feishu / Discord / Email are all **lists**, so one process can run several bots or several mailboxes; each entry's `name` is both its source name and the name of its gate policy file, defaulting to the platform name for the first entry and numbered suffixes for the rest.

A construction failure (missing token, missing secret) **skips just that source** with a loud log and never takes the others down. Conversely, two sources sharing a name panic at construction — that is a wiring bug, and silently shadowing one source is far worse than crashing.

### Health and lifecycle

The bus runs a heartbeat, probing every source's `HealthCheck` concurrently on a ticker with a short per-probe timeout. A probe must honour its ctx, or shutdown leaves it running on an orphaned goroutine. The health table folds outcomes into four states:

| State | Meaning |
|---|---|
| `ready` | fine |
| `degraded` | consecutive probe failures crossed the threshold (one transient miss doesn't count) |
| `dead` | `Run` exited with an error, and nothing restarts it for the process lifetime |
| `stopped` | `Run` self-exited cleanly via `io.EOF` (e.g. local stdin closing under a no-tty deployment) — not an error, but the receive loop is gone |

`dead` and `stopped` are both **sticky**: once `Run` has exited, an API-level probe succeeding afterwards would simply lie (the receive loop is gone), so later probes must not resurrect it.

The only automatic restart is the **boot-window retry**: a `Run` exit within the first few minutes of the process is almost always a connect-time transient (a host rebooted before DNS is up, an IM backend briefly unreachable) — exactly the failure an unattended deployment hits, and exactly the moment sticky-dead would take the whole inbound platform offline for the process lifetime. Past that window a steady-state `Run` exit is a real fault an operator should see; reconnection inside `Run` is the source's own business, and the bus stops hammering it.

Alerts always go out asynchronously (fresh goroutine plus `recover`): a panic in the deliverer crosses the goroutine boundary where the caller's stack cannot recover it, so a synchronous notify would crash the whole process over one alert — and a slow deliverer would stall the source and health goroutines.

The same health table is also translated into a webui panel: one count stat plus a table whose status column is colour-coded. The panel is marked **admin-only** — `LastError` can carry connection or configuration detail, which is operator data — and it is read-only: the bus reports here, it does not accept edits.

---

## Internal structure

The module proper is three files — `10-module.go` (fan-in and lifecycle), `20-health.go` (health table and probe loop), `90-panel.go` (its panel self-description). The mass lives in nine packages under `sources/`: three shared facilities, four external platform adapters, two in-process channels.

The four external platforms are not uniform in what they can do — which is precisely why the shared core exists: the differences are *declared*, and the core degrades for them.

| | telegram | feishu | discord | email |
|---|---|---|---|---|
| Transport | long polling | WebSocket long conn | gateway long conn | IMAP poll + SMTP |
| Rendering discipline | edit-stream | block | edit-stream | block |
| Trace channel | yes | yes | yes | no (a thread of half-thought-out replies beats no finished one) |
| Typing indicator | yes | no | yes | no |
| Reply anchoring | yes | yes | yes | yes (RFC threading headers) |
| Seen reaction | yes | yes (enum mapping) | yes | no |
| Distinct picture send | yes | no | no (attachments render inline) | no |
| Outbound throttle discipline | per-bot slot | per-bot slot | relay (library buckets) | send queue + retry |
| Length-cap unit | UTF-16 code units | UTF-8 bytes | Unicode code points | effectively uncapped |

That last row is not trivia: splitting must be computed in **the unit the platform itself counts**, or an emoji-dense reply gets rejected at a length that looked safe. The core therefore assumes no per-character cost model at all — the cut point is binary-searched over rune boundaries through `Wire.WireLen`.

### streamsink — the shared rendering core

Reply rendering for external chat channels has **exactly one implementation**. The core implements the fullest case: a two-channel (trace + content) buffered renderer, a rate-limited edit cadence, over-cap splitting, degrade-to-block under throttling, a "still working" typing indicator, and final-frame delivery that survives a cancelled turn context. A channel that cannot do one of these **declares** the gap and the core degrades for it — that declaration *is* the "fallback in the source".

The platform seam is `streamsink.Wire`: `Send` / `Edit` / `WireLen` / `MaxChars` / `RateLimited` / `BenignEdit` / `EditGone` / `Typing` / `RedactErr`, plus a `WireCaps` declaration. The core imports `contract` and the standard library only; **a platform SDK appearing in this package is a boundary leak**. A new channel plugs into bob by writing only its own leaf — the gateway and session layers never change.

Two rendering disciplines fall out of the one core:

- **Edit-stream** (`CanEdit=true`) — buffer, flush on a ticker by send-then-edit, split over the cap into a chain of messages, degrade to block on a sustained 429. The cadence is deliberately slow: a sub-second one both fights the platform's rate limit and outruns the eye.
- **Block** (`CanEdit=false`, email and feishu) — no intermediate flush; one render at `Finish`. Splitting still applies (email returns a huge `MaxChars` so it never splits).

A few load-bearing details:

- **`WireCaps.LineBreak`**: a line boundary is not written the same way everywhere. A markdown-rendering channel reads a bare `\n` as a *soft* break and collapses a whole trace run — or a command list — into one paragraph; there the boundary must be a GFM hard break. Only the **encoding** belongs to the channel; **which text is line-structured** belongs to the producer — only the slash dispatcher knows a command reply is bob's own UI, whereas newlines inside a model-written table must never be rewritten. That split of one decision between two owners is why `contract.LineBreakSink` exists.
- **The `Finish` fork**: `full` is the canonical content reply, but it is **not always** the concatenation of prior content deltas — the turn core calls `Finish` directly with error notices that never streamed. So the core renders the part of `full` the already-streamed prefix hasn't covered, and re-renders `full` whole when it diverges from what streamed. `contract.BareProductSink` is the symmetric convention on the other side: a sink whose `ContentDelta` renders nothing at all (the bridge return sink, an email batch segment, the sub-turn capture sink) declares "my `Finish` *is* the product", and the turn core must hand it the bare reply rather than the accumulation with tool-round preambles glued on.
- **Where 429 belongs**: the core treats a throttle as **terminal** — 429 pacing lives in the source-level send gate, which already relayed the call before the error surfaced, so a 429 still standing is a sustained throttle and further retries only hold the turn longer. The core degrades to block mode instead.
- **Split rollback**: hitting the recursion cap on an intermediate flush is acceptable (the tail stays in the buffer and a later flush sends it), but the `Finish` flush has no "later", so the terminal split is uncapped — capping there would permanently truncate the reply while reporting success.

`OnAnchor` fires for **every** new send that returns a platform message id — on both channels, once per chunk of a split chain — so a flow can index each delivered message for reply routing the instant it is sent. A user replying to a *middle* chunk then routes back into the same session instead of forking a context-less new one.

### sendgate — outbound send discipline

One implementation of "what happens when a send hits the platform rate limit", consumed two ways:

- **`Gate`**: a per-bot serialiser (telegram, feishu). Every REST send for one bot — streaming edits, one-off notices, reactions, file uploads — funnels through a single slot, honours a shared cooldown floor before dialing, and **relays** a throttled call itself (bounded) rather than dropping it. The platforms rate-limit per bot token, so per-bot serialisation is the correct grain. Without it, two concurrent group turns each build their own sink (whose lock only serialises *within* one sink), fire platform calls in parallel and back off independently — straight through the limit.
- **`Relay`**: the same bounded 429 retry *without* the serialiser, for sources whose client library already paces per route (discord). An outer per-bot slot there would only head-of-line-block unrelated channels.

Timeliness is the **caller's ctx deadline**, not a gate knob: every wait (slot queue, cooldown floor, relay backoff) is ctx-aware, and ctx is checked once more immediately before dialing — a typing indicator or reaction that queued past its useful life is dropped, never sent late. Callers declare urgency by bounding their ctx.

Only a **classified 429** is ever relayed: the platform explicitly rejected the send, so a retry cannot duplicate. Every other error — timeouts included, since those *may* have delivered — returns to the caller untouched.

### common — cross-source odds and ends

One thing: `RedactErr` replaces a source's own credentials (bot token, app secret) in an error before it reaches a log, then runs the generic scrubber over the rest. An error carrying no secret comes back unchanged (type identity preserved, hot path cheap). The package depends only on `contract` and shared infrastructure and **never imports a platform SDK**.

### telegram — the Bot API source

The largest adapter. It long-polls for updates, translates them into events, and replies through `streamsink`. In groups it answers only when @-mentioned or replied to.

- **Media-group (album) coalescing**: the platform fans an album out as N separate updates sharing a `media_group_id`, and the user's caption rides on **only one of them**. The buffer accumulates by group and flushes on whichever comes first — caption arrival or timeout — then emits **one event per sibling**, not one merged event. Each photo becomes its own turn, so the user gets a crisply-bounded reply per photo instead of one giant reply the platform then slices up, blurring which fragment answers which image. A flushed group is retained for a TTL so an out-of-order late sibling can still pick up the cached caption.
- **Deferred download**: an un-addressed group album's photos are **never fetched**. The download is built as a closure and deferred to flush, where the group gate is applied at **album level** (the whole album is accepted iff *any* sibling addressed the bot) — because the @ rides only on the captioned sibling.
- **Mention parsing**: entities are authoritative when present, so `mail bob@somebot.com` is not a mention. One message can carry several mentions of this bot, so every byte range is collected and spliced out in one **reverse-order** pass — stripping per-entity against the original text would leave all but the last behind. It also handles the command-menu form `/cmd@botname` (Bot API convention: `/cmd@me` acts as `/cmd`) and whole-word stripping of another bot's `/cmd@otherbot` in a multi-bot group.
- **Rich text**: replies are already markdown, the platform's rich message is close to GFM, and crucially it needs **no escaping** — so the model's raw text goes on the wire as-is. There is no escaper here and there must not be one; an escaper would show up as backslash litter in the rendering. Half-open markup (an unclosed `**`, an unclosed fence) degrades to literal text rather than erroring, which is what makes it usable at all for an edit-stream whose window necessarily cuts mid-syntax. Once the server rejects a rich payload the sink latches **sticky** plain text, so one reply can't flip between rendered and literal mid-stream.
- **Two kinds of "picture"**: the platform's photo send puts the image inline and pays for it with a server-side re-encode — original bytes, alpha channel and filename do not survive; the document send keeps all three. So the choice belongs to the caller and it is a choice about **intent**: a picture bob just produced for someone to look at goes as a photo; a file the user asked for by name always goes as a document, because silent re-encoding is data loss nobody agreed to.
- **Token hygiene**: the file-download URL carries the token in a path segment, and `net/http` wraps the URL into returned errors. The wrapper here redacts while preserving `Unwrap`, so `errors.Is/As` still reach the original sentinel.

### email — the IMAP + SMTP source

One `Source` instance per mailbox account; multi-account is several instances, each with its own poll loop and send queue. It is the least chat-like channel, and nearly every feature traces back to the same three facts: mail is asynchronous, threaded, and possibly co-read by a human.

- **Two-stage fetch and dedup**: each uid first fetches only the `Message-ID` header (PEEK, so `\Seen` is untouched) and runs the in-memory dedup. Only a not-yet-seen message pays for the full body fetch and the disk write.
- **`\Seen` is the durable queue**: emit first, mark seen after. A message cancelled mid-send during a graceful shutdown stays unread and is re-fetched next boot (at-least-once); the reverse order is at-most-once and silently loses the last in-flight message on every shutdown. An operator can turn the flag write off entirely so a human co-reader still sees every message — and then **no** path may touch it.
- **Error grading**: only a **permanent content defect** (broken MIME structure, no From) is marked seen immediately — re-parsing a broken message every poll is pointless. A **transient** transport or server error leaves the message unread for a reconnect to re-fetch, but that retry is bounded per uid: a poison message failing repeatedly up to the cap gets marked seen to break the re-fetch loop.
- **Clamping the dedup ring**: each poll clamps the unseen set to what the ring can hold. Without the clamp, an unseen backlog larger than the ring evicts ids that are *still* in the unseen set, and the next poll re-emits the whole backlog — a domino, every poll.
- **Quote stripping**: two strategies. First, a chunked-substring match against the **previous outbound body** — locate the section of the incoming mail that reproduces the prior reply, then back-walk to a quote-header line and cut. That tolerates line-wrap changes, punctuation tweaks and prefix re-quoting that defeat a regex. When there is no prior body (fresh process) or the hit rate misses the confidence floor, it falls back to multi-locale regexes. Heuristic by design: mail clients commit to no format and a perfect stripper is impossible; the goal is text that stays bounded across N rounds, not byte exactness.
- **Forwarded aliases**: when a mailbox is declared a forwarding target (several aliases of your own domain routed into one inbox), the source resolves the **original recipient** from the forwarded headers and carries it on the event's `ThreadID` — splitting the DM scope per alias and letting the orchestration layer key on the recipient — while leaving `ChatID` (which doubles as the outbound address) alone. The reply's `Reply-To` is set to the alias so the recipient's answer travels alias → forwarder → bob and the thread stays on your domain.
- **Reply-all loop prevention**: the reply's Cc must drop every one of bob's own identities (the account address, any alias under an owned domain, the current alias) — otherwise the forwarder routes the reply back into bob's own inbox and bob answers itself. Being on that list grants **no authorization**; it only widens who receives.
- **Coalescing**: email is not a chat channel — replying to every turn separately spams the inbox. When enabled, a session's per-turn replies are **held and combined into one mail**, sent once the session has been quiet for the window. Keyed by session id rather than scope, so opening a new session flushes the old buffer first instead of merging across the boundary. The buffer is persisted as a file, so a restart doesn't lose a computed-but-unsent reply. Turn-level system notices (denials, onboarding, paused) go through coalescing **on purpose**: a paused sender who fires five messages collects one merged rejection rather than five — coalescing *is* the anti-spam intent. Only non-turn sinks (slash replies, restart notices) send immediately.
- **Outbound recovery log**: between "enqueued to the send worker" and "SMTP accepted it", the rendered message is persisted to a file; a crash, restart, or permanent failure in that window replays it on the next boot within a TTL. The replay is **byte-equivalent** — same headers, same `Message-ID` — so the recipient sees the same mail, not a duplicate. It complements coalescing: the coalescer owns the pre-enqueue combined draft, this log owns durability from enqueue until acceptance. Why a file and not a database row: the trunk deliberately gives the email source no DB handle, and a transient buffer whose authoritative history already lives in session messages is the file's weight, not the store's.
- **Header-injection defence**: every inbound-derived header value has CR / LF stripped before interpolation, or a crafted `Message-ID` could inject a `Bcc` onto the reply. Non-ASCII goes through RFC 2047 encoding.

### feishu — the enterprise IM source

Receives events over the SDK's WebSocket long connection (no inbound HTTP route needed) and replies through the IM REST API. Rendering is block discipline: the platform's message-edit endpoint is rate- and lifetime-capped, so live edit-streaming is deferred.

- **Receive queue and dedup**: the WebSocket ack pump is decoupled from message processing — the callback only enqueues (fast) and a fixed worker pool drains under bounded concurrency. The reasoning is causal: a delayed ack is exactly what triggers the platform's at-least-once redelivery, so downloading attachments synchronously on the pump would **amplify** a media storm. A `message_id` dedup table is marked **before** the async dispatch, so two near-simultaneous redeliveries can't both pass.
- **Card vs plain text**: replies are markdown, and a plain text message shows that markup literally, so a **short** reply is wrapped in an interactive card whose single markdown element renders it. But a card silently truncates long content, so a **long** reply skips the card and goes straight to plain text — complete beats pretty (the long case, OCR reports and the like, is plain text anyway). If a short reply's card is rejected for any reason, the same text is immediately retried as plain: degraded to literal markup, never dropped.
- **Fetch the parent once**: deciding "is this a reply to bob" needs the parent message's sender, which is an API call. Parent info is memoised by id, so N images replying to one prompt cost one fetch — and that same fetch serves both the group-noise gate and the quoted context.
- **The group gate keys on "not a DM"**, not on "is a group" — topic groups, and any future or unknown chat type, must be gated too, or the bot answers all the chatter in them.
- **Reaction mapping**: the cross-source contract passes a portable unicode glyph; this platform's reaction API keys on its own enum, so the conversion happens in the source.

### discord — the guild and DM source

Connects over the gateway WebSocket. It deliberately does **not** request the privileged message-content intent: content arrives only for DMs, @-mentions, and replies that ping the bot — which is exactly the group gate anyway (a non-@ guild message would be dropped regardless), so the developer-portal toggle and the verification requirement are avoided entirely.

- **Fail closed on channel classification**: a guild-less channel may be a 1:1 DM or a multi-person group DM. Only a **confirmed** 1:1 is ungated; a group DM — and a channel whose type lookup transiently failed — is gated as a group. A missing lookup must never make bob answer an un-@'d multi-party message. The type is immutable so it is memoised, but **only confirmed results are cached**; a failed lookup stays uncached rather than poisoning the cache.
- **Inlined replies**: the platform inlines the referenced message (author included) in the event, so "replied to bob" is decidable with no extra fetch and no message-content intent — only the referenced *content* would need it. A **forward**, however, also carries a reference pointer and must be excluded: treating one as a reply would tell the agent the user "replied to" content they merely forwarded.
- A thread is its own channel and therefore its own scope, never folded into its parent.

### bridge — the in-process "internal" source

No upstream wire protocol at all: in-process callers (the `send_message` tool, cron, proactive delivery) push a `MessageEvent` straight into the gateway through `contract.MessageEmitter.Emit`, and the gateway processes it **exactly as if it had arrived from a real chat**.

Why a Source-shaped object rather than a bare channel: `Source.Run` plus the event channel is the single canonical inbound path — scope resolution, slash dispatch, sid resolution, the turn. Reusing it makes an internally-emitted message **indistinguishable** from a human-typed one at every level below the gateway.

It is bidirectional: `NewSink` is the **return leg** of an agent→agent dispatch. An internally-woken worker session's reply must reach the caller rather than being dropped, so the sink buffers the turn's content and, on `Finish`, emits one internal event addressed at the caller's scope — the caller resumes with the worker's product as an ordinary inbound message. The destination scope is computed by the session core and handed in on the target, so the bridge itself makes **zero decisions and imports no orchestration package**.

Loops (A→B→A) carry **no mechanical cap by design**: a count or rate cap would kill a legitimately-looping agent, since the two are structurally identical. A degenerate loop instead ends at the **model's** judgement — a woken worker with nothing to add calls `stay_silent`, so this sink is never finished, nothing is returned, and the dispatch chain stops. Who can be reached at all is bounded earlier, by organisation permissions.

`Emit` is non-blocking: a full buffer returns a retryable busy signal so a slow downstream never blocks an emitting tool.

### local — the terminal source

A small REPL that feeds the bus exactly the way an IM platform does and prints the reply. Its rendering discipline is terminal append-printing and stays bespoke **on purpose** — it is not an external interface an inbox connects to.

The interesting part is concurrency: each `NewSink` mints its own stream channel and appends itself to a FIFO, and the renderer consumes one sink at a time in registration order. A FIFO rather than a single slot because sinks and their consumers are asynchronous: a turn stalled on the store may open its sink *after* the user has already dispatched the next line, so two turns' sinks can be alive at once and a single slot would let the late one shadow — and lose — the real one. A companion "owed" counter and catch-up loop render late replies *as* late replies, each under its own header, instead of misattributing them to the new line.

A discarded sink may be carrying the product of an **internally-woken** turn (a worker return, a cron delivery), which must not be dropped silently: its buffered content is drained and printed with a header, so a terminal user doesn't have to go digging through session history for a background answer.

`Run` returns `io.EOF` when the user presses Ctrl-D — the bus records that as `stopped`, not `dead`: not an error, but the receive loop really is gone and the panel must stop showing green.

---

## Design rationale: layered registration

A source is **not** a peer module on the trunk. It hangs off stoma.

The reason is simple: a source is meaningless without the gateway. Its entire existence is "push inbound events into that fan-in stream, render replies back to that platform" — the gateway on both ends. Registering it as a trunk peer would invent N nodes on the architecture graph that talk to exactly one module, and `wantGraph` would turn red every time a platform is added — a red that reviewed nothing.

So the plugin registration surface sinks one layer down: `sources.Assemble` is the platform wiring point, `stoma.New` is their host, and the trunk sees one `contract.Gateway`. Two consequences follow directly:

1. **The contract layer does not grow when a platform is added.** A source and the gateway are a **direct** coupling, so shared types live in the owning side and are imported directly by the other. Only what is genuinely trunk-mediated — the `Gateway` interface itself and the `MessageEvent` / `Target` / `Sink` envelopes flowing through it — earns a place in `contract`.
2. **Platform names appear in exactly one file.** stoma proper is platform-blind: it talks about sources, fan-in, and probes. Adding a platform touches neither the gateway nor any layer above it.

The same pattern recurs inside: `streamsink` hosts the rendering discipline, `sendgate` hosts the throttling discipline, and neither registers on the trunk. Whoever owns a family of "meaningless without me" plugins hosts them.
