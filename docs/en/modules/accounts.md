# accounts — cross-entrypoint identity

How the same person arriving from different platforms collapses into one identity; what that identity has been granted, what it has spent, and how a machine knocks on the door on its behalf.

---

## Place in the architecture

**Provides**

| Capability | What it is |
|---|---|
| `contract.Accounts` | Read-only identity resolution: handle → account → flow entitlement, plus the reply-language seed |
| `contract.AccountProvisioner` | The onboarding write seam: mint a bare identity with no access |
| `contract.APIKeys` | Bearer-token verification for machine callers |
| `contract.ConsumptionReporter` | The narrow per-user billing seam |

**Needs**

| Dependency | Why |
|---|---|
| `contract.DB` ([pgpool](small-modules.md#pgpool)) | Persistence (its own tables on the shared pool) |
| `contract.SlashRegistry` ([slash](small-modules.md#slash)) | Registers `/accounts` and `/bindaccount` |
| `contract.PanelRegistry` ([webui](webui.md)) | Registers the account roster panel |
| `contract.ClaimTokens` ([claimtoken](small-modules.md#claimtoken)) | Mints binding codes |

**Soft edges**: `contract.AccessGranter` ([gate](gate.md) — the auto-allowlist side effect of binding; absent means identity is recorded but access is not granted), `contract.Agora` ([agora](agora.md) — whether approval also grants the organisation flow), `trunk.Housekeeper` (periodic drain of the usage buffer).

**Consumers**: the flow router reads `Accounts` to decide which path a message takes ([flows](../flows.md)); the model pool books usage through `ConsumptionReporter` ([model](model.md)); the outward-facing API gateway authenticates callers through `APIKeys` ([modelgate](modelgate.md)); the onboarding flow mints identities through `AccountProvisioner`.

The turn core never sees this module — usage accounting is a host concern, not loop mechanism.

---

## What it does

**Identity collapsing.** One person may hold several accounts on one chat platform, several mailboxes, several ids across apps of the same vendor. accounts collapses them into one logical **account**, with each entrypoint a **handle** under it.

**Access is granted, never default.** Identity (having an account) and access (having a flow entitlement) are two layers. A freshly minted bare account can do nothing until an admin approves it. The entitlement field is a comma list with a single parse rule and **no implicit floor** — an empty entitlement grants nothing at all, not even basic conversation.

**Binding.** A user can self-serve another of their channels onto the same account; an admin can mint a code and hand it to someone so they bind themselves to a named account (with the authority to repoint an existing binding). Binding codes are one application of the project-wide claim-token facility, not a token mechanism of their own.

**Accounting.** Every model call's tokens are booked against a **handle**. Keying on handles means an unbound person's spend is still recorded; the account-level number is a read-time rollup.

**API keys.** Mint a machine-usable bearer token for an account: the machine-facing entrypoint authenticates the caller by it, bills the owning account, and enforces the policy the key carries.

**Reply-language seed.** Remember which language a sender speaks so a message carrying no language signal (a bare slash command, say) still answers correctly. It is **per sender**, not per chat — in a group, everyone is independent.

---

## Internals

### Why the four contract faces are separate

One `Manager` implements all four, but they are **deliberately** four distinct seams:

- `Accounts` is **read-only** because it sits on the routing hot path. Admitting a write here would put a potential write behind every message.
- `AccountProvisioner` is the onboarding **write** face, resolved lazily by the onboarding path. Layering identity under access works precisely because minting lives only on this face, and what it mints carries no permissions.
- `APIKeys` has exactly one action. When it is absent every consumer request **fails with 401** — never open.
- `ConsumptionReporter` is narrow enough that the model pool can depend on it without knowing anything about the identity model.

### The stand-in for a failed start

The module is Optional: a deployment that never registers it runs fine (the router self-selects, usage is dropped). But "registered and failed to start" and "never registered" must be **two different things**.

If a failed migration made the module simply vanish, the router would see no identity authority and fall **open** — a stranger on an onboarding source would get the full flow. So on a start failure the module still **provides** `contract.Accounts`, but provides a fail-closed stand-in: every sender reads as unbound, so the router funnels non-admins to onboarding or refusal while admins (identities the platform already knows) still pass the basic floor. The language operations degrade to no-ops.

The other three contract faces are **not** provided: usage is dropped and onboarding writes are unavailable — their consumers resolve lazily and degrade past that. The module then returns success rather than an error so the spine continues in a degraded state, with health reported as failed for the ops view; returning an error would instead trip the "provided then errored" abort guard.

This is the canonical case where absent and present-but-broken must give different answers: the first is a deployment choice, the second is a fault.

### The identity key: platform family + stable uid

A handle is keyed not by "source name + user id" but by **platform family + cross-app-stable uid**. Source names are per-bot or per-mailbox (several run on one platform); folding them to the family makes the same person **one handle** across all of your bots, mailboxes and apps. The uid likewise prefers the identifier the platform guarantees stable across apps rather than an app-local one.

That folding rule lives in the contract layer (`MessageEvent.AccountHandle`) because it *is* the definition of identity collapsing — no participant may hold a private version of it.

A handle row also stores the **real** source name and access uid, but for display and audit only: access is never re-granted from those columns. They answer "which concrete channel was this bound from", not "where may this person come in from now".

### Binding

`40-binding.go` holds three binding paths, all serialised under one mutex so two binds never race on one handle.

**Self-service creation** mints a **flow-less** account, binds the current handle, and does **not** auto-allowlist. The reason: once a source is open for onboarding, an un-allowlisted stranger could otherwise mint themselves into full access with nobody approving anything. The self path grants identity only.

**Code redemption** does auto-allowlist — a code *is* the admin's approval. The grant follows the redemption event's scope: redeeming in a group grants in that chat (otherwise a per-group allowlist would still block them), redeeming in a DM grants source-wide. When no grant lands (no gate wired, or a write blip) the return value honestly says "bound, access pending" rather than reporting success.

A handle already bound to a **different** account is refused by default; only a call carrying repoint authority (an admin-minted code, or a real admin) may re-point it. A repoint **deliberately does not revoke** the prior allowlist entry: that entry is keyed on the same physical channel and the same person, who now belongs to the new account and should still be able to reach the bot. Access is additive across repoints (no cross-account leak); real access removal belongs to an unbind path, not to a repoint.

The **binding code** itself is a batch-command token: minting freezes a `/accounts bindto <id>` command (plus a repoint marker for an admin code) and freezes "runs with admin authority". Since `bindto` is admin-gated, that frozen authority is exactly what lets the recipient execute it — the code is the grant, single-use and time-limited.

### Onboarding and approval

`45-provision.go` is the two ends of onboarding.

`EnsureBareAccount` is the way **in**: for a sender with no identity yet, mint an account with **no flow entitlement**, bind the handle, and record **which scope they first appeared in**. It writes the empty entitlement explicitly rather than letting the column default apply — otherwise a bare account is born with default access.

`Approve` is the way **out**: one admin action completes onboarding. It allowlists each handle on its real source, decides which flows to grant based on the onboarding scope, and writes the entitlement.

Two idempotency choices matter. The allowlist is **re-affirmed on every run** (adding an existing entry is a no-op), so re-running approve repairs an allowlist write that blipped. The entitlement, by contrast, is **never overwritten once non-empty**, so re-running approve cannot reset a richer set an admin configured later back to the minimum. Their ordering is deliberate too — the allowlist block runs *before* the already-entitled early return, or a re-run would never reach it.

When a bind fails right after the account row was created, the orphan row is deleted on a **detached** context: the failure may *be* context cancellation (shutdown racing the onboarding goroutine), and reusing it would just leak the row.

### The failure posture of the read path

`AccountFor` is **load-bearing for routing**, not merely for display. So it distinguishes three outcomes, not two:

- **Genuinely unbound** → the router sends non-admins to onboarding.
- **Bound, entitlement read** → normal routing.
- **Bound, entitlement not read** (store blip) → reported as "bound, flow unknown".

The third is the point. Folding a store blip into "unbound" would send an entitled member into the onboarding flow on a hiccup, swallowing their turn and emitting a false "registered, pending admin approval" notice. So unknown stays unknown: the contract carries an explicit flag for "was the entitlement actually read", and the router **fails closed** for non-admins when it is false — enforcement must not lean on a read that did not happen. It self-heals on the next turn.

That is a different thing from a **known-empty** entitlement, which is a bare account awaiting approval — a definite answer. Without the flag the two are indistinguishable (both are the empty string).

### The usage buffer

`10-buffer.go` accumulates per-handle usage in memory. A per-turn read-modify-write against the store would hammer one row for an active user; folding into a mutex-guarded map and draining periodically turns it into one write per active handle per window, regardless of turn count.

The drain rides the trunk housekeeping scheduler — it is a **write**, not a prune, but having every periodic database-touching job centrally scheduled and staggered beats each spinning its own timer. Module stop does a final drain so a graceful shutdown does not lose the last window.

The best-effort boundary is stated precisely: `Add` never blocks and never fails a turn; a per-handle drain failure logs a warning and **re-stages** the entry for the next window, so usage is not dropped while the process lives; a crash loses the unflushed window. The known cost is spelled out too — the map's size bound holds only *while drains succeed*. A prolonged store outage re-stages everything each cycle with no cap, so the map grows with the number of distinct handles seen during the outage. That is recorded and accepted rather than ignored.

Two locks do different jobs. One guards the map; the other serialises the drain **body**. Snapshot-and-clear makes the map race-safe but not the per-handle read-modify-write, and the drain is reachable from both the periodic tick and shutdown — without the second lock two drains could each read the same row and lose a delta. `Add` never takes the second lock, so a turn is never blocked by a drain.

### API keys

`60-apikey.go` is machine identity. The life of a key:

**Minting** produces a random token and persists only its hash — the plaintext is returned to the admin exactly once and is afterwards unrecoverable, including by the system itself. Minting is **all-or-nothing**: besides the key row it binds a **synthetic billing handle** (a dedicated source family plus the key id) so the model pool's per-handle ledger rolls the key's spend up under the owning account with zero new billing machinery. If that bind fails the key is rolled back — a billing credential must never exist working-but-unbilled, or its spend strands forever under an unjoined handle. The synthetic source name is pinned to a single constant in the contract layer, because billing correctness depends on the producer and the ledger agreeing on that string byte for byte.

**Verification** hashes the presented token and looks it up. An unknown token, a revoked key, and a paused owning account all return **the same result**, so a caller cannot probe which one it is. Account status is a join in the same query, so pausing an account instantly disables all of its keys with no extra round-trip.

A hit also bumps a last-used timestamp, but **coarsened**: verification runs on every request, and an unconditional write would hammer one hot row under a batch caller. The field is a rough "recently active?" signal, so one write per key per interval is plenty. The write uses a detached context — a request whose context cancels right after auth should still be recorded — and a write failure must never invalidate an otherwise valid key.

**Policy** comes in two mutually exclusive forms: a **lane allowlist** (which model lanes the key may enter, with each request naming the one it wants) or an **exact entry allowlist** (pin the key to specific backends, for evaluation runs or to fence it away from some). Both empty means the key reaches nothing — fail-closed again, not "anything goes".

The lane form deliberately carries **no tag allowlist**: a request's tags are a soft preference that only reorders candidates within a lane, and cannot widen or narrow reachability. Fencing on it would be self-deception. Fence on entries when that is what is meant.

The mint command is deliberately pedantic about its arguments, and the pedantry has a history: any token that *looks like it was meant as an option* — a trailing comma (the list was cut in two by a space), a stray `=`, a bare option name — is refused rather than silently worn as a label. Those near-misses used to mint a key with entirely the wrong policy, and the mistake surfaced much later as a rejection pointing nowhere near its cause. By the same logic, a policy option written but empty is a mistake rather than a request for the default, and a typo'd lane name errors immediately instead of handing over a key that can never match anything.

### Management commands and panel

| Subcommand | Who | What |
|---|---|---|
| (bare) | anyone | Answers "who am I here" |
| `new` | anyone / admin | Non-admins create and bind themselves; an admin may create an empty account |
| `show` | anyone | Details of one account; with no id, your own |
| `list` | admin | The full account table |
| `mint` | admin (DM) | Mint a binding code to hand to someone |
| `approve` | admin | Grant a pending bare account access |
| `flow` | admin | Set an account's flow entitlement directly |
| `pause` / `resume` | admin | Toggle account status |
| `bindto` | admin (run via token) | The binding action itself; a binding code's batch invokes it |
| `apikey` | admin (DM) | Mint / list / revoke API keys |

`/accounts` is registered as a non-admin command because its "new" path must stay open to any already-admitted sender; every other subcommand checks admin status inside. A bare `/accounts` answers "who am I here" before printing usage — every other subcommand wants an account id, and until then nothing told you your own.

**Status and entitlement are orthogonal axes.** Entitlement answers "which paths may you take"; status answers "is this account serving right now". Pausing an account does not touch its entitlement field (so resuming restores nothing), it simply makes access fall back to the onboarding path with a suspended notice. The same status bit is a join condition in the API key verification query, so pausing instantly disables every machine credential under that account.

Any subcommand that emits a secret is **DM-only**: minting a binding code or an API key in a group would hand it to everyone present.

`/bindaccount` is the self-service face: an already-bound user mints themselves a code to attach another channel to the same account. A self-code cannot repoint — it can only bind a handle that is currently unbound.

The panel (`90-panel.go`) is an admin-only account roster: one count stat plus a **server-side paged** table. Usage rollups are per-account joins, too costly to run for every account on every tick, so they belong to a detail drill rather than the roster — the roster stays light.

---

## The store subpackage

`leaf/accounts/store` is the module's persistence. The identity model, handles, usage accounting types and the `Store` interface all stay **here** rather than in the contract layer — the `Manager` and this implementation are their only users, so the seam is internal to the module; the flow layer reaches accounts only through `contract.Accounts`. It is a worked example of the rule that types shared across a direct coupling do not earn a place in the contract.

### Three tables

The **accounts** table is the logical person: display name, note, flow entitlement, status, onboarding scope, timestamps. The **handles** table is the bound entrypoints, unique on (platform family, stable uid) and indexed by account, cascading on account delete. The **per-handle usage** table is keyed on the same pair and carries a JSON blob plus that sender's language seed — note that it needs **no binding**: an unbound person's usage and language are recorded all the same.

The **API key** table stores only the hash (unique — it is the verification lookup key), plus a label, the policy columns, status and timestamps, cascading with the account.

### Migrations

Migrations are **data-preserving** (accounts holds real user data, not a cache that can be wiped on a schema bump) and every step is written to be safely re-runnable. A couple worth recording:

- Introducing "access is granted, not default" required a **run-exactly-once** backfill stamping the basic entitlement explicitly onto every pre-existing account, so nobody lost access in the flip. It matches on **whole tokens** rather than substrings, so it neither re-stamps an account that already has the entitlement nor matches some longer name that happens to contain the word. Because it is version-gated, bare accounts created afterwards are never re-entitled by it.
- The API key policy changed shape once: the fence moved from a scalar on the key to a routing parameter on the request, and a key now holds a lane **allowlist**. The old columns were **not dropped** — dropping a column is irreversible and keeping it costs nothing. Old-form keys therefore land in the fail-closed "reaches nothing" state and need re-minting, while old keys pinned to explicit entries keep working.

### Downsampling the usage blob

`30-blob.go` is the pure, I/O-free half: one row per handle holding a JSON map of **period key → counts**, downsampled by age — recent periods daily, middle-aged monthly, old ones yearly. The folding is lossless (counts carry forward), so the **sum stays exact** while the key count has an upper bound almost independent of how long the deployment has run — a few hundred keys, a few kilobytes, over a century.

One turn is one read-modify-write: turns, successes, per-**kind** token counts (language model / OCR / translation, each with input and output), and per-**native-unit** service usage (audio measured in seconds, say). Tokens and native units are structurally disjoint, so summing one can never fold in the other.

There are two deliberate fail-open points: a malformed blob is reset to an empty map on **write** (this write overwrites the corruption and the handle self-heals) and sums to zero on **read**. Without that, the buffer would re-stage the entry every window for the life of the process — turning an already-unreadable history into a permanent malfunction.

### Read and write choices

A few intentional decisions in the storage layer:

- **Write explicitly rather than relying on column defaults.** Creating a normal account writes the entitlement explicitly; creating a bare one writes the empty entitlement explicitly. Two sources of truth drift eventually, and here drift means an account that should have no access is born with some.
- **Aggregate rather than N+1.** The roster shows a per-row channel count, obtained from one aggregate query rather than a per-row fan-out.
- **Revoking flips status, it does not delete.** The key row stays: historical usage under its bound handle must remain readable, and the hash should stay reserved.
- **The language seed is written once**, via a condition on the conflict update, so the seed stays stable — a mid-conversation switch belongs to the session runtime, not here.

---

## Design rationale

**Identity is layered under access.** Minting produces identity only, and no automatic path ever produces access as a side effect. That constraint is what makes "this source is open for onboarding" a safe setting: a stranger can arrive, speak, and be registered, but receives nothing until an admin presses approve.

**No implicit entitlement floor.** An empty entitlement grants nothing. The earlier arrangement treated basic conversation as a free fallback, which meant access was not an explicitly granted thing — and therefore not an explicitly revocable one either. Making it explicit required a one-time backfill to carry existing users across; every grant since then is visible in the field.

**Unknown is not the same as no.** A failed read reports unknown and leaves the posture to the consumer (the router fails closed for non-admins, and self-heals next turn). Folding a blip into "unbound" manufactures a confident-sounding answer that is in fact invented — and that answer directly changes what the user is shown.

**Billing keys on handles, not accounts.** Unbound people spend money too and should be recorded; the account-level figure is a read-time rollup. The choice also makes operations like merging two accounts naturally safe — handles re-point and their history follows, with no usage data to migrate.

**Machine identity rides the same ledger.** An API key is not a parallel billing system; it is one more handle under the account whose "platform" happens to be synthetic. Adding a new kind of machine entrypoint therefore needs no new billing machinery — binding a handle is the whole job.

---

See also: [Architecture overview](../architecture.md) · [flows](../flows.md) · [gate](gate.md) · [claimtoken](small-modules.md#claimtoken) · [modelgate](modelgate.md) · [model](model.md) · [agora](agora.md) · [webui](webui.md)
