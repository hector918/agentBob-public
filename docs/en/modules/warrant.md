# warrant — capability authorization

The security core of the system: it judges whether a principal may use a capability, and narrows the tool catalog, the skill catalog, the credential vault, and the file/exec surfaces down to **the subset this particular turn is actually allowed**.

---

## Place in the architecture

**Provides**

| Capability | What it is |
|---|---|
| `contract.Warrant` | The single capability judge: project, filter, vend channels, revoke |

**Needs**

| Dependency | Why |
|---|---|
| `contract.ToolCatalog` ([tools](tools.md)) | Reconcile grant rows; look tools up when projecting |
| `contract.ChannelPool` ([tools](tools.md)) | Holds the stateful exec channels it vends |
| `contract.PanelRegistry` ([webui](webui.md)) | Registers the permissions panel |
| `contract.SlashRegistry` ([slash](small-modules.md#slash)) | Registers `/tools`, `/skills`, `/permission` |

**Soft edges** (`TryRequire`, degrade when absent): `contract.SkillCatalog` ([skills](skills.md) — no skill subsystem, no skill projection), `contract.Broker` ([credentials](small-modules.md#credentials) — no broker, no remote spaces and no credential resolution), `contract.AdminLine` (summon an admin when the policy is broken), `trunk.Housekeeper` (exec-home retention sweep).

**Consumers**: the flow layer ([flows](../flows.md)). A flow resolves the principal itself and hands the projection to warrant to filter against.

The module declares `Optional() = true`, which does **not** mean "absent equals permissive". With no warrant, a flow projects **zero** tools and skills, not the whole catalog. That fail-closed reading runs through the entire module.

There is also a hard **start-order** constraint: the skill catalog must start before warrant, or warrant's boot-time reconcile runs against an empty catalog and treats every skill grant as non-existent. That constraint is deliberately **not** expressed as a hard dependency — a hard Need would let an absent (equally Optional) skills module skip warrant entirely, and with warrant gone tool enforcement disappears with it. So it is a soft edge plus registration order, with an architecture guard test machine-checking the order. A miss here fails closed (skills denied), never open.

---

## What one turn's authorization looks like

1. The flow resolves this turn's **principal** — an opaque string; how it is derived is the flow's business.
2. The flow asks a supplier for a `GrantSet`: the outer single-tenant path calls `Warrant.Grants`, the organisation path uses the organisation module's own projection.
3. The flow hands that projection plus the full spec list to `Authorize` and gets back this turn's tool bag; skills work the same way. The model sees only what is in the bag.
4. When the model calls a tool that needs disk or a shell, the tool asks `File` / `Exec` for a channel into some space. That is the **level-two gate**: level one was "is this tool in the bag", level two is "may you reach this particular resource".
5. A tool needing a credential goes through `ResolveCredential` and receives a **built client**, not a secret.
6. When permissions change, `Cut` reclaims that principal's still-live channels.

**Principal and member are two separate stamps.** The context can carry both a principal (per role — what the gate judges on) and a finer member identity (per person). They are separate because different people in the same role may hold different logins: the gate still judges by role, but anything that must be isolated per person (a browser login, say) keys on the member. An unset member stamp falls back to the principal. Both ride on the context rather than being threaded through every signature, so deep callers such as the credential broker do not force a signature change down the whole chain.

---

## What it does

**Judgment.** The currency of one decision is `contract.GrantSet` — a resolved set of capability strings (`tool:use:X` / `skill:use:X` / `credential:use:X`). Who *produces* that projection is the **supplier's** business (warrant collapses its own matrix; the organisation module collapses per-company permissions). Filtering a catalog against it is the **judge's** business, and there is exactly one judge. Set membership is defined once (`GrantSet.Granted`, exact match; an empty set grants nothing).

There are only three families of capability string, and the verb is always `use`:

| Form | Governs | Judged by |
|---|---|---|
| `tool:use:<name>` | Which tools the model sees this turn | Bulk projection |
| `skill:use:<name>` | Which skills the model sees this turn | Bulk projection |
| `credential:use:<name>` | Whether a client built from a given credential may be obtained | Single-capability check |

All three share one matrix, one parse, one degradation path — so "grant someone this capability" is the same act in all three cases.

**Vending capability surfaces.** Authorization is not only "may this tool be called" — it is also "may this disk be touched, may a command run on that host". warrant unifies the three surfaces under the notion of a **space**: `File` vends a file channel, `Exec` vends an exec channel, `Cut` reclaims a principal's still-live channels when permissions change.

**Audit.** Every explicit single-capability decision writes one JSONL line (allow *and* deny); every bulk projection writes one summary line. The stream is dedicated, separate from the noisy runtime log, so it can be filtered directly.

**Operations.** The matrix is editable in a webui panel with immediate effect, or hand-editable on disk and hot-reloaded from a slash command. No restart either way.

---

## Internals

### The matrix and the storage seam

The matrix is a two-dimensional **principal × capability** table: capability string → principal → on/off. The engine side (`10-policy.go`) holds exactly three things — the matrix type, the fail-closed lookup, and the `PolicyStore` seam.

Fail-closed shows up at every layer. `policy.allows` returns false for an unknown capability, an unknown principal, even a nil matrix. Cell values go through `truthy`, so a typo'd value is "off", never "on". `LoadOrFailClosed` turns a **present-but-corrupt** source into an empty deny-all matrix plus a `broken` flag — never an allow-all gap.

The storage implementation only reads and writes (`12-store-file.go`, the yaml backend); the semantics stay in the engine. That seam exists so a future database backend (per-company / role / flow permissions) can reuse the identical engine, and both backends degrade byte-for-byte the same way.

The file backend writes through tmp+rename and sorts its keys, so every change is a reviewable diff. It also offers a verbatim-write path: when the caller is holding text an admin edited by hand, the bytes go down unchanged, preserving comments and ordering. The file stays something a human reads and edits, not something a program has reshuffled.

### Reconciling against the catalogs

`20-reconcile.go` makes the matrix a **pure mirror** of the registered capabilities: a newly appeared capability gets a row default-**OFF** for every principal, and a capability that is no longer registered is deleted outright. It never guesses — every row it adds is off, and the admin turns on what they want afterwards.

Two guards:

- **An empty set is not a prune signal.** An empty "registered capabilities" set means "I don't know" (a catalog module that failed to start), not "everything was removed". A real deployment always has at least one tool, so an empty set skips the delete half — otherwise one transient failure wipes every grant, including admin-flipped ones.
- **A degraded catalog is add-only.** If the skill catalog fell back to its built-in set because an external scan failed, the names it serves are **known-incomplete**. Reconcile switches to add-only there, or the temporarily-unseen external skills lose their grant rows and the loss gets persisted. This check must be **live** at both call sites — boot and hot reload — because a reload can land inside the degraded window.

### The judge: project and filter

`30-authorize.go` implements `contract.Warrant`. Four actions:

- `Grants` collapses warrant's own matrix into a principal's `GrantSet`. This is the outer supply; the organisation module has its own.
- `Check` is the single-capability gate — one decision per call. It is the one judgment boundary that gets audited line by line.
- `Authorize` / `AuthorizeSkills` filter a spec list against the `GrantSet` the flow handed over, producing this turn's tool bag / skill bag. Ungranted tools are dropped outright; the model never sees them.
- `ResolveCredential` scans the vault by kind, keeps the authorized names, and requires **exactly one**: zero reports "none authorized for you", more than one reports a configuration error (one member should hold one credential of a kind). The secret never leaves the broker, and the error reports a count only — vault credential names are never leaked to the tool or the model.

The bulk projections **deliberately do not log per item**: they run over the whole catalog every turn, per-item lines would flood the file, and no decision was actually made — the model simply never saw those tools. So a bulk path writes one summary line (principal + the names granted this turn) and `Check` carries the itemised record.

The matrix lives behind an **atomic pointer**. A hot reload can swap a fresh matrix in while concurrent turns read: a reader sees the whole old table or the whole new one, never a half-built one. The pointer may be nil before the first store, and the lookup is nil-safe (deny, not panic). Writers additionally take a mutex that serialises the read-reconcile-persist-swap sequence, because the reload, the skill rescan and the panel save would otherwise interleave and lose each other's edits — the atomic pointer makes each swap tear-free, not the multi-step update atomic.

A policy that was corrupt at boot sets a flag and raises **one** alert on the admin channel at the **first projection** rather than at startup — the admin channel may start after warrant does.

### The decision log

`15-decisionlog.go` is a thin wrapper: it owns the record shape, while the append / cap / trim / cached-handle machinery lives in the shared ring-file primitive. Records run around two hundred bytes, so the default soft cap of ten megabytes holds a long stretch of audit. When a write would exceed the cap the oldest half is trimmed **in place**, so the file stays parseable JSONL and a live tail keeps working.

It is best-effort throughout: an unwritable path or a full disk produces one warning and self-disables for the process lifetime. It never returns an error to, and never blocks, the decision path.

### Spaces and the three capability surfaces

A **space** is an addressable workspace. `70-spaces.go` maps a space name to a backend: anything not listed is a **local directory**; a space declared `ssh` is a remote host reached through a named credential.

Configuration validation is fail-closed too: an unknown backend name, or a credential set on a local backend, is a parse failure — it must never fall through to the local branch. The reason is blunt: a "remote" space that quietly runs in an empty local directory *and* skips the credential gate is the worst possible outcome (the operator believes it ran on the far host; it ran nowhere). A missing config file is the normal all-local default; a present-but-unparseable one disables space routing entirely.

Resolving a space name to a path (`SpaceLocalDir`) is the security-critical part here:

1. **Fold first, validate second.** Every character outside the allowed class folds to `_`. That covers scope separators and anything a chat id, member handle or company name might carry, so a structured scope name always becomes a valid single-segment directory name rather than being rejected. The fold **is** the traversal guard — a `/` can never survive into a path join.
2. **The post-fold regex check is defence in depth.** Both rules derive from one shared character-class constant, so they cannot drift out of being exact inverses — and drift is precisely what would open a hole (a character the validator admits but the fold no longer removes, or the reverse).
3. **Pure-dot names are rejected separately.** The regex admits `.`, so without an extra check a folded name could still climb out of the spaces root.

The function is exported because attachment placement runs at session submit — before the warrant runtime is reachable — and the placement target must match the file channel that later serves that space byte for byte.

**The local file channel** (`50-localfs.go`) resolves every operation under its root and refuses any escape. First a lexical check (`..` and absolute paths are neutralised by force-rooting), then a **symlink confirmation** — the lexical check cannot stop a symlink *inside* the space pointing out of it, and the exec channel shares that directory, so the model can create one. The channel resolves symlinks on the deepest **existing** ancestor and requires the real path to stay under the real root.

One more layer, information hygiene: filesystem errors embed home-rooted absolute paths, and those errors flow straight back to the model. Every returned error is rewritten to keep only the space-relative remainder — the model works in space-relative terms and must not learn the host's on-disk layout.

**The local exec channel** (`60-localexec.go`) runs a shell in the space directory. Each run is a fresh process rooted at that directory, so **files** in the space persist across commands (and are shared with the file channel, making write-then-run work) while **working directory and environment do not**. The design wants a genuinely continuous session, which needs a pty — a shell writing to a pipe block-buffers, so a sentinel-on-a-pipe stalls, and an interactive shell on a pty brings prompt and echo handling with it. The one-shot model is the current baseline; a continuous session is an upgrade behind this same channel interface. Either way the channel is pooled — what gets reused is the holder.

A single command has a time bound and its output has a byte bound, both fixed inside the channel implementation rather than chosen by the caller. Four points:

- **The environment is a whitelist, never the inherited one.** Hard invariant: the database connection string, API keys and bot tokens are all read from the process environment and must not be inherited by a model-controlled shell. Only `PATH` plus locale/timezone variables pass through; `HOME` and `TMPDIR` are pinned to a private directory **outside** the space so a program's dotdirs and temp files do not litter the user-visible file area.
- **Process-group kill.** stdout/stderr are pipes, so the wait blocks until pipe EOF; a backgrounded child inheriting the write end can hang the call forever and wedge every later exec on that channel. The whole tree goes into one process group, cancellation kills the group, and the pipe-EOF wait is bounded.
- **Output is capped as it is produced.** A model-controlled command can emit gigabytes; buffering it all and truncating afterwards is an OOM. A capped writer keeps only the head while counting the total for the truncation notice, and trims a trailing incomplete multi-byte character so what is kept is always valid text.
- **The parent turn's deadline is checked first.** The command's own timeout context derives from the turn's, so a cancelled turn trips both at once; checking the turn's first prevents an external cancellation being misreported as "command timed out", which would point debugging in entirely the wrong direction. The remote channel uses the same ordering.

**The SSH exec channel** (`80-sshexec.go`) runs the command remotely. The expensive part is the TCP + SSH handshake, so the **connection** is pooled and reused while each run opens a fresh session. It is the channel pool's first genuinely stateful consumer. Output capping and timeout semantics match the local channel; a connection found dead closes itself so the pool rebuilds it.

The gating layers on the vending path: `Exec` looks at the backend first, the remote branch passes the per-resource **level-two gate** (`credential:use:<name>`) before asking the broker for a connection, and a local space needs no credential at all. The resulting channel is cached by the pool under (principal, space, kind); `Cut` reclaims by principal through the pool.

### Attachment placement

`45-placement.go` relocates an inbound message's downloaded attachments from their staged path into the **space** of the resolved scope, so a user's files live where the file / OCR / vision tools read, and outlive the turn. It runs **once per inbound message**, before any organisation-side member fan-out, so a staged file shared across members moves exactly once.

Three details worth keeping:

- **The inbox filename comes from the attachment's display name**, not the uniquely-prefixed staged name. The prompt shows that name and the model passes it straight back to a tool, so display equals on-disk: no per-tool name-to-path resolver, and no staging prefix leaked. When there is no usable display name (a nameless sticker, say) it falls back to the staged name, which is collision-proof by construction.
- **Name collisions are claimed by exclusive create**, not by stat-then-write. Placement runs before the per-scope turn lock, so two concurrent same-name placements are a real race; the claim gives them different names instead of letting both win a stat and overwrite each other.
- **A failure clears the path.** An attachment that did not make it into the space has a staged path that is now space-dead; keeping it would put an unreadable handle into the prompt and the history. So a whole-batch abort or a single failed move clears the path, degrading honestly to "not downloaded" rather than leaving a fake handle behind.

The move prefers a rename and falls back to copy-then-delete across filesystems — the staging area and the space area may well sit on different volumes.

### Exec-home sweeping

The private `HOME`/`TMPDIR` tree the exec channels use is invisible to both the user and the model — and that privacy cuts both ways: nobody sees it, so nobody cleans it, and it grows unbounded for the life of the deployment. `45-exechome-sweep.go` is its retention sweep, registered on the trunk housekeeping scheduler (persistent disk state belongs there, not on a module's own ticker).

Rules, most to least aggressive:

- **Orphan**: an exec-home whose space no longer exists is removed whole.
- **Cache**: inside a live space, the *contents* of the well-known regenerable cache directories are age-pruned by modification time. A cold cache is re-downloaded on next use — slower, never wrong.
- **Temp litter**: the exec-home root *is* `TMPDIR`, so non-dot top-level entries are removed whole once their own timestamp ages out.
- **Everything else is untouched.** Other dotdirs are **install state**: a user-level install tree keeps its install-time timestamp forever, so age-pruning it would part-delete a still-working toolchain — wrong, not slow.

The tree is model-writable, so the walk pins directory handles instead of walking path strings. A path-string walk leaves a window in which a directory can be swapped for a symlink between the check and the removal, unlinking files outside the tree.

### Hot reload and revocation

`40-slash.go` provides three commands.

`/tools` and `/skills` run the **same** projection a real turn would, under the sender's own principal, so what they list is exactly what that sender can use — not a separately written "what you should have" list. Both print names only: a description is the model's tool contract (a functional prompt), not user-facing copy, and showing it would push un-localisable text at a human reader. These commands belong to warrant because per-identity projection *is* its job — cramming them into a session-side command would make a lower-level module reach up for the catalogs.

`/skills reload` does one necessary extra thing: after rescanning the catalog it **immediately reconciles**, so a newly scanned skill gets its default-off grant row. Otherwise it is both fail-closed denied and absent from the permission file, leaving the admin no way to turn it on.

`/permission reload` is an admin action: re-read the matrix file, reconcile against the current catalogs, atomically swap in the live matrix. The subcommand is required rather than letting the bare command mean reload, so future subcommands slot in without changing the existing surface.

Reload and boot **deliberately fail differently**. At boot there is no earlier matrix to keep, so it must fail closed. At reload there *is* a good matrix in hand, so a parse error swaps nothing — the admin keeps the working matrix and is told what is broken.

After a swap comes the **diff-cut**: any principal that lost a grant in this reload has its live pooled channels reclaimed. Otherwise a permission *removal* lingers on a pooled channel until it idles out, possibly outliving a whole in-flight turn. Purely additive reloads (reconcile's default-off rows, a newly granted capability) cut nobody. Revocation is per principal because the pool keys on principals; a still-authorized channel is simply re-acquired and re-checked.

### The operations surface

`90-panel.go` is the module's self-description for the webui, **admin-only** — the matrix is sensitive and is redacted from the state API until authenticated. The panel shows health, principal count, total grants, and a read-only capability → grantees table, so an admin can audit who may use what at a glance.

The editable side is a yaml editor over the same file an admin would hand-edit. Validation before it touches disk is **strict**, not lenient: one click puts a new policy live, and under lenient parsing a typo'd top-level key or a cleared editor yields an **empty matrix with no error** — the save would then flatten every grant instance-wide, cut every principal's channels, and report success. So the typo'd key is rejected by strict field checking and the empty document by a dedicated emptiness guard. Revoking everything on purpose requires a hand edit plus a reload, which keeps that act deliberate.

The save path persists the raw text (comments and ordering preserved) and then reloads through the **identical** path the slash command uses. The write lock is held across both the write and the reload, so a concurrent reload cannot slip between them and clobber the save.

---

## Design rationale

**Authorization has one source of truth: the permission configuration plus the gate.** There is no "admin override" bypass and no second `IsAdmin` check anywhere that can route around the matrix. Any temporary hole becomes a permanent hole, and it makes the audit log lie.

**Fail-closed is a posture, not error handling.** The default answer everywhere is no, not "let it through for now":

| What went wrong | Result |
|---|---|
| Permission file present but unparseable | Deny everything; summon an admin at the first projection |
| Permission file missing | Empty matrix (also deny-all); reconcile seeds the rows |
| Space config present but unparseable | Space routing disabled entirely |
| Capability catalog unreadable (empty set) | Do not prune; keep existing grant rows |
| Skill catalog degraded | Reconcile goes add-only |
| The warrant module itself absent | Zero tools, zero skills |
| Reload finds a broken file | Swap nothing; keep the good matrix in hand |

That is also why a module marked Optional carries the strictest possible meaning when it is missing.

**The judge does not resolve identity.** warrant knows nothing about *who* or *which flow* — it takes an already-resolved `GrantSet`. That lets the outer single-tenant policy and the organisation's per-company/role permissions run through the **same** judge, so the two filtering semantics cannot diverge; and it means the identity model can change without touching the authorization engine.

**Channels are a product of authorization, not a tool's private property.** Tools do not open file handles or spawn shells themselves — they ask warrant for a channel. That splits "may you touch it" from "how you touch it": the gate is in warrant, the mechanics are in the channel implementations, revocation is in the pool. When permissions change, reclaiming is one call.

---

See also: [Architecture overview](../architecture.md) · [flows](../flows.md) · [tools](tools.md) · [skills](skills.md) · [credentials](small-modules.md#credentials) · [agora](agora.md) · [webui](webui.md)
