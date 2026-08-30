# turn

turn is the turn-execution mechanism: given a `TurnSpec` composed by a flow, it drives **one** bounded turn end to end — read history, assemble the messages, call the model, stream the reply to the sink, persist the exchange. It knows nothing about which flow drove it.

---

## Where it sits

**Provides**: `contract.Turn` — a single method, `Run(ctx, TurnSpec) TurnResult`.

**Needs**: `contract.MessageStore` (owned and provided by [session](session.md)) · `contract.ModelPool` (provided by [model](model.md)).

turn does not touch the database. Its only database use was history, and history belongs to session — so what remains is one consuming edge through a contract interface. `MessageStore` is a **hard** Need: with no history there is no meaningful turn. The reverse edge (session needing someone to actually run turns) is the soft, lazily resolved `TurnHandler`, invisible to the topological sort, so the graph stays acyclic.

**Upstream**: [flow-normal / flow-agora](../flows.md) compose the spec and drive the core; [session](session.md)'s arbiter decides when this turn's slot opens.
**Downstream**: [model](model.md)'s pool (chat, streaming, tokenizing, window sizing); and the tool set, skill set, channel and credential openers carried in the spec — projected and authorized by the flow via [tools](tools.md) / [warrant](warrant.md) / [skills](skills.md). The core dispatches; it never judges authorization.

The division in `TurnSpec` is sharp: **the flow composes the spec, turn owns everything else**. Notably `Prompt` arrives as a *builder*, not a built string — the core re-Builds it every round, so a multi-round turn automatically picks up the growing history and the round's transient layers.

---

## What it does

- **One turn's full lifecycle**: persist the user rows first, then iterate rounds (model → tool dispatch → repeat), then exit through one of the doors, releasing the sink exactly once.
- **Two loop drivers**: `regular` (a budget-bounded reply loop) and `looping` (a convergence-driven work loop), over one shared round kernel and one shared guard family.
- **Tool rounds**: capped parallel dispatch (with an opt-out), a fixed result pipeline (redact → cap → summarize → empty-guard → persist with one retry), and orphan repair.
- **Context compaction**: sliding-window segmentation, per-segment summarize, then a merge — with triggering and splitting both measured by **exact token counts** from the serving backend's own tokenizer.
- **Salvage**: a turn that ran its budget dry or hit a guard still owes the user one readable line, earned rather than templated.
- **Degenerate detection**: abort a small model's repetition wall mid-stream, and let an already-poisoned history self-heal on every rebuild.
- **Guards and a progress predicate**: same-call repetition, per-tool failure streaks, a no-progress budget — all keyed on **staleness**, never on raw round count.
- **Acceptance gates**: a mechanical RUBRIC check for skills that ship one, and an advisor review built into the looping mode.
- **Sub-loop delegation**: a depth-1, blocking, serial sub-turn on a clean context that hands back only its product.

---

## Internals

### The lifeline of one turn

```
Run
  ├ nil-sink check · panic backstop
  ├ persist the user rows first (one per speaker, atomic batch)
  └ dispatch on Mode once → the driver's loop:
        loop top  compaction (weighed trigger) → arm the read-back tool
                  → harness nudge ladder → user-nudge fast-lane
                  →〔looping: stall diagnosis〕
        a round   rebuild the prompt → advertise tools → one streaming call
                  ├ no tool calls → final-reply door →〔gate〕→ persist + Finish
                  ├ tool calls    → persist assistant row → dispatch
                                    → result pipeline → continue / exit
                  ├ oversized     → force-compact → retry the round
                  └ degenerate / bloat / cancel / stream break → their own doors
  shared teardown  budget dry → cancel door (silent) → salvage
```

The **order** of those four loop-top steps is reasoned. Compaction runs before the round reads history (and hands the replay it read to the round for reuse, so one round issues one history read). Arming the read-back tool follows compaction immediately — after writing a batch the replay is re-read so the *same* loop top arms against the fresh summary. And the stall diagnosis sits *after* the nudge ladder, so a turn that is already exiting on the budget guard never pays for a consult it cannot use.

### The shell: the skeleton of one Run

`Run` is the shell every driver shares, and it does exactly four things:

1. **Precondition**: a spec with no sink cannot deliver a reply — return a contained error rather than nil-panicking deep inside a round.
2. **Panic backstop**: no uncaught panic anywhere on the round path may leave the user in silence — log with stack, Finish a notice, return the error.
3. **Persist the user rows first**, before the model call. Every round's message list is rebuilt entirely from history, so a failure here means the model would never see what the user just asked — it would answer against stale history, silently. So this failure aborts honestly. (If the reason is an already-cancelled context, that is not a storage fault: exit silently, but still release the sink.)
4. **Dispatch on mode once**: `spec.Mode` is read here and **only** here. The round kernel and the entire guard family never read it — they are pace invariants shared by every driver. An unknown mode deliberately drives regular: fail-safe, never a half-built path.

A driver returns `(result, done)`. On `done=false` control falls back to the shell's shared teardown: the default iter-cap exit → the cancel door (silent) → salvage.

### Two drivers, one kernel

This is the module's central design choice: **never branch on Mode**. The two exit semantics are two *policies*, one file each; the round kernel, the guards, compaction, weighing and the result pipeline all stay single-copy and shared.

| | `regular` | `looping` |
|---|---|---|
| The designed exit | one final reply | a **converged product** (the model claims done and any armed acceptance gate passed) |
| The round budget | a legitimate exit — running dry salvages an honest answer | degrades to a **fuse** — hitting it is pathological, not a normal ending |
| Default cap | 40 rounds | 500 rounds (the fuse) |
| Sink policy | stream live | **quiet**: content deltas dropped, trace passed through, the converged product delivered in one Finish |
| Advisor | none | built in (stall diagnosis + pre-delivery review) |

The two loop skeletons are deliberately near-identical. That is both the cost and the point of the split: `looping`'s exit semantics can keep evolving without `regular` ever seeing the diff.

The quiet sink additionally **holds pictures**. A work turn runs for minutes; streaming every intermediate image out is a flood — so pictures are held until the turn's outcome is settled and then released in one burst, including the attempts a gate rejected (nobody can tell the model's third try from its first without eyes, so that judgement is the user's to make). The release happens at two points (Finish, and the driver's deferred drain), both idempotent — because salvage and the loop-top cancel door finish on the *raw* sink and never reach the wrapper's Finish, while a panic is the one path no explicit hook can cover.

### The round kernel

One round: rebuild the prompt → advertise the turn's authorized tool specs → make one streaming model call rendering content to the sink → resolve. No tool calls → the final-reply door; tool calls → the tool branch.

The stream watcher does four things at once:

- **Forward content only while no tool call has emerged** — once the model starts a tool call, the text before it is preamble, not the user-visible reply.
- **Withhold plain tool-call markup.** Some local models emit tool calls as **text** (the pool only extracts them after the stream closes); without this the whole markup streams to the user raw. And the marker can arrive **split across deltas** — so the trailing suffix that could still grow into the marker is withheld byte-exactly until the next delta either disproves it (flush) or completes it (discard).
- **Thinking rides the trace channel, never the content channel** — it is process, not product. That is also its entire gating story: trace is dropped at the sink contract when the scope's trace preference is off, and it is excluded from `Finish(full)`, so a thought never lands in the reply, in history, or in the next round's context. Reasoning arrives one token per event while every trace call terminates with a line break, so it is flushed on **sentence boundaries** — otherwise it prints one word per line. On a round whose product is buffered (a gate hasn't judged it yet, or the quiet driver may supersede it), the thinking is marked as a draft: a number read out of the trace must never be mistaken for the answer that finally ships.
- **Repetition-wall detection** on content and thinking **independently**, each on its own byte counter. Watching content alone is blind to a model that loops before emitting a single character — a reasoning delta counts as liveness, so such a runaway would run all the way to the provider's hard cap.

Failures outside the stream each have their own door: cancellation (visible content already shown → persist it with an honest "cut here" marker; nothing shown → full silence), a broken stream (same, preserving the half-answer the user saw rather than clobbering it with a notice), an args-bloat abort (a chunk-retry ladder), and a context overflow (force-compact and retry the round).

**Termination discipline**: every door ends through `finalizeTurn` (persist the assistant row, then Finish) or `releaseSink` (`Finish("")`), so the sink is released exactly once. `finalizeTurn` persists on a **durable context** — the turn's own token may already have died during a multi-second judge call, but what is finished and what is persisted must agree, leaving no hole in history. It also **returns** the Finish error: a turn whose delivery failed outright must not be booked as a satisfied answer, or downstream would record a phantom "answered" and never resend.

The **Sink contract** is the source of several fine details: `Finish`'s full must extend, byte for byte, the content deltas already sent. So the kernel tracks exactly how many bytes went out, and the final-reply door — when the reply streamed — composes its Finish text from the *exact streamed bytes* (untrimmed) plus the un-streamed footer, while the *persisted* row stays the trimmed reply (the preamble was already persisted with its own round's assistant row; persisting it again would duplicate history). Sinks that declare themselves bare-product (a sub-turn's capture, an agent-to-agent return, an email coalescer) take the other path: their Finish full *is* the product and must never have preamble glued on.

### Tool rounds and the result pipeline

The order is fixed, unskippable, and mechanism rather than policy:

```
dedupe + backfill ids + neutralize malformed args → persist the assistant-with-calls row
→ dispatch → redact → cap → summarize → empty-guard → persist the result row (one lock retry)
```

Several choices are deliberate:

- **The assistant row is persisted before dispatch.** A crash mid-dispatch then leaves a recoverable trail. The reverse order would leave result rows with no parent — *reverse* orphans, which forward-only repair cannot heal and which a strict template rejects on replay. So a failure here aborts instead of dispatching.
- **Backfilled synthetic ids must be unique across the whole replay window**, not just the round: orphan repair keys on the id alone, so a repeated id would let an earlier round's result row mask a later round's orphaned call — exactly the crash gap the repair exists to close. The id carries a wall-clock component so a post-restart turn's ids stay distinct from the persisted orphan it must repair.
- **Malformed arguments are neutralized to `{}` in place.** A truncated arguments JSON would otherwise be persisted verbatim and **replayed to every model on every later round**; a strict server rejects the whole request, so one bad call poisons the pool for the rest of the session. After neutralization the tool's own required-field check returns a clean error the model can retry from.
- **Redact before capping.** Capping first would leave a credential that straddles the boundary with an un-redacted head (redaction's minimum-length anchors can't match a truncated tail). And redaction must precede summarization: the summarizing model may be remote, and raw secret bytes must not reach that wire.
- **Summarize only oversized successful payloads**, and only when the tool didn't opt out (verbatim tools like file reads and patches). Any failure or empty summary falls back to the verbatim payload — never an empty or lost result.

Dispatch is parallel by default under a cap, with tools that declare serialization run after the parallel batch. An already-cancelled turn dispatches nothing further: some side-effecting tools never inspect their context, so without this gate a message would be sent *after* the user cancelled. Every trace line — one before each call, one after each result — is emitted by the **main goroutine** in call order, so parallel tools' output never interleaves.

`buildMessages` rebuilds each round and performs three window repairs there: replace a degenerate assistant row's content with a marker **in the replay slice** (not the store), so an already-poisoned history self-heals on every rebuild; drop **reverse** orphan tool rows severed by a tail cut; and splice a synthetic result after any assistant call that has none. That synthetic result deliberately does *not* assert failure — it says the call may have completed and must not be blindly retried, because a flat "failed" would make the model re-fire a side-effecting call.

### Per-turn state

One private structure that lives and dies with a single `Run`: the exit marker, accumulated usage, the tools this turn ran, the folded user nudge, two progress watermarks (**progress** vs **production**), guard counters, the bloat ladder, the side-effect failure ledger, the acceptance gates' arming and retry counters, and the exact bytes already handed to the sink.

The exit state is a **label, not a branch key** — the only thing a driver branches on is cancelled (silent) versus everything else (salvage). The zero slot is deliberately reserved for "unset": cancelled used to sit there, which made any exit that forgot to set its state read as a silent cancel — skipping salvage and returning nothing to the user. It now falls through to the default: loud, not silent.

Outward, the detailed internal exit reason maps onto a `TurnOutcome`: `Final` (a clean answer, the only one that counts as satisfied) · `Process` (a deliberate non-answer: ask a clarifying question, hand off, stay silent) · `Degraded` (a best-effort give-up) · `Cancelled` · `Error`. That classification is the first-class signal every cross-layer decision reads: which gate applies, whether a flow counts the turn as answered, whether failure learning is fed, what a delegating parent sees.

### Progress predicate and guards

Progress is defined as **new information**: a successful tool call whose result hash differs from that tool's last one is progress; a tool that declares itself a delivery tool succeeding is production (and also progress). Three guards: consecutive identical successful calls (the model spinning in place), a per-tool failure streak (bombarding one broken tool with varying arguments still counts), and an exhausted no-progress budget.

The whole guard family and the whole nudge ladder key on **staleness** — rounds since either watermark last advanced — never on raw round count. That is precisely what lets both drivers share them verbatim: a turn still producing new information is never nagged however deep it runs, and a stalled looping turn dies at the same staleness however large its fuse.

Failed side-effect tools are tracked separately (by name plus arguments, superseded when the same call later succeeds); whatever remains becomes an honest footer on the reply, contradicting a model that claims the action succeeded.

### The nudge system

Both kinds of nudge are applied at the same point — the loop top — but they are **different kinds of thing** and no longer share a lane:

- **Harness nudges** (the staleness ladder: try a different approach → stop calling tools and answer honestly → the no-progress menu → a grace period, then exit) are machine-generated steering text riding a transient prompt layer, never persisted. All of them are wrapped in an internal marker so a small model won't paste them verbatim into a user-facing reply.
- **The user nudge** — the human message that arrived while the turn was running and was pulled in by the fast-lane — is conversation *content*: it is persisted as a real user row and spliced onto the round's replay, so it reaches the model as the conversation's newest user message.

The latter used to ride the transient layer too, and two problems compounded. **Position**: a human's question sat where a model reads standing background, competing with the discipline layer telling it not to break off mid-task and with a window full of tool output. **No record**: it reached no store at all (a queued message gets no WAL row either), while the queue side had already consumed it — so a model that simply didn't address it destroyed the message. Persisting it fixes both halves at once.

### Routing tags and cache affinity

Every round's model request carries the session id as an **affinity key**: the pool then prefers to keep this conversation on the backend that last served it, round to round and turn to turn (remembered per key, with a TTL). It is only a load-balancing tie-break and never overrides failover or spreading across equal peers — but for prefix caching, that one tie-break is the difference between a hit and a miss.

A tool round additionally **joins** (never replaces) two soft tags: one preferring a model that is stable at tool calling, and — once this turn's failures pass a threshold — one preferring a stronger model. Both are soft, so a missing tag simply doesn't bias anything. This too speaks the staleness vocabulary ("failures since the last progress"), so a long turn that recovers **sheds** the bias instead of wearing it for the rest of its life.

Joining a tag clones before appending: the request's preference slice shares its backing array with the flow's own, and a bare append could write into flow-owned data — routing tags must never mutate an upstream slice.

### User-facing notices and language

The handful of notices the core renders itself (model unavailable, conversation too long, persist failure, internal error, the cancelled marker, the stream-interrupted marker, the salvage floor) come from the i18n catalog, in the reply language the flow resolved at ingress; with none set, the core detects from the user input at hand. It grows no new contract field for this — the text the user just sent is the honest signal already in hand.

### Acceptance gates

Two gates, **either/or on one delivery, never both** (one product judged by two standards doubles the cost and can produce two verdicts).

**The RUBRIC gate.** When the model reads a skill that ships acceptance criteria, that reading *is* the "I am using this skill" signal, and the skill's criteria are armed. At the final-reply door an LLM-as-judge call (routed to a model tagged for judging) checks the product against the evidence; a fail sends it back for a bounded redo, and past the cap the turn declares failure rather than shipping ungrounded output. The judge only checks the product against the evidence the turn already has — it cannot tell whether that evidence is itself correct, and the criteria say so. Arming is per-turn and never persisted, so compaction cannot disarm it.

A blind fail **escalates once**: an evidence-capable second opinion on the advisor chassis (a depth-1 sub-turn sharing the parent conversation's history), whose verdict **supersedes** — it can overturn a false fail or confirm with a record-backed reason. A pass or a fail-open never escalates (a judge hiccup must not buy a 10–100× sub-turn), so the cheap path stays cheap. At most one per turn.

**The advisor review.** In looping mode, with no rubric armed and when the turn **actually changed something outside the conversation** (a delivery or side-effect tool succeeded), delivery is reviewed before it ships. It runs on the same sub-turn chassis with read access to the parent's full record, checking whether the product's key claims are actually supported. The verdict uses the same JSON contract, so all three roles (blind judge, escalated arbiter, reviewer) share one parser.

Both gates **fail open**: an unavailable advisor or an unparseable verdict passes, with a log line and a trace line — a gate never blocks what it could not verify. But when the review's redo budget is spent and an objection remains, that objection ships as a footer on the delivery rather than vanishing: the artifact already exists, so declaring failure would be its own kind of lie.

In looping mode the advisor has a second role: **stall diagnosis**. An exact staleness match fires one consult (exact, so each stall episode costs at most one), and the direction it returns is parked in a dedicated prompt layer until the next diagnosis overwrites it. The harness owns the trigger, because "how many rounds stalled" is a harness fact the model cannot see. The brief is deliberately thin: only the two numbers the harness knows and the model does not — the advisor pulls the trajectory itself, and a retold summary here would be exactly the biased framing the read-back exists to bypass. The brief carries **no acceptance criteria**: a diagnosis is quoted verbatim into the working model's prompt, so folding criteria in would hand the graded model its own grading sheet the long way round.

At the ladder's "stop calling tools" tier the parked diagnosis is cleared: the harness has ruled that this turn stops working and starts answering, the diagnosis says the opposite, and it sits later in the prompt where it reads as the more recent instruction.

### Compaction

Two layers with a hard contract between them:

- **Upstream owns structure only**: where the cut line falls (keep roughly a third of the budget as recent tail, and never split an assistant-calls → tool-results pair) and how the result lands (a summary marker row plus the tail re-appended by shape, one atomic batch). It never reasons about size beyond the keep walk, and never refuses because of content.
- **Downstream owns compression only**: given text of any size, run **one** pass — cut at logical boundaries with a sliding window, one summarize call per segment, join, plus a final merge call when the join fits the window. A join still over the window is returned as is: it lands as the summary row, and that row doubles as the **work marker** — the next loop top's trigger sees the still-oversized replay and runs another pass. Convergence rides the turn loop; there is no internal `while`, no depth counter, no size-based refusal.

Failure has exactly one shape: a summarize call failed → this round didn't compress (logged), the history is untouched (recoverable), the next loop top retries.

**Exact weighing** underpins every sizing decision: each sizing question asks the backend's own tokenizer for a real count instead of estimating and correcting. The pool owns the count cache (only it knows which tokenizer measured a text); turn owns the degrade posture — the estimator survives *solely* as the fallback for pools with no tokenize path or during a tokenizer outage, one fallback rather than a parallel ruler. A failed tokenize latches a short breaker, so a hung endpoint costs one timeout per window instead of one per text per loop top.

The trigger takes the larger of the weighed total and the **last real prompt reading** the model itself reported: the real reading naturally includes the chat-template overhead and tool specs that weighing cannot see, and on pools without a tokenize path it is the only real number available. After a compaction writes, that reading is cleared — it describes a window that no longer exists.

Sizing must ask about **the model that will actually serve this round**: the loop-top budget is computed with the routing tags a tool round will add, or you get a budget sized against a large-window entry while a small-window entry serves the round — a rejection every tool round.

Several details were forced by real incidents. The kept tail is additionally clamped to **a third of what is actually there** — a dimensionless rule that holds on the fallback ruler too, guarantees the cut lands strictly inside the replay, and makes every reactive pass a multiplicative reduction. The marker is weighed against the rows it replaces on the same ruler, and a pass that doesn't shrink is abandoned (the net for a parroting model that echoes its input, which would otherwise burn one summarize per loop top forever). And the summarize instruction states explicitly that text quoted from tools or web pages is **source material, not fact** — naively summarizing it restates a page's self-description as a finding, which can invert the assistant's own conclusion.

Compaction drops mechanical inbox paths, so a **file manifest** read from the durable structured attachment column (untouched by text compaction) is re-attached to the summary, keeping files the user sent several turns ago reachable afterwards.

After a compaction the replay is **re-read**, so the same loop top arms the read-back tool against the fresh summary — the summary is a table of contents, not a replacement, and the originals never left the store. That tool is core-owned: no catalog registration, no authorization grant, not nameable by a sub-task's suite. Its exemption is fenced by what it *is* — read-only, pinned to the one session its context wraps, no cross-session reach. The prompt line that says "the earlier originals are not lost, pull them back" is **rendered at projection time and never persisted**: both share one predicate, so the note can never promise a tool that isn't in the bag, and the store never freezes a tool name or a wording.

### Reading back: a summary is a table of contents, not a replacement

A compacted session gets a read-only tool pointed at its **own** log. It is **pull-based**, not push: browse the index page by page (newest first) or search by keyword to locate something, then read the specific entries by number. Every cap points at the same reason — a large read-back would immediately re-cross the very compaction line that armed the tool — so entries are truncated, a read takes at most twenty ids, and one call is bounded in total characters. Tool-call arguments are searchable but never displayed.

One implementation, two habitats: a history-sharing sub-turn reads its **delegator's** log (the advisor's whole purpose is auditing the parent's actual trajectory rather than trusting the asker's retelling), while a compacted main turn reads **its own** originals from behind the summary. The two descriptions differ, because telling a turn that is reading its own log that it is "reading the delegator's log" would simply be a lie to it.

### The context gauge

The core keeps three in-memory readings per session: the last **real** prompt token count (as the model itself reported it), the last sizing budget (the declared window of the entry that would serve it), and the number of messages in the last replay window. It is exposed to read-only panels through a soft seam — one mutex and one map read, touching neither the store nor the pool.

After a compaction the token reading is cleared while the budget survives: the first describes how much was eaten from a window that no longer exists, the second describes the size of the mouth, which is unchanged. The whole map is process-lifetime and capped: on overflow it is cleared wholesale and live sessions re-learn from the model's next report.

### Salvage

A turn that ran its round budget dry, or hit a guard, still owes the user **one** readable line. Salvage is where that line comes from, on its own short-timeout context (the turn's token may already be dead, and this line must land).

The ladder:

1. **One no-tool model call** — the model reads the failed turn's history, tool results included, and answers honestly from what it has. That is the whole value: a real "here is what I found, here is where I'm stuck" instead of a stock apology. The degenerate path first tries one tier of a stronger model.
2. **A no-LLM action brief** — when the model is unavailable, at least name the tools this turn ran.
3. **A stock floor line.**

Before the ladder runs, every **steering transient** is cleared (harness nudges, the bloat prompt, a redo critique, the advisor diagnosis). The diagnosis is the loudest of these: it says what to try next, which is the exact opposite of salvage's "stop calling tools, answer honestly from what you have" — and the stall path that triggers a diagnosis is the same path that ends in salvage, so the collision is the common case, not an edge.

A rubric-armed turn must not deliver an **unjudged** product even on the give-up path — the grounding guarantee is the whole point of the criteria, and salvage is the path most likely to fabricate. So the first tier's product is judged once (no retry; and only the *model-produced* tier — the lower tiers are self-evidently not products, with nothing to fabricate). On a fail the product is withheld and replaced by an honest failure notice — but the withheld product rides out on the result, so failure learning reflects on the genuinely flawed output rather than on the apology.

### Sub-loop delegation

The primitive is "run one blocking, bounded turn and capture its product" — **no core was extracted and no loop rewritten**. A sub-turn is the same turn core re-instantiated on a child sid; the parent's channels, scope and identity are inherited by being *passed down*, so the sub has zero extra privilege. That is the load-bearing wall of the design.

Delegation is not a concurrency primitive. Its payoff is **context cleanliness**: the parent's history holds only the task brief and the product, while DOM, image bytes and intermediate steps never enter the parent context.

- **Depth 1 is a hard rule**: tools inside a sub-turn get a refusing runner.
- **A private in-memory store**: a sub-turn's transcript is its own multi-round working memory, the parent takes only the final product, and a sub-turn is bounded and non-resumable — so it lives in process memory and a hard crash drops it with **zero orphan rows**. That store mirrors the persistent implementation's replay/compaction semantics exactly, so the shared round loop behaves identically inside a sub-turn.
- **The suite fence is at the dispatch layer**: the model picks a suite *name*, never a free tool list (one less injection surface), and the suite is intersected with the parent's authorized bag — a name the parent isn't authorized for simply isn't there. Turn-ending process tools are dropped: a sub has no channel to ask a user or hand off, and must always return a product.
- **A capture sink**: nothing leaves. Delivering a file inside a sub-turn **fails with an explicit error** rather than silently dropping, so the model folds the artifact into its product instead of believing it was sent.
- **Honest partials**: a sub-turn that ran out of rounds or tripped a guard still returns text, marked partial, never passed off as clean. A sub-turn's panic becomes an error product and never crashes the parent.

All three advisor roles (diagnosis, review, escalated arbitration) run on this same chassis with a different guide and a hard model tag; a history-sharing sub additionally gets the read-back tool **injected directly** — the capability and its access tool must travel together.

---

## Design rationale

### One kernel, two drivers

Under the word "turn" sit two things with different purposes: a chat reply, and a stretch of work that may run for minutes. Their difference is entirely in **exit semantics** — is the budget a legitimate ending, or a fuse?

So the divergence is compressed to that one dimension: two driver files, each owning its own exit semantics, with everything else single-copy. `spec.Mode` is consumed **exactly once** at the core's entry, and nothing downstream reads it. That is an enforceable criterion, not a stylistic preference: the moment a guard starts asking "what mode am I in", both sets of semantics seep into the kernel — and the kernel is the only place in this module able to hold its invariants.

The immediate payoff is the guard family keyed on **staleness** rather than round count. Because they don't distinguish modes, a 500-round looping turn and a 40-round regular turn obey identical stall discipline; the former is merely allowed to run deep while it keeps genuinely producing.

### The turn mode is stamped at session birth

The mode is stamped onto a session from the scope's default at creation and is immutable for its life; flipping the scope default affects only future sessions. So no conversation ever switches driver mid-flight — which would mean one stretch of history was written under one set of exit semantics and read under another, and those two read "ran out of budget" in opposite ways. The price is a "changed but not yet in effect" state, which `/session` makes visible by printing the birth stamp next to the scope default rather than leaving an operator to guess.

### Honesty over comfort

This runs through the whole module. Salvage's first tier is **a real model call**, not a template. A broken stream keeps the half-answer the user already saw and adds a "cut here" marker instead of a notice overwriting it (Finish replaces the whole message window). A failed side-effect tool leaves a footer that contradicts the model's claim that it went out. Orphan repair splices a result that says "may have completed, do not blindly retry" rather than "failed". A turn whose delivery failed outright would rather report an error than be booked as a satisfied answer.

The converse holds too: **silence needs a reason**. A cancellation with nothing visible makes no sound at all (but still releases the sink, so a coalescing sink's per-turn counter stays balanced); a stay-silent exit delivers empty content — and an empty Finish is a no-op on the agent-to-agent return sink, which is exactly how the dispatch chain terminates there.

### A judge never blocks the road

Both acceptance gates **fail open**. A gate's job is to stop things that are demonstrably wrong, not things it could not verify — a judging model that isn't deployed, an unavailable advisor, an unparseable verdict all pass, with a log line as well as a trace line (trace is off per scope by default, so a gate that quietly stopped gating must not be invisible).

The cost hierarchy is part of the design too: a blind judge is one ordinary call, the escalated arbiter is a whole sub-turn — one to two orders of magnitude more. So escalation happens only **after a blind fail**, at most once per turn; and the advisor review's retry cap is lower than the rubric's, because each of its redos costs that much more.

### Delegation by instantiation, not by extraction

The obvious way to build "sub-tasks" is to extract the loop into a reusable core. That was not done: a sub-turn is the same `core` with a different store, a different sink and a different spec. It therefore inherits every guard, every pipeline discipline and every compaction behaviour for free — there is no second set of semantics to keep in sync. And privilege inheritance works by **passing the parent's channels down verbatim**: a sub cannot reach anything the parent couldn't. That is a structural guarantee, not a checklist to audit.
