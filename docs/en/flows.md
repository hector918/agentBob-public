# flows: the policy layer

`flow/` is where thin orchestration scripts live. "How a particular kind of conversation runs" belongs here and can be swapped out whole; "what the system can do" belongs to the module layer below and does not move. **Changing behaviour means adding or swapping a flow, not touching the mechanisms.**

## Where it sits

Five flow modules plus one shared library. Provides / Needs come from the approved connection graph in the architecture guards:

| Module | Provides | Needs (hard) | Soft edges (`TryRequire`) |
|---|---|---|---|
| `flow-router` | `contract.FlowRegistry`<br>`contract.TurnHandler` | — | `contract.Accounts`, `contract.Gateway` |
| `flow-normal` | — | `FlowRegistry`, `Turn`, `PromptFactory`, `Gateway`, `PanelRegistry` | `MessageIndexer`, `RetrievalFeed`, `SkillCatalog`, `ToolCatalog`, `URLLibrary`, `Warrant` |
| `flow-agora` | — | `FlowRegistry`, `Turn`, `PromptFactory`, `Gateway` | `Agora`, `ChatHistory`, `MemberFailureSink`, `MessageIndexer`, `MessageStore`, `PanelRegistry`, `RetrievalFeed`, `SkillCatalog`, `SkillFailureSink`, `ToolCatalog`, `URLLibrary`, `Warrant` |
| `flow-intro` | — | `FlowRegistry`, `Gateway` | `AccountProvisioner`, `Accounts` |
| `inbound` | — | `Gateway`, `Screener`, `SlashRegistry` | `Accounts`, `SessionManager` |

`flow/compose` is **not a module**: it registers no trunk node and has neither Provides nor Needs. It is the shared orchestration library both main flows reuse, and it is the single exemption in the guards' import-boundary check — precisely because it is shared infrastructure rather than a module, importing it across flows is allowed.

Upstream: events arrive from [stoma](modules/stoma.md) (`contract.Gateway`), pass the screening in [gate](modules/gate.md) (`contract.Screener`), and come back batched by session. Downstream: a flow drives turn (`contract.Turn`), asks [warrant](modules/warrant.md) to authorize [tools](modules/tools.md) and [skills](modules/skills.md), and hands the reply to the source's sink. Per-module detail is in [modules/](modules/); the three spine mechanisms are in [core/trunk.md](core/trunk.md).

## What it is for

Between the process edge and the model and back, the decisions a turn requires split into two kinds.

**Mechanism** — how messages are batched, how history is persisted, how tool rounds run, how context is compacted, how tokens stream back into a chat window. These are the same for every kind of conversation, and they live in the module layer.

**Policy** — who should answer this message; what identity the turn runs as; who authorizes the tool bag; which prompt layers exist and what fills them; whether the output counts, and whether it feeds the learning loop. These vary by conversation type, and they live here.

The policy layer therefore has the shape of a thin shell: resolve a sink, compose a prompt, fill a `TurnSpec`, hand it to the turn core, consume the result. Both main flows share **the same skeleton**; what differs is what goes into it.

## Internals

### inbound — dispatch at the process edge

`inbound` is the one "flow" that produces no turns: it is the wiring from the source bus to the rest of the pipeline. A single consume loop pulls events off the gateway's fanned-in stream and routes each one.

The routing decision itself has exactly one cheap criterion: is this an internal event (an agent-to-agent dispatch, a timed wake, a bridge return) or an external chat event. Internal events are trusted and address a scope verbatim — no screening, no slash fork. External events run the full ingress sequence.

Four things happen on that external path.

**Language resolution.** Every chat event is stamped once with a reply language, via a cascade: an operator's pin → this message's own detection → the sender's remembered language → default. The cascade lives here rather than in `i18n` because it needs the per-sender memory held by accounts, and `i18n` is deliberately kept a stateless detect/translate library. Language is keyed **per sender**, not per chat — otherwise the last speaker in a group would set the language for everyone. To keep that lookup off the single consume loop, the read side has an in-process cache (the stored column is seed-once, so a cached value cannot go stale) and the write side is fired off the loop entirely.

**Admission screening.** Every event except those from a source marked trusted goes through the centralized screen, and the screen is **fail-closed**: only an explicit pass continues; a drop, a redeem, and any future unknown action all stop here. An unknown action additionally logs at WARN, so a new enum value is loud rather than silently admitted — pass is iota 0, so falling through would mean admitting. An unresolvable source drops for the same reason: a failed lookup must never behave like trust.

**The slash fork.** An event starting with `/` is a command, dispatched out of band rather than submitted as turn input. This is the densest cluster of judgement in the ingress sequence, because *who may run a command* turns on the account behind the handle rather than merely on whether the screen let them through: a platform operator always dispatches; a bound-but-paused account gets an honest bounce in a DM and falls through to the turn in a group (where the onboarding flow handles it); an account whose entitlement could not be read (a store blip) falls through to the turn so the router's fail-closed rule catches it; a genuinely new sender falls through to be funnelled into onboarding. Album siblings all carry the same shared caption, so a dedup guard ahead of the fork ensures such a command runs exactly once.

**Submit.** What remains is handed to the session module. `Stop` halts only the consume loop: events received but not yet started are dropped from the bus (the sender re-sends), and only the message a turn was actively running carries a write-ahead row, which boot recovery clears silently.

### router — selecting a flow

`router` is the turn-execution entry the session core sees. It provides both `contract.TurnHandler` (to the session) and `contract.FlowRegistry` (to the flows), and it starts before any flow so the registry exists when flows register themselves.

Selection has two rules: **highest Priority first, take the first flow whose `Accepts` returns true**. Registration order is irrelevant. That is the whole of the policy layer's replaceability — adding a new flow means registering it, giving it a priority, and writing its `Accepts`.

The real design is that routing splits two concerns into **orthogonal axes**:

- **Path** — decided by **scope**, independent of entitlement. Whichever flow accepts the work *is* its path.
- **Permission** — the caller's per-account entitlement to that path.

A bound caller without permission for the chosen path is **refused**, never rerouted to a lesser flow. That came at a cost: an earlier entitlement-first reroute meant a non-entitled human in an organisation's chat was silently handed the generic assistant — running under that worker's own scope and session, leaking context they had no business seeing. Refusal is safer than degradation.

Ahead of both axes sits a **front gate** (non-admins only): an unbound or paused sender is sent to the onboarding flow *before* the path runs. That flow never self-selects (its `Accepts` is always false) and is only ever returned explicitly by the router, so this is a routing decision rather than a competition between flows. Every default here points closed: with no onboarding flow present, suspension **refuses** rather than falls through — falling through would land on the permission gate, which never reads account status, and "paused" would quietly become a no-op.

A third rule is fail-closed in the same spirit: when the entitlement read **fails** (a store blip yields "unread", not "empty"), a non-admin is refused for that turn and self-heals on the next one once the store recovers. Enforcement must not lean on a read that did not happen.

The router also carries two light duties. It reports the selected flow's session mode back to the session arbiter, so the arbiter can cache and persist it without selecting a second time; and when that mode is automatic, it nils the nudge fast-lane — an automated turn must never be steered by a user message that arrived mid-flight. Read-only probes (dumping a prompt, querying the route) deliberately use path selection **without** the permission gate: inspecting a scope's prompt is not a runtime turn, and an admin must be able to inspect any scope, including an organisation inbox they are not a member of.

When no flow accepts, nothing is dropped silently; the router sends a localized notice, preserving the "every turn finishes exactly once" invariant.

### normal — the default conversation flow

`normal` is the Priority 0 catch-all: its `Accepts` is always true, so it takes everything no other flow claims.

Its policy is short:

- **Identity**: `admin` or `user`, decided by the ingress admin stamp. **Any** human event in the batch being an admin escalates the whole turn — a drain batch coalesces several senders into one turn, and a co-present admin lending authority is by design (the admin holds the close-session lever). A purely internal batch (a dispatch return, a timed wake, no human present) inherits the session's own admin ownership, so an admin's session resumed headlessly after a dispatch round trip stays admin.
- **Authority**: `warrant`. The tool bag is warrant's authorization over (identity, catalog). An absent warrant yields nil — **fail-closed**: a degraded or test configuration gets no tools rather than the entire catalog.
- **Session mode**: none. Both busy-queue notices and the nudge fast-lane are on.

The `RunTurn` skeleton: resolve a sink → open a trace (annotated with the flow name and this turn's session id, so a trace reader does not have to look it up) → compose the prompt → fill a `TurnSpec` → drive the turn core → consume the result.

That last step is entirely **outcome-classified** judgement, and it is the clearest example of policy living in a flow:

- Only outcomes that actually put something on the wire count as answered and are reported to the arbiter, which gates the drain's "handled N messages" acknowledgement.
- Only a clean final answer marks the turn's last URL as having satisfied the need. A clarification or a hand-off is a *process* reply — it asked a question or passed the work on, so it must not count as satisfied. Every other outcome still explicitly consumes the tracked entry, or the per-session map leaks.
- The cold-memory feed is enqueued only on a clean outcome (a final answer, or a deliberate process reply): a degraded give-up is a stock apology, and cancelled or errored turns delivered nothing, so neither belongs in a recall corpus.

Two further writes serve reply routing. After the turn, the platform message id that was sent is recorded in the index; and, whenever the sink supports it, each channel's anchor message is recorded **the instant it is sent**. The latter is not an optimisation: without it, a reply to the trace line — or to the live stream mid-turn — misses the index and forks a *new* session instead of folding into the busy one as a nudge. Both writes detach the context, because the turn's context is cancelled on close-session and on shutdown while the routing record must still land.

`composePrompt` is a pure function: the same work in yields the same (identity, prompt builder, skill set, tool set) out. Running a turn uses it, and so does dumping a prompt — so the dump is exactly what the real turn would use. Inside it, every canonical prompt layer is pinned **in top-down order**, including slots deliberately left empty, because `SetLayer` fixes a layer's render position on first set: that sequence *is* the render order.

### agora — the organisation-member flow

`agora` is the Priority 10 specialisation, claiming work whose scope routes to an organisation inbox. With the organisation module absent it claims nothing and the catch-all handles everything, which is why it can be optional.

It shares `normal`'s skeleton and swaps every piece of policy:

| | normal | agora |
|---|---|---|
| Accepts | always true (Priority 0) | scope resolves to an inbox (Priority 10) |
| Identity | ingress-stamped admin / user | organisation principal (member / inbox sentinel) |
| Tool authorization | warrant's own matrix | organisation **supplies** the member projection, warrant **judges** |
| Credentials | warrant matrix | the organisation's company grants gate it; warrant builds |
| identity prompt layer | default assistant persona | the member's identity card (**replaces** the persona) |
| platform prompt layer | empty | directory plus company playbook |
| Session mode | none | auto (autonomous worker: queue notices silent, nudge lane off) |
| Turn driver | decided by the session's birth stamp | **unconditionally** the long-task loop |
| Cold-memory partition | by caller (own handle in a DM, room in a group) | by member principal |
| Failure learning | none | degraded turns feed a per-role and a per-skill learner |

Several structural points deserve unpacking.

**Authorization is split into supply and judgement.** The organisation module knows who holds which role at which company and what that company grants, but it does not adjudicate; it hands over a member projection, and warrant — the **single judge** — intersects it with the catalog. So "who may use what" has one decision procedure, whether the request came from a personal conversation or an organisation member. File and exec channels stay warrant-bound throughout, with the reachable spaces being the member's own workspace plus one per company it belongs to.

**A bridge inbox only routes and never runs a model.** A company's talk-to-a-human line is pure forwarding: an internal (worker-originated) message goes out to the bound external chat; an external (human-authored) reply is re-injected into the downstream member inbox. This check runs first, before a sink is resolved or a prompt composed — which saves more than overhead: it makes it structurally impossible for the line to accidentally run a turn. Attachments crossing the bridge are **copied** into the downstream scope's space (both sides go through file channels confined to their own space root, never a raw cross-root copy), and on a successful copy the relayed text is re-rendered **without** those attachments' listing lines, because the downstream turn re-lists them itself and keeping them would show every file twice. A failed copy degrades the whole thing to a text-only relay, where those listing lines become the record of what was sent.

**Pause has two levels, and both bounce explicitly rather than reroute.** A company-level pause stops the entire link, bridge included. A member-level pause is judged separately, and the scope **is still claimed by `Accepts`** — otherwise a reversible pause would silently reroute a company group to the generic assistant, after which a human's reply would re-thread into an ordinary session. Claiming it and then bouncing is the only clean semantics.

**A virtual group has to ask who.** One group can serve several members' inboxes at once, with a human addressing one by a `^membername` prefix. With no name given and no default member, the flow runs no turn and instead lists the members to choose from — listing only those **probed individually** as currently available (a fan-out can span companies, so one company's pause must not be generalized to another). When none are available it sends the pause bounce instead of asking someone to pick from an empty roster. Each entry is prefixed with this bot's own mention handle, so in a multi-bot group the line is a **complete** address that can be copied verbatim.

**An addressing-hidden slash command must be intercepted.** The ingress slash fork only recognises a **leading** `/`, so `^member /command` sails past it; once the flow strips the addressing token, the bare `/command` would enter the model as literal text — and the model then **role-plays** the command instead of running it. So the flow intercepts exactly those events whose strip **reveals** a leading slash: a bare command never reaches a turn (ingress dispatched it), and a worker's own text beginning with a slash (no token) stays ordinary input.

**Degraded turns feed the learning loop.** Only a degraded outcome (a salvage, or a rubric rejection) snapshots the turn once and feeds two learners: the member-level one (every degraded organisation turn is a role failure, reported by scope) and the skill-level one (reported per skill the model actually engaged). Clarifications and hand-offs are process outcomes rather than failures, and keying on the outcome — instead of on "was there a reply" — is exactly what keeps a legitimate question from training the learners as if it were a failure. The snapshot's action window is anchored on this turn's last user row; when the turn has already scrolled past the replay cap and no anchor exists, it uses an **empty window** and honestly reports the actions as unknown — better to learn nothing this cycle than to fold an earlier turn's tool calls into this turn's failure snapshot.

### intro — mechanical onboarding

`intro` is the one flow that **never calls a model**: detect the sender's language and reply with one canned message about how to get an account. No agent, no history write.

It never self-selects (`Accepts` is always false) and is only returned explicitly by the router. Three details:

- **Dedup is keyed on (source, chat, sender, message sent)**, so a notice goes out once. The chat is in the key because sender-only dedup let a group greeting permanently mute that person's later DM. The message sent is in the key so a group stays at one neutral nudge across a pending→paused transition, while a DM re-notifies on it (its key flips with the message).
- **The group reply is neutral.** A reply in a group is public, so it becomes a "DM me" nudge rather than disclosing account status (pending, suspended) to the whole room; a DM keeps the status-specific message.
- **Dedup is recorded only after a successful send.** The cost is a tiny double-send window when two of a sender's messages race, which is far better than a transient send failure permanently swallowing the sole onboarding line. For the same reason an unresolvable sink is not recorded either — the next message retries.

Before replying, an unbound sender has a **bare account** minted (no entitlement, pending approval) so the admin has something to approve. That runs after the dedup check, so it fires once per greeting rather than on every message.

### compose — the shared orchestration library

`compose` is the mechanism both main flows reuse. The dividing principle: **a flow copies orchestration (a few lines of shell) but never logic.**

Concretely, "which prompt layers in what order, where identity comes from, who authorizes the bag" is policy and stays in each flow; what is **byte-identical** between them lives here:

- **Loading and seeding the static prompt layers.** Each of the four static layers has a built-in default and a runtime-editable file. The default is only a seed (written once when the file is absent, never clobbering an edited one); the file is the live source, read once per turn, so an operator's edit applies without a recompile or a restart. Seeding writes a temp file and renames it, so a crash mid-write cannot leave a **partial** layer file to be loaded verbatim later. A blanked file falls back to the built-in default: emptying a file should not silently strip a core layer out of the system prompt.
- **User-text joining and the attachment listing.** Attachments are listed by their **space-relative path** — the exact string a tool's file channel reads — so the model can pass it straight through with no per-tool name-to-path resolver. Every line is run through control-character stripping (so a crafted filename cannot inject extra list lines) but is deliberately **not truncated** (truncation would clip the extension, and the model's echo would no longer resolve). A named attachment that failed to download is still listed honestly as unreadable rather than dropped — otherwise a failed attachment with a caption is invisible and the model answers as if it were plain text. Already-transcribed audio gets a note saying the words are above: a voice note rendered as a bare path is indistinguishable from an untouched file, and the model spends a whole tool round fetching a transcript sitting a few lines up.
- **The skill index layer.** Rendered only when the skill-view tool is actually in this turn's bag — "use skill X" and "may invoke skill view" are independent grants, and telling the model to call a tool it cannot invoke surfaces as a config gap masquerading as a hallucination.
- **The tool-selection rubric layer.** A "which scenario calls for which tool" map assembled from the selection hints of exactly the tools **visible this turn**. Invisible tools contribute nothing, so the map is always made of tools the model can really call.
- **Bound channels.** One object with two faces: the opener for file and exec spaces, and the kind-unique credential opener. Building them once is what makes it impossible for those two `TurnSpec` fields to drift apart. The space argument is a hard gate: a non-empty space must be the default or a member of the allowed set, otherwise it is refused with the reachable spaces listed so the model can self-correct. An empty **principal** is refused too — an unidentified turn is safe today only because its bag happens to be empty, but local channels confine by space directory and ignore identity, so "no principal, no capability" must hold explicitly here rather than resting on "no tool happens to exist".
- **The attachment set.** The model is image-blind, and its only handle on a specific file is the name in that prompt listing. Resolution order is: a named attachment in this turn's batch → a same-named file left in the space inbox by an **earlier turn** → the batch's predicate matches. Step two precedes step three deliberately: the listing promises that a path stays resolvable across turns, so "no, the first one" must beat silently substituting this turn's sole image.
- **The prompt-dump skeleton.** Every flow's dump shares one rendering and one user-text preparation, with the flow supplying only what it composed. This is drift protection: one flow's dump once forgot a preparation step the real turn performed, so the dump and the run disagreed.

`compose` also owns a few prompt constants **both flows need**, most notably the long-task work-discipline layer. It states that mode's contract: deliver on convergence rather than reporting mid-way; the process is invisible to the user, so the final reply must be self-contained; gaps that could not be closed are written down honestly. It is shared rather than duplicated because the catch-all flow injects it for a session stamped as long-task while the organisation flow injects it on **every** turn.

## Design rationale

**Why compose exists, and why it is not a module.** The architectural rule is that a flow copies orchestration, not logic. Two flows each holding their own prompt assembly would drift — and drift here shows up as a missing defence on one path, or a dumped prompt that disagrees with the real run, neither of which is a compile error. Collapsing the logic to one copy leaves each flow as a few dozen lines that genuinely express its policy difference. It is not a module because it has no lifecycle, no resources, and neither Provides nor Needs; making it one would invent a trunk edge for a pure function library.

**Why path and permission must be orthogonal.** Merging them into one axis (choose the flow by entitlement) reads more simply, but it converts an authorization failure into a **reroute**, and the reroute lands in somebody else's scope and session. Split apart, the result of missing permission is "you are not authorized here" — with one exception: a caller with no entitlement at all is funnelled to onboarding, because "registered, awaiting approval" is *true* for them. Truthfulness is the criterion: someone who holds entitlements but not **this** path's is not sent to onboarding, since that would promise an approval nobody will action.

**Why every default in this layer points closed.** The tool bag is nil rather than the whole catalog when warrant is absent; the skill index is empty when authorization cannot be read; a failed entitlement read refuses the turn; a missing onboarding flow makes suspension refuse rather than fall through. The shared shape: **enforcement must not lean on a read that did not happen.** A degraded configuration gets less capability, never more.

**Why turn outcomes are classified rather than reduced to "was there a reply".** The classification feeds four downstream loops: the drain's handled acknowledgement, the URL library's satisfaction signal, the cold-memory corpus, and failure learning. Each of the four owes a clarifying question a different answer — it is a real delivery (acknowledge it), not a satisfied need (do not mark it), good recall material (store it), and **not** a failure (do not train on it). Reduced to "was there a reply", at least two of the four would get it wrong.
