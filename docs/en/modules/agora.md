# agora — multi-member organisation

Turns an org graph — companies, roles, members — into a routable, authorizable, deliverable runtime: an inbound message first asks **whose** inbox it landed in, and that identity then runs the turn.

---

## Place in the architecture

**Provides**

| Capability | What it is |
|---|---|
| `contract.Agora` | Routing (chat scope → inbox), per-turn context assembly, grant-projection supply |
| `contract.AgoraSend` | Target resolution and send authorization for a member's outgoing message |
| `contract.MemberFailureSink` | Intake for a degraded turn's role-guidance learning |

**Needs**

| Dependency | Why |
|---|---|
| `contract.DB` ([small-modules.md#pgpool](small-modules.md#pgpool)) | Its self-managed org entity tables |
| `contract.SlashRegistry` ([small-modules.md#slash](small-modules.md#slash)) | Registers the `/agora` admin command family |
| `contract.PanelRegistry` ([webui.md](webui.md)) | Registers the org graph panel and its two editors |
| `contract.ClaimTokens` ([small-modules.md#claimtoken](small-modules.md#claimtoken)) | Mint and redeem inbox wiring tokens |

**Soft edges** (`TryRequire`, degrade when absent): `contract.ToolCatalog` / `contract.SkillCatalog` ([tools.md](tools.md), [skills.md](skills.md) — enumerated only by the admin views), `contract.SessionManager` ([session.md](session.md) — the in-flight probe before a company delete), `contract.AccessGranter` ([gate.md](gate.md) — allowlist the redeemer after a wiring), `contract.LearnRegistry` ([small-modules.md#learn](small-modules.md#learn) — role-guidance distillation).

**Consumers**: the agora flow ([../flows.md](../flows.md)) assembles its turn from `TurnContext`; [arrangement.md](arrangement.md) uses `RoleProjection` / `MemberScopesForRole` / `CompanyBridgeScope` to dispatch work; the session resolver takes only the narrowest group-routing slice of the surface.

The module declares `Optional() = true`: a deployment that never bootstrapped an org simply does not install it, the agora flow's `Accepts` never fires, and everything runs as ordinary conversation. That is a genuine fail-open.

Worth naming separately are the edges that **do not** exist. agora imports no other leaf, and its entity types never enter `contract`. What crosses the module boundary is ids, thin values, and **pre-rendered prompt strings** — so the prompt layer never has to know the concept "agora" exists.

---

## What it does

**The org model.** Four entities:

- a **member** is a durable identity that outlives companies, employments and inboxes;
- a **company** owns its own role-grant matrix and operating manual, each an independent snapshot;
- an **employment** binds a member to one (company, role) pair, and a member may hold several at once;
- an **inbox** is the routing aggregation point, either `member` or `bridge`.

**Chat-scoped identity.** Identity is not stored on the session; it is **recomputed every turn**: the inbound path (chat scope) is the key, the outbound reach is computed on the spot, and history attribution is stamped per message. The session tables never gain an agora column, and there is no per-session agora side table.

**Inbox routing.** An external chat is bound to an inbox by a wiring row. One scope bound to one inbox routes 1:1; one scope bound to several member inboxes is a *virtual-group fan*, addressed by a `^name` token or the group's default member, then routed to a member sub-scope.

**Grant supply.** agora collapses a (company, role)'s grants into a `GrantSet` and hands it over — it **does not judge**. Judgment belongs to warrant, the single judge ([warrant.md](warrant.md)). A multi-company member gets the intersection; a per-inbox visibility denylist narrows it further.

**Delivery and authorization are one computation.** "Who may I write to" and "who is in my directory" are two outlets of the same calculation: a send is allowed exactly when the target is in the directory. There is no admin allow-all bypass.

**The bridge line to humans.** Each company has one bridge inbox — its bus to a real person. Internal messages go out through it to the operator; a human's message is re-routed through it to a downstream member inbox. A bridge never runs a model turn itself.

**Accumulated role guidance.** A degraded turn's trajectory snapshot is filed per (company, role), distilled by the experience optimiser, and injected into the prompt of the next turn held by that role.

**Admin surface.** The `/agora` command family plus a webui org graph: found a company, hire, change a role, wire a chat, pause, edit the grant matrix and the visibility filter.

---

## Internal structure

### The org mirror

`agora.OrgCache` (`20-orgcache.go`) is the **single in-memory mirror** of the whole pg org graph. Every hot path — identity resolution, send authorization, tool projection — reads RAM and never touches the database.

Reads take `RWMutex.RLock` plus a map lookup. Writes take one of two routes:

- **Write-through** (editing the grant yaml, editing visibility): store first, then cache, serialised per company. The order is load-bearing — a failed store write must never desync the mirror.
- **Full reload** (admin-triggered): read all four tables into fresh maps, then swap them wholesale under one `mu.Lock()`. A reader sees either the entire old set or the entire new set, never a half state.

There is also a set of cache-side upserts: the operation layer pushes a freshly written row straight into the mirror, so a new company / member / inbox is usable immediately instead of stale until the next full reload.

`Snapshot` returns a deep copy sorted by id. The deep copy is so a caller mutating the result cannot poison the cache; the sort is so the webui org graph's nodes do not jump between polls (positions are assigned by slice index, and map iteration order is random).

One invariant runs through all of it: **a known company's role config is never nil**. An empty or unparseable grant yaml lands as a non-nil empty entry rather than a missing key — otherwise a lookup miss could read as "unrestricted", which is a cross-tenant visibility leak. On a parse failure the cache stores the empty entry **and** returns the error; both happen.

### The role-grant matrix

`10-roles.go` defines the syntax and parsing of each company's grant yaml. The on-disk shape is the matrix an admin can scan at a glance: a top-level `roles:` list, then `grants:` as a capability → role → on/off table. Unmarshalling flattens that into one grant-string list per role.

A grant string is **strictly three segments**, `kind:verb:name` (e.g. `tool:use:read_file`), and **wildcards are not supported**. Parsing is *loud* throughout — every check returns an error rather than coercing quietly:

- an invalid role name (lowercase-only, bounded length);
- the same role declared twice under `roles:`;
- a grant cell referencing a role not declared under `roles:`;
- a cell value that is neither on nor off.

That last one matters most: a typo'd value is **not** treated as off, it hard-errors. This targets the "hand-edited typo silently coerced to off" bug class, whose symptom is a permission mysteriously missing with nothing anywhere saying so.

Grant iteration is sorted, so the same yaml always parses to the same order.

One deliberate exception in shape is the **coarse role bundle**: the orchestration tools can only be granted through `arrangement:role:creator` (define + inject) or `arrangement:role:collaborator` (pull + submit), which expand at parse time into the underlying tool grants. The four individual tool grant names are explicitly rejected in yaml, with the error pointing at the right bundle. The reason is that a half-grant is a meaningless state — a role that can claim work but not hand it back wedges the whole queue.

### The inbox router

`30-inbox-router.go` maintains the chat scope → inbox index. The whole index is an immutable snapshot published through an `atomic.Pointer`; a lookup is one atomic read plus one map lookup, with no lock on the hot path. Rebuilds happen under a mutex and end with a pointer swap.

The index is keyed by `contract.ScopeFor` over each wiring row's coordinates (source + chat type + chat id + thread id) — **the same scope grammar the session resolver uses**, so the two cannot drift apart. The router itself has zero per-platform knowledge.

Inbox **rows** come from the org mirror (the module's one RAM copy; it never reads back to pg here). Only the wiring rows are read from the store, because the mirror does not hold them.

Resolving a scope has three paths:

1. **The native path** — the scope *is* some inbox's own scope. Internal delivery and member-to-member messages both encode the target inbox's scope.
2. **A member sub-scope** — `<group scope>#<member>`, looked up among that group's member bindings. This path **deliberately has no fallback**: a miss is a miss, and it never drops back to the group's default member. Members are addressed *explicitly*; a stale sub-scope can only arise when the member was renamed or removed after a reply anchor was recorded, and silently redirecting to somebody else there would contradict the resolver's whole no-implicit-fallback model. Letting it miss re-roots to a fresh session and the human re-addresses — that is the honest outcome.
3. **The chat path** — scope → wiring row → inbox. One retry is allowed here: a forwarded-alias direct message carries a `#<alias>` suffix while the wiring row matches the bare sender scope, so the suffix is stripped and the lookup retried once (the stripped scope has no `#`, so it recurses at most one level).

All three paths end at the same **addressability gate**: the inbox is in the snapshot, its status is active, and its owning company is not disabled. Factoring that gate into one function is intentional — member *selection* (picking someone in a fanned group) pre-filters with exactly the same rule, so it can never pick a member the resolver will then refuse. Without that, every message would land in a "who do you want?" prompt whose roster still lists the unpickable member: a loop with no exit.

The "is the owning company disabled" check is deliberately fail-open: an unknown inbox or unknown company counts as active. The reasoning is in the rationale section below.

**Virtual-group fan-out.** When one scope is bound to several member inboxes, the 1:1 table is **deliberately left empty** for it (it cannot choose), and member addressing decides instead. A few details:

- The addressing marker is `^`, not `#`. A `#` collides with real hashtags and forum topics — a casual `#sale` must not be read as a member — while `^` is reserved for routing and clashes with no platform syntax.
- Matching is by **prefix plus a name boundary**, because CJK users glue the tag to the content with no space (`^alice帮我查下`), so whole-token equality would never fire. The boundary test is byte-wise (member names are ASCII), so `^alice帮我查` matches `alice` while `^alicexyz` does not silently match `alice`. When several names prefix the token, the **longest** wins.
- A `^` token that matches nobody returns "this *is* a fanned group but no one was picked", so the caller lists the roster and asks — not "this is not a fanned group". The two cases are handled completely differently.
- Several wiring rows binding the **same** inbox to one scope (a forum group's topics flatten to a single scope) are deduped by inbox, merging only the default flag — they route identically anyway.

### Per-turn context

`Impl.TurnContext` (`40-impl.go`) is the one assembly entry point the flow calls, returning the thin value `contract.AgoraTurn`:

- `InboxID` / `MemberID` — the latter is finer-grained than a role. Two members in the same role may hold different site logins, so member-level state keys on it.
- `Principal` — an active inbox with a known member gets `member:<name>`; everything else gets a **no-grants sentinel**. The sentinel's whole job is that a hub admin identity can **never** leak into an agora turn (a memberless bridge inbox lands here too).
- `Identity` — the rendered identity card: who you are, which companies and roles you hold, plus the member's own persona. It carries **no internal ids** — worthless to a user, and the model might echo them. This card *replaces* the default persona; the other prompt layers are untouched.
- `Directory` / `Playbook` — the rendered contact list and company operating manual, landing in a platform-layer slot distinct from the identity layer.
- `Spaces` — one file-space name per company the member works for. The same set feeds both the **hard gate** (the flow writes it into the allowed channel set) and the model-facing tool hint, so the two can never tell different stories.

The directory computation lives in `50-directory.go`, driven by a single deep-copied snapshot so the whole walk is frozen on a consistent view.

Reachability is the **union** across **all** of the member's active companies: someone employed at two companies reaches colleagues at either, may use either bridge, and sees both manuals. A colleague present in two of them produces two directory entries (each labelled by company), because those are two distinct addresses.

Note this runs opposite to grants, which take the **intersection** — a capability survives only if *every* company grants it. **Union for reach, intersection for power** is a deliberate asymmetry: one extra introduction carries no risk, one extra capability does.

### Grant projection

`MemberProjection` collapses an inbox's authorization into one `GrantSet`: the cross-company intersection, then narrowed by that inbox's visibility **denylist**. The denylist only ever subtracts — it can hide a granted tool or skill, never conjure an ungranted one, and credential grants are never hidden. An empty denylist (the common case) returns the set unchanged with no copy.

`RoleProjection` (`48-member-scopes.go`) is the single-(company, role) view the orchestration layer uses; the company matches by id or by name, and anything unknown yields an empty set.

Both are **supply** operations, answering "what does this identity hold", not "is this allowed". There is exactly one membership predicate, `Granted`, defined in `contract` and shared by both sides.

`DecorateToolSpecs` appends the member's reachable company spaces to the description of any tool taking a `space` parameter — the model cannot pick a space it has never been shown. This is **decoration, not judgment**: the specs are copied before editing so the shared catalog is not mutated, and the result still has to pass warrant.

### Send resolution

`45-send-resolver.go` implements `contract.AgoraSend` over the *same* org mirror and router that `TurnContext` uses — so "who can I write to" and "who is in my directory" are structurally one answer, not two implementations that have to be kept in step.

`ResolveTarget` first asks whether the argument already looks like a scope, and otherwise resolves it as a member name. **The name-versus-scope ambiguity is root-caused at creation time**: entity names are constrained to ASCII letters, digits, `-` and `_`, so a name can never contain a `:`, `/` or `#`. A member called `x:inbox:y` is refused when it is created, rather than mis-resolved when someone writes to it — ambiguity killed at the source instead of an ordering heuristic at the point of use.

A colleague name that matches in more than one of the caller's companies returns an **ambiguity error** listing the candidate scopes and demanding a specific one, rather than quietly taking the first by company name.

`CheckSend` has exactly one criterion: is the target in the caller's directory (same-company colleagues plus the caller's own bridges). Several deliberate points:

- A target scope that resolves to **no** agora inbox is **denied**. "Unknown means allow" holds only for a non-agora caller, and that case returned at the top of the function; letting an agora member through here would let it emit worker-authored text into any external chat.
- Self-send is denied — it would loop.
- A **bridge with no bound human chat** is denied. Such a bridge drops the message silently, and a member must not be told it was delivered.
- On success the delivery scope returned is **the resolved inbox's own scope**, not the external chat scope the member typed. That way an internal event lands on the inbox session instead of mixing into the operator's external-chat session.

`ResolveCredentialName` checks a tool's "send as" argument against the member's `credential:use:<name>` grants, using the identical intersection the tool projection uses. An empty argument passes through (the inbox default applies); anything under-wired — no resolver, no inbox, no member — is denied.

`ResolveBridge` serves escalation to a human; a caller employed by several companies with several bridges gets an ambiguity error, because which operator to escalate to is genuinely undetermined.

### Bridge routing

`BridgeRouting` reports how a scope behaves as a bridge: the bound external chat (internal message → forward out), the default downstream member inbox (human message → re-route in), and the **owning company's name**.

That last one exists because several companies' bridges can legitimately share one operator chat, and only a company label on the outbound forward keeps them distinguishable.

Bridge routing is a cold path (a bridge runs no model turn), so the wiring rows are read fresh — no warming, and no cost on the member-turn hot path.

### Two levels of pause

**Company-level pause** is a pure in-memory toggle, not persisted — a restart clears it, and that is the intended default. While paused, the company's whole link stops: member turns *and* bridge forwarding. Every arriving message is bounced with a notice, and **not deduped** — the sender resends after the resume, whereas dedup would leave them believing the message got through.

**Inbox-level pause** is a different thing: a member inbox that **exists but is paused** keeps its scope **owned by agora**; the flow claims it and bounces "this member is paused". Otherwise a temporary pause would quietly reroute a whole company group to the generic assistant — worse than not answering. This applies to member inboxes only: a paused bridge is not a "member", so the member wording would be wrong and it would short-circuit bridge routing.

Disabling differs from pausing again: disable is the persistent, reversible master switch — new inbound stops and cross-company sends into it are refused, while in-flight work continues and outbound still flows.

### The operation layer

`60-operations.go` is the one place data changes; the slash commands and any future CLI are thin fronts over it. Every operation has the same shape:

> validate → write the store → write through to the cache → (only when routing is affected) reload the router

The error policy is equally fixed: validation errors are plain errors; store errors propagate untouched so callers can discriminate them; cache and router refresh failures are logged at WARN and **not returned** — the side effect already committed, a stale mirror self-heals on the next reload, and reporting failure would only tempt a caller into retrying a write that succeeded.

A few operations deserve naming:

- **Founding** is an idempotent one-shot: company + bridge + founding member + employment + member inbox + an optional bridge wiring. Each step looks up by natural key first and only creates on a miss, so it is safe to re-run. The founder's role is the **first** role in the grant yaml (no hardcoded name) and must additionally survive the **strict** parser — otherwise a hand-edited yaml could seat a founder whose role exists to the lenient node walk but collapses to zero grants at runtime.
- **Two ways to take a company down**: disable is reversible; a hard delete is not, so it carries an extra **in-flight probe** — with the session manager absent it fails closed and refuses outright. Better to leave it undeleted than to yank an inbox out from under a running turn. The hard delete also cascades to members left with no employment at all, and reclaims the per-company write lock entry.
- **Wiring an inbox** goes through a claim token: mint on the admin side, redeem inside the target chat. Redeeming does two things at once — bind the chat to the inbox, and add the redeemer to the admission allowlist. Unwiring is addressed by the human-readable scope rather than an opaque id, and removes **every** row pointing at that scope, since a forum group's several topic rows route to the same place anyway.
- **Changing a role or ending an employment** touches no inbox: grants are recomputed next turn from the new role's matrix, and there is nothing to backfill.

### Slash and panel

`70-slash.go` is the thin front for `/agora` — argument parsing and replies, no business logic. It accepts both `--key=value` flags and positional arguments.

Two diagnostic subcommands carry most of the debugging weight:

- One prints the agora identity **the current chat** resolves to: inbox → member → employments → intersected grants → principal. "Why does this worker have the wrong tools" is answerable from that one screen: no member means the fail-closed empty set; several employments means the intersection narrowed it; neither, and the answer is in that company's matrix.
- The other prints the full live context **a named member** actually sees — identity, directory, manual, and the authorized tools and skills — assembled through exactly the same path a real turn uses. That one answers "why can't it see colleague X".

`90-panel.go` renders the org as a node/edge graph in the webui (three bands: companies, members, inboxes; employment and ownership edges), with a read-only glance plus action buttons behind each node. The whole panel is admin-only — org data is sensitive.

The panel carries two editors. The **grant matrix editor** prunes retired capabilities against the live tool and skill catalogs when saving, guarded by one necessary condition: it only prunes when a catalog actually **resolved**. Otherwise "not in the live catalog" merely means the catalog has not started, and pruning on that reading would wipe every real grant at once. Credential grants have no catalog to check against and are therefore always preserved. The **visibility editor** edits the per-inbox denylist and lists the full catalog names to pick from.

### Role-guidance learning

`80-learn.go` is a deliberately isolated peripheral — agora's core (operations, routing, slash, cache) does not know it exists.

After a degraded member turn, the flow hands over exactly two things through `contract.MemberFailureSink`: a scope and a trajectory snapshot. **The resolution is agora's own job** — scope → inbox → member → active employments → (company, role) pairs. A member may hold several roles at once, so the snapshot is filed under **each** of them, and the later per-role distillation filters which one it was really about.

Storage is a file ring under the home directory rather than the database — so no schema change, and no data migration to think about. Writes are atomic (temp file plus rename) and filenames carry a nanosecond timestamp, because filesystem mtime is only second-granular on some volumes and too coarse to order records. Each role's undistilled snapshots are capped, so a role that is never distilled (no distillation model configured, say) cannot fill the disk.

There is exactly one consumer: the distiller reads snapshots and deletes the consumed ones as it goes. Directory reaping lives in that same step, because it is the sole consumer — which lets "which roles have pending failures" stay a **pure read**, and the webui panel polls that every few seconds.

The distilled guidance is read back by `TurnContext` and injected into the same prompt layer right after the company manual, under a total length cap: neither a member wearing many hats nor a hand-edited oversized guidance file should be able to inflate the system prompt without limit.

---

## The store subpackage

`leaf/agora/store` is the module's self-managed pg persistence. It speaks only `contract.DB` — no `database/sql` import, so the fact "this is Postgres" stops at the connection pool and this package — and uses `contract.ErrNoRows` for not-found.

**The entity types live here, not in `contract`.** That is the boundary rule made concrete: agora entities are the module's own data, `contract.Agora` expresses agora outward through ids, thin values and pre-rendered strings, and the heavy structs never leave.

Five tables: members, companies, employments, inboxes, wirings. Several constraints are worth explaining:

- **Companies and inboxes form a reference cycle** (a company points at its bridge inbox; the inbox points at its owning company), so that foreign key is added by an idempotent post-create block — a forward declaration at CREATE TABLE time simply fails in pg.
- **Employments** carry a partial unique index on `(member_id, company_id, role_name) WHERE ended_at IS NULL`: at most one active row per triple, while a terminated row leaves the index once its end time is set, so the same person can re-join the same post — even within the same wall-clock second. A plain unique index cannot express that.
- **Wirings** carry a four-column unique index on `(source_id, chat_type, chat_id, thread_id)`, so duplicate wirings are caught in the database. There is no matcher JSON and no canonicalisation step — **the coordinates are the address**: inbound derives the scope from them, outbound uses them directly as the reply target, and both directions share one row.
- Each company carries its full role-grant config **inline in a column**, as an independent snapshot. Adding a new tool to the registry does **not** auto-grant it to existing companies — their snapshots have to be edited explicitly. That is intentional: a new capability defaults to off for everyone.

The schema is wipe-on-bump, so the base shape is one consolidated create rather than a chain of incremental ALTERs; a future destructive change adds a higher migration step.

---

## Rationale

**Identity is computed, not stored.** The session tables never gain an agora column and there is no per-session side table. The cost is a handful of extra in-memory lookups per turn; the benefit is that changing an employment, editing a permission, or disabling a company needs no backfill and no migration — the next turn is simply correct, and no stored identity can drift away from the facts. The whole in-memory mirror exists to make that recomputation cheap enough to do every turn.

**Supply is separate from judgment.** agora hands out a `GrantSet`; warrant is the single judge. The temptation is obvious — agora holds the data, why not decide? Because once two places can decide, "why was this call refused" has two places to look, and the two rule sets will eventually diverge. Kept separate, the single-tenant policy and the org's per-role permissions go through the *same* judge and cannot disagree.

**Authorization is the directory.** Send authorization is not a second rule set; it is a membership test against the very directory rendered for the model. This guarantees that the reachable set the model sees is byte-identical to the set actually allowed — it will never try a target that "would have worked but was not listed", nor hit one that "was listed but is refused". Either inconsistency teaches the model a wrong world model.

**Where fail-closed ends and fail-open begins is a decision, not an accident.** The authorization side is uniformly fail-closed: no active employment, a directory that will not build, an identity invalidated mid-turn — all project the empty set. The routing side's "is the company disabled" check is fail-open: while the cache lags, delivering to a possibly-disabled company beats dropping everything. The dividing line is the asymmetry of consequences — **a wrong refusal leaks capability, a wrong delivery just sends one extra message.**

**Ambiguity errors out; it does not pick one.** Duplicate colleague names, several bridges — all return an error listing the candidates. Silently taking the first demos better, but it turns "delivered to the wrong person" into a silent failure nobody discovers until much later.

**No admin backdoor.** The send judge has no allow-all branch. An escape hatch inside the judge would give *every* future caller a way to skip authorization entirely, and it would make the audit record lie.

**Kill ambiguity at the source, not at the point of use.** Member names cannot contain a scope delimiter, enforced by one regex at creation. The alternative is a "try scope first, then name" ordering heuristic in the send resolver — a rule that always reads like guesswork, and has to be re-derived every time a new scope shape appears.
