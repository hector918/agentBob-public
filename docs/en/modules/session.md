# session

session is the single authority on which conversation an inbound message belongs to, and the arbiter that keeps one session to one turn at a time. A conversation's *continuity* is the session, and history is the carrier of that continuity — so history belongs here too.

---

## Where it sits

**Provides**

| Capability | What it is |
|---|---|
| `contract.SessionManager` | The subsystem's front door: `Submit` takes one inbound event, `RecoverPendingOnBoot` clears the in-flight records the last shutdown left behind, `RunningAtScope` reports how many turns are live at a scope |
| `contract.MessageStore` | The conversation-history read/write face — the **only** coupling face between turn and session |
| `contract.ChatHistory` | A read-only browsing view addressed by chat scope (an admin paging an inbox's log) |
| `contract.MessageIndexer` | The write side of the "delivered bot message → sid" index |
| `contract.SessionResume` | Wake a session to continue its task (the hand-back after an external event finishes) |

**Needs**: `contract.DB` (the shared pool) · `contract.Gateway` (resolve a live source by name for the manager's pre/post-turn I/O: reactions, transient notices, reply targeting) · `contract.PanelRegistry` · `contract.SlashRegistry`

**Soft edges** (`TryRequire`, absent → degrade, invisible to the dependency graph): `contract.Turn` (the `/session` context gauge) · `contract.TurnHandler` (whoever actually runs turns) · `contract.Transcriber` (inbound voice) · `contract.Accounts` (`/whoami` shows the bound account) · `contract.Agora` (virtual-group member routing) · `trunk.Housekeeper` (periodic sweeps).

**Upstream**: [stoma](stoma.md)'s gateway feeds platform events into the [inbound flow](../flows.md), which calls `Submit`.
**Downstream**: [turn](turn.md) reads and appends history through `MessageStore`; the [flow router](../flows.md) supplies the reverse soft edge `TurnHandler`; [agora](agora.md) and [arrangement](arrangement.md) dispatch work through `SessionManager`; [webui](webui.md) uses `SessionResume` for the browser-takeover hand-back.

The seam between session and turn is **bidirectional but asymmetric**: session hard-provides `MessageStore` (turn hard-Needs it, so session starts first in the topological order), while turn's side — `TurnHandler` — is a soft, lazily resolved edge. The graph stays acyclic, and "a session is a session even with no turn module wired" stays literally true (a stub then just logs the turn that would have run).

---

## What it does

- **Ownership resolution**: route an inbound event to a scope, and where relevant to a specific sid (reply-to-continue, a group's `@` opening a fresh one, an internal dispatch naming its target scope, an email thread following its reply chain).
- **Arbitration**: at most one turn in flight per sid; messages arriving while busy are queued and drained in batches once the turn ends.
- **The submit path**: fold voice transcripts into the message text, place attachments into the scope's space, acknowledge receipt, dedupe repeats, cap and drop on overflow.
- **The busy queue's visible layer**: queued / running-count / cleared / full notices, plus the **nudge fast-lane** that pulls one busy-arrived plain-text message into the running turn.
- **Message ownership**: the whole conversation history (plain rows, assistant-with-tool-calls rows, tool-result rows, compaction summary rows) is held here and provided outward.
- **Cold archival and revival**: a long-idle session is serialized into an archive row and dropped from the hot tables; a later reply revives one that was still alive when it went cold.
- **Start/stop discipline**: silently reclaim the last shutdown's in-flight records on boot; drain in-flight turns under a deadline on stop.
- **Commands and panel**: `/new`, `/session`, `/whoami`, `/close-session`, `/trace`, `/looping`, `/uncensored`, `/thinking`, `/prompt`, plus the webui session panel.

---

## Internals

### The lifeline of one inbound event

```
gateway event
  → resolve ownership      a scope; and where relevant a specific sid
  → transcribe voice       sound folded into the message text
  → place attachments      staged files moved into this scope's space
  → read acknowledgement 👀
  → admission gate         blocked → queue + surface, no turn consumed
  → busy?
      busy → dedupe → queue (internal events priority; a real message past
                              the cap is dropped with a notice)
      idle → claim the sid (busy + a turn token)
               → write the inbound WAL
               → assemble TurnWork → TurnHandler.RunTurn
               → drain the WAL + possibly "handled N messages"
               → drain the queue (next batch / release the sid)
```

Every ordering on that line was pinned down by a specific failure: transcription must precede placement (which clears the local path), placement must precede any member fan-out, and the WAL write must happen at the moment a message genuinely goes in flight rather than at submit.

### Three coordinates: scope, sid, message index

**Scope** is the routing coordinate, minted by exactly one grammar (`contract.ScopeFor`): a DM is `<source>:dm:<chat>` (optionally with a `#<thread>` sub-scope), a group is `<source>:group:<chat>`, and an internal dispatch carries the target scope verbatim. Virtual groups add a member sub-scope, `<group>#<member>`. Both sides of the grammar — write (`ScopeFor`) and the partial inverses (`TargetForScope`, `SplitMemberSubScope`) — live together, because a second parser elsewhere is exactly how two sides drift apart.

**sid** is session identity. A scope may hold many sessions, but exactly one is **active**, elected by cursor (`cursor_set_at` first; `started_at` is only a tie-break among rows with equal cursors). Delegated sub-sessions are excluded from the election.

**The message index** records "(scope, platform message id) → sid": a reply landing on one of bob's own messages reconnects to that message's session. The write side is `MessageIndexer`, called by a flow after delivery (only the flow holds both the sink's sent message id and the wire scope); the read side lives inside the resolver. The index is also the **authority** on "is this one of bob's messages" — a hit means the reply targets bob, so no extra flag from the source is needed (some platforms do not reliably inline the referenced message's author).

### Ownership resolution: one entry point

The base routing rules are pure and store-only — and **private**. The single public entry point is `Manager.ResolveSession`, which wraps those rules with member sub-scope routing and cold revival. Privatising the base makes one class of bug structurally impossible: a caller reaching past the overlay and getting an incomplete answer.

The rules themselves:

- **Internal dispatch**: `ChatID` *is* the target scope; a dispatch frame may ask for a fresh session (force-new only **supersedes** the active one — sessions are never closed by design).
- **DM**: continue the scope's active session by default. Entry points that opt into per-reply routing (email) walk the reply-ancestor chain against the index; a hit continues that session, a full miss is a fresh compose → a new session.
- **Group**: one group is one scope holding many sessions. A reply that hits one of bob's messages reconnects to that session; anything else — a fresh mention, or a reply to a message with no recorded session — is **`@`=new**. To continue an old topic you *reply* to it. The rule exists so a single group session can't grow forever → compact repeatedly → drift.
- **A read error is not a miss**: when an index lookup fails, the resolution degrades to "continue the active session", never to a fresh one — one database hiccup must not fork a live thread in two.

`ensureLive` is the last step: if the named sid is no longer in the hot table, try to revive it from the archive. One that was alive when archived is rehydrated in place, keeping its sid; one that was ended or purged is not revivable, so a fresh session is opened in the scope instead. Two concurrent replies to the same archived session collapse onto one revival via singleflight, so the transcript can't be double-inserted.

Read-only slash commands route through `readScope`: the same member sub-scope resolution, **without** the revival — a peek must not drag a cold session back just by looking at it.

### The arbiter: one turn per sid

The arbiter is a set of purely in-memory per-sid maps behind one mutex. It knows **when** a turn may run; `TurnHandler` knows **how**.

| Map | What it holds |
|---|---|
| `busy` | a turn is in flight on this sid |
| `pending` | events queued while it was busy |
| `nudged` | message ids the fast-lane pulled this turn (drained with the batch at settlement) |
| `dropped` | this sid's queue overflowed; the next turn announces it |
| `sidMeta` | scope + busy-chain start + turn token + the flow-declared mode |
| `closing` | `/close-session` is draining this sid: abandon its queued work |
| `cancels` | the lever that cancels the in-flight turn |
| `armedPrefer` | model tags armed per (scope, issuer) that no turn has consumed yet |

All of it is **in-process** state: none of it needs to survive a restart. The durable half is exactly three things — the session lifecycle row, the inbound WAL, and the reply index.

One turn's lifeline:

1. Claim the sid (set busy, mint a monotonic turn token, stamp scope and start time).
2. Write every message this turn is about to run into the inbound WAL — **at the moment it genuinely goes in flight**, which uniformly covers the first turn and every drained follow-up.
3. Assemble the `TurnWork`: reply target, the scope's sink prefs, the session's birth-stamped turn mode, sticky model tags, and three callbacks (`OnMode` lets the flow self-report the session mode it imposes, `OnOutput` lets it self-report that it actually delivered something, `NextNudge` is the fast-lane's tap).
4. Run the turn under a child context whose cancel *is* the `/close-session` lever.
5. On return, drain the WAL rows it served, emit a "handled N messages" acknowledgement where warranted, then drain the queue.

Picking the next drain batch is deliberate: an image-bearing or media-group batch is processed **one event per turn** (so the agent sees each image cleanly), while plain text is batched **by sender**. A group is one shared sid, so its queue mixes several people, and the flow router routes a batch by its *first* human event — a two-sender batch mishandles the non-anchor.

**The turn token is the ownership proof.** Every cleanup path (normal drain, panic recovery, `/close-session`) compares tokens before touching anything, because otherwise it would be stealing a fresh turn's slot.

**Panic recovery** does more than clear the busy bit: it **redispatches in-process** the messages that queued during the panicked turn, under a new token, instead of stranding them until the next restart. Bounded — each distinct message runs at most once in-process.

**The reply target follows the session's destination, not the triggering event.** This is load-bearing: an internal wake (a worker's return, a cron tick) carries an internal source and the woken session's own scope, so naively addressing by event would send the reply back at the session itself — an unbounded self-feeding loop. Hence: a real chat event → thread the reply to the chat; an internal wake with a caller → return through the internal bridge to the caller's scope; an internal wake on a source-bound session → reconstruct the delivery target from its own scope; nothing resolvable → drop, never self-address.

**Pre- and post-turn I/O speaks through the batch's own source**, not through the source that dispatched the turn. A drained or redispatched batch may have queued behind a turn triggered by a *different* source (an internal bridge wake), and that bridge's capabilities are wrong for humans' messages — the reply anchor is dropped and the send is a no-op, so the "handled N messages" acknowledgement silently vanishes. Each batch therefore re-resolves the source of its first non-internal event.

**Concurrency is counted from the busy set on demand**, with no separate counter: `RunningAtScope` scans it for matching scopes. The set is bounded by live turns so the scan is cheap — and with no second piece of state there is nothing that can drift across the panic-recovery and follow-up-drain branches.

**Admin ownership is a promote-only bit**: once an admin has addressed a session, it is marked admin-owned (idempotently — already-set is a no-op, so it doesn't amplify into a write per admin turn). It is read in exactly one situation: a **purely internal continuation**, where the batch holds no human event at all (a dispatch return, a scheduled wake). Such a batch has nobody present for the flow to judge authority from, so it inherits the session's persisted bit; the moment a human *is* present in the batch, that person decides and this bit stays out of it.

### The submit path

`Submit` returns promptly (the work runs on its own goroutine), so the gateway's consume loop is never blocked. Then, in order:

- **Resolve ownership** → a scope.
- **Voice transcription** (soft edge) happens here, **before placement and before persistence**, folded into the message text. Two halves, two positions: an *instruction* (the sender's own captionless voice note — it *is* the request) replaces or leads the text; *material* (a captioned clip, a replied-to message's clip, a video soundtrack) is somebody else's words, so it always goes *after* whatever the user actually said and may never occupy the request slot.
- **Attachment placement**: staged files move into the resolved scope's space, **once per event** and before any member fan-out, so every downstream reader sees the space-relative path.
- **Read acknowledgement**: one 👀, which also covers all the busy / blocked / dedup branches below. Pure UX, deliberately not part of the shutdown drain.
- **Admission gate**: when `TurnHandler.Blocked` is true, no turn is consumed — the message is queued and surfaced to whatever is holding the session.
- **Busy branch**: internal events get queue priority (they bypass the cap — a worker's product is a system reply, not user spam, and dropping it loses work); a real chat message past the cap is dropped with a notice. A same-sender same-content repeat is skipped silently.
- **Idle branch**: claim the sid and run the first turn synchronously.

There is a narrow window between "set busy" and "register the cancel", into which a `/close-session` can land; the submit side handles it with bounded retries and a short backoff, and when the retries are spent it *tells the user to resend* rather than dropping the message in silence.

Two kinds of event collapse repeated "open a fresh session" onto **one**: media-group album siblings (the source emits one event per attachment, so naively that is N sessions running N concurrent turns, defeating the one-image-per-turn drain) and onboarding-pass senders (an unapproved stranger's repeated mentions would otherwise mint one empty session row each, bounded only by retention). Both ride the same singleflight plus a short-TTL reuse cache.

### The busy queue and the nudge fast-lane

The queue's **visible layer** is gated on the session's flow-declared mode: an automated session (an agora worker) stays silent throughout, and only a session with a human present gets notices. An unknown or empty mode is treated as silent too — better to under-speak than to narrate at an automated flow.

**Dedup is a same-person double-tap guard**, not content equality. Two people in a group both queuing "go on" are two distinct inputs; dropping one loses the second speaker's turn and their attributed row. Attachment-bearing events are stricter still: only a **true redelivery** (the same message id) counts, because an album's siblings share one caption and — outside the photo path — may share a static fallback filename, so content equality alone would swallow a genuinely different file. An uncaptioned attachment is never deduped: each one is its own unit of work.

**Album de-noise** is the other half of the same problem: a ten-image album emits ten events, and naively that is nine "queued" notices. So only the album's **first queued sibling** speaks; later siblings sharing its media-group id stay quiet (the 👀 still acknowledges each). The matching half on the drain side is that single-event drains emit no cleared acknowledgement — an N-image album drains one per turn, and acknowledging each would be spam.

**Onboarding-pass senders stay quiet throughout.** Their session runs a flow whose whole restraint is "one canned reply, then silence", and a queue notice on top of that is misleading noise — the reply *is* their feedback. For the same reason they get no 👀 either: a "seen" signal with no turn behind it is not telling the truth.

The **nudge fast-lane** pulls one busy-arrived plain-text message into the running turn. The eligibility rules are narrow: plain text only (images and audio need preprocessing and stay on the drain path), never an internal event (a system reply must not impersonate the user), and it must have arrived **after** the turn started — the backlog already queued when the turn began drains in order, and the fast-lane does not reorder it. At most one per turn.

What makes that safe is the consumer: turn persists the pulled message as a **real user row** before showing it to the model, so it survives in history whether or not the model addresses it. Previously it was injected into the system prompt as a transient layer, which meant a model that ignored it destroyed the message outright — already taken from the queue, no WAL row, no trace in history.

### Waking a session: SessionResume

When an external event finishes (the browser-takeover hand-back is today's only caller), a session needs to be woken to continue its task. The policy is applied *here*, not decided by the caller: **only an automated session is woken** — it has no human present to re-trigger it, while a session with a human is left for that person's next message and this is a no-op.

The wake is not a bespoke resume hook. It resolves the sid's scope and emits an internal event, which is processed as a genuine inbound (scope → sid → a fresh turn), so the note arrives as ordinary turn input and no extra machinery is needed.

Two details were forced by real failures. The wake first confirms the sid is **still the scope's active session** — a member scope is shared with one active session, so if something force-newed during the waiting window, waking the scope now would land the hand-back in the *wrong* session; better to drop it with a log line. And a busy internal bridge is retried a bounded number of times: an automated worker has no human to redo it, so a dropped wake would wedge it forever.

### Cold archival and revival

The archiver lives entirely inside this module: the lifecycle row and the transcript are both session subpackages over the same database, so archival is a single-module operation with no cross-module seam.

- **Archive**: a session whose last activity predates the cutoff (dead or alive) is serialized into one archive row, then its hot message rows are deleted, then its session row. **That order is the idempotency guarantee** — the archive row is written first, is `ON CONFLICT DO NOTHING`, and is removed only by revival or purge, so a crash anywhere is safe to retry. What is archived is the **replay window** (from the last summary marker, else the last N rows), not the full raw history: a turn only ever replays that window, so restore fidelity equals replay fidelity.
- **Skip the in-flight**: the sweep asks the arbiter whether a sid is busy or closing right now and skips it — otherwise it would delete a live session's row and transcript out from under it.
- **Revive**: only a was-alive archive revives. Recreating the lifecycle row, re-importing the transcript, and dropping the archive row happen in **one transaction** — a non-atomic sequence could leave a live-but-empty session shadowing the still-present archive, silently losing the whole transcript.
- **Purge**: archive rows past a hard TTL are hard-deleted together with their reply-index rows. The index retention window is deliberately *longer* than the archive age alone (it must cover cold-age plus hard TTL): prune the index first and revival is dead on arrival — the reply misses the index and simply opens an empty session.

### Retention and sweeps

Five periodic tasks hang off the trunk's housekeeping scheduler (a soft edge — a minimal configuration runs without one and simply doesn't sweep):

| Sweep | What it reclaims |
|---|---|
| Stale in-flight records | Inbound WAL rows older than a day — a normal row drains within a turn, so one that survives a day is crash residue a boot clear missed |
| Reply-index prune | Index rows past the retention window (one per delivered bot message, so it grows without a sweep) |
| Cold-session archive | Long-idle sessions moved into the archive table |
| Compacted-row prune | Message rows before the last summary marker *and* past retention (archival only reaps *idle* sessions; this covers the busy ones) |
| Archive purge | Archive rows past the hard TTL, together with their index rows |

Two of these windows are **interlocked**: the reply index must outlive the archive (covering cold age *plus* hard TTL), or the index is pruned first and a reply to a cold session has no revival path — it silently opens an empty session, which is precisely what the revival mechanism exists to prevent. A cross-sweep timing constraint like that gets broken on the next round of tuning unless it is written down.

### Start and stop

Boot recovery is **silent**: it clears the in-flight WAL rows the last shutdown left, and pushes nothing to anyone ("I restarted, please re-send" was removed). Shutdown waits for every goroutine spawned by submit or drain, under a deadline; on timeout it logs how many sessions are still busy, with the hint that a tool is probably blocked on uncancellable I/O. Messages still queued are warned about explicitly as lost — a queued message never gets a WAL row, so it goes with the process and the sender simply re-sends.

### Commands and panel

Nine slash commands register into the shared [slash](small-modules.md#slash) registry (the table lives there, the logic here). Worth calling out:

- `/session` is a **switchboard**: the resolved scope, the active sid, which flow *would* handle this event and what mode it imposes (a read-only probe — no turn, no persistence), the turn mode's **birth stamp next to the scope default** (they differ exactly when `/looping` was flipped after this session was born and hasn't taken effect yet), the sink prefs, and the context gauge borrowed from turn.
- `/looping` flips the scope's **default** turn mode. That default is stamped onto a session at creation and is immutable for its life — so the toggle affects only future sessions, and no session ever changes driver mid-conversation.
- `/uncensored` and `/thinking` each toggle one soft model tag, armed per (scope, issuer). A scope is shared, so a single slot per scope let one person's toggle silently replace another's; and only the issuer's own turn may consume an arm. Consumption happens at turn start and **merges** into the session's existing stamp rather than replacing it, re-reading under a lock first — so two toggles issued together can't lose one another, and a set computed against a since-superseded session can never be stamped onto this one.
- `/close-session` scans the busy set for the scope's running turn and cancels it. It also drops **every** issuer's pending arm at that scope: close is a boundary the user just drew across the conversation, and carrying anyone's preference across it would be wrong. The reply echoes the stopped sid — a scope can hold two busy sids at once (the active session plus a turn replying to an old message).

The panel reports busy / pending / dropped / tracked counts, plus the **durable** queue depth (a different question from the in-memory one; neither implies the other), plus a table of turns running right now (scope / elapsed / started). "Busy longest" measures the busy **chain**'s age rather than the current turn's — a ten-image album legitimately reads tens of minutes as it drains one image per turn. What it catches is a sid that never gets free.

### Subpackage `messages/`

The conversation-history store. Row shapes: plain user/assistant rows, an assistant row carrying its tool calls, a tool-result row carrying the call it answers, and a compaction summary row. Three fields are JSONB rather than side tables or columns — row attribution (whose shape varies by row kind, and which is always read inline with the row), a user row's structured attachment list (metadata and space paths, never bytes), and tool-call arguments.

Two semantics are promises of the interface, not implementation detail:

- `GetReplay` **anchors on the last summary marker** (a compaction's start) rather than a fixed row count — so the marker is always present and the recent tail is always whole. With no summary yet it falls back to the last N rows.
- `AppendCompactionBatch` is an **atomic batch**: one summary marker plus the recent tail re-appended by shape, committed or rolled back together, so a replay never sees a half-written compaction.

`AppendUserMsgs` is atomic for the same reason: one row per speaker in a turn, all-or-nothing, so a mid-batch failure plus a retry cannot duplicate an already-committed speaker row.

In-place compaction rewrites nothing — the marker and the tail are **appended**, and the pre-marker rows stay in the table. They are dead for replay (which anchors on the last marker) but unbounded for a long-lived, repeatedly-compacting session. `PruneCompacted` is the matching sweep: delete rows that are both before the last marker **and** past the retention window.

### Subpackage `store/`

Session lifecycle, the crash-safe inbound queue, the reply-routing index, and per-scope preferences — three tables on the shared pool.

- **Lifecycle**: the session row is lifecycle-only (creation, cursor, end, title, model tags, admin ownership, mode, turn-mode stamp). "Who is active" is answered by the cursor election.
- **Inbound WAL**: only **in-flight** messages are recorded. Queued, blocked, dropped, and closing-collision paths write nothing — write and consume must be symmetric, since the drain can only account rows by (scope, message id), so an id-less row could never be consumed and would just sit until a sweep. Internal events are excluded too: they are best-effort in-process emits by nature.
- **Cold candidates**: last activity comes from a per-session lateral "newest row" lookup (timestamps are monotonic with the serial id), so the cost is driven by the smaller session table rather than by the whole message table; a per-sweep limit lets a large first-run backlog drain over successive sweeps.

---

## Design rationale

### Ownership has exactly one door

The base routing rules are private; the only public face is `ResolveSession`. This is not a style preference. With a second door, some caller eventually gets an answer that skipped member routing or cold revival — and that failure shows up as *an attachment landing in the wrong scope*, or a reply silently opening a new session. Both are very hard to trace back from the symptom. Welding the overlay to the single door makes that class of bug structurally impossible.

The read-only peek commands take the **non-reviving** variant of the same primitives, sharing the member sub-scope computation, so the two paths cannot drift.

### One group is one scope, many sessions, one active

The alternative — one permanent session per group — grows forever, compacts repeatedly, and eventually drifts semantically past repair. **`@`=new plus reply-to-continue** hands the topic boundary back to the user: mention to start something, reply to continue it. The price is needing a reliable "is this one of bob's messages" test, which the message index provides rather than the platform's reply flag (unreliable on some platforms, and a false negative forks a context-carrying conversation into a context-less new one).

### Message ownership sank into session

History used to belong to turn, because turn is what reads and writes it. But turn is the short-lived "read the tail and append" executor, while **conversational continuity** is the definition of a session — and history is the carrier of that continuity. In engineering terms: turn touches history only through `MessageStore` and never builds its own store; a new session implementation may freely change *how* messages are stored (schema, index, backend) but may not unilaterally change the shared data shape or the replay/compaction semantics — those two are part of the interface, not leakage through the seam.

The same implementation is offered as `ChatHistory`, a **separate, narrower** interface, rather than by widening `MessageStore` — the turn-hot write path should not grow because an admin read path needed something.

### The WAL records only what is in flight

The crash-recovery boundary is drawn tight: only a message **currently being served by a turn** gets a durable row. Queued, blocked and dropped messages get an immediate in-chat notice instead. The reason is symmetry with recovery: boot pushes nothing to anyone, so a row surviving to the next boot produces exactly one log line and nothing else. The only case that genuinely needs durability is "the user believes it is being handled and the process died"; everything else is said out loud on the spot.

### The queue's visible layer is declared by the flow

One queueing mechanism has to behave completely differently towards a human and towards a machine. session does not guess: the flow handling the session **self-reports** its mode (`OnMode`, called once by the router from its single flow pick), and session caches and persists it. Automated sessions are silent and never nudged; sessions with a human present get notices and the fast-lane.

The "handled N messages" acknowledgement carries one extra gate: it fires only when a flow **actually delivered** something (`OnOutput`). Merely *selecting* a flow is not enough — some flows' dedup no-ops select themselves and produce nothing, which would make that acknowledgement a lie.

### A session is a session with no turn

`TurnHandler` is a soft, lazily resolved edge with a stub behind it. That is not defensive decoration but a statement about the module boundary: a session that never runs a single turn is still a session — it has ownership, history, and a lifecycle. The arbiter knows nothing about *how* a turn runs, only about *when* one may. That seam is the **only** thing connecting the two cores.
