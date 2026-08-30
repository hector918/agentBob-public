# gate

The first gate on the way in. Every message from every platform is triaged here — "may this person talk to bob at all?" — before it earns a place in the pipeline behind it.

---

## Place in the architecture

**Provides**

| Capability | Consumers |
|---|---|
| `contract.Screener` | the inbound flow (its only caller — see [flows](../flows.md)) |
| `contract.AccessGranter` | [accounts](accounts.md) (auto-allowlists an applicant as a side effect of binding) |

**Needs**

- `contract.ClaimTokens` — see [claimtoken](small-modules.md#claimtoken). The admit code carried in a bounce notice is minted and verified there.
- `contract.SlashRegistry` — see [slash](small-modules.md#slash). Registers the `/gate` command, and dispatches the batch a redeemed token carries through the same table.
- `contract.PanelRegistry` — see [webui](webui.md). Hangs a panel.

The module is **non-optional**: without the screen, a gated source has no authorization at all. So a failed policy load must abort startup rather than fail open.

Only the central inbound flow calls it. No platform-side source screens earlier, even where a protocol would naturally let it see the sender sooner; access control is deliberately concentrated at this one point.

---

## What it does

**One access policy per source.** Each connected source has a yaml file describing: whether the source is open at large, an allowlist, a denylist, an admin list, per-group overrides, and whether onboarding is open. The filename is the source name. **The zero value is closed** — a gated source whose file is missing or failed to load fails shut, never open.

Connection configuration (enabled or not, where credentials come from) is **not** here; that belongs to the central config and the source bus. This file carries access policy only, and hot-reloads.

**A three-step gate order.** Every inbound event passes in a fixed order:

1. **Denylist** — a hit is discarded, with no reply and no bookkeeping (see below).
2. **Bare-code redeem** — if the message body *is* a live token, it is itself the credential and bypasses the allowlist.
3. **Allowlist** — otherwise, ask the policy whether this person is authorized in this chat.

There are three verdicts: **pass**, **drop**, and **redeemed**. "Redeemed" is a distinct terminal state: the event has already been consumed and must not proceed, but may carry a localized receipt for the caller to deliver by its own platform's rules.

**Onboarding.** A source can opt in to onboarding one at a time: a stranger who is not allowlisted (and not denied) is **passed** through to the router, where the intro flow creates a bare pending account. This is not "open to everyone" but "open to *onboarding*" — real admission sinks into a granted entitlement on the account. It is off by default, so a deployment with no account system is never silently thrown open. The verdict carries a flag telling downstream "this one came in through onboarding", and the inbound flow uses it to **withhold** the slash-command fork from such senders: a `/…` from an unapproved stranger should land in the onboarding conversation, not execute a state-mutating command.

**A bounce notice carrying an admit code.** A stranger turned away gets a friendly explanation carrying a **freshly minted admit code**. The code is not a permission — it is a batch of **frozen commands** that admits exactly this applicant, at exactly the scope where they were turned away. A real admin redeems it by pasting it. It is safe to show in a public group: the grant target is frozen at mint time, and an ineligible redeemer commits nothing at all.

It is sent on first contact, then goes silent for a cooldown, then re-arms once. Long enough that a chatty stranger cannot turn the bot into a spammer; short enough that someone who missed the first notice is not left waiting.

**A rejected-senders ledger.** A bounded, deduplicated in-memory ledger of recently turned-away senders. It solves the chicken-and-egg of a fresh deployment: "I need my own id to allowlist myself, but the message that carries it is exactly the one being dropped." An operator sees the ids on the panel and admits with one click.

**Policy writes and hot reload.** Admitting someone edits that yaml and then swaps the live policy in place. The webui panel also carries a whole-file editor whose save reloads.

---

## Internal structure

### The policy model (`10-policy.go`)

`Policy` is one source's complete access policy; `Group` is a per-group override. The merge rule is explicit and two-tier:

```
denied (source-level OR this chat)              → deny
else on this chat's allowlist_add, or chat open → allow
else chat has an allowlist and user isn't on it → deny
else source is open, or user is on its allowlist → allow
```

The three per-group grant semantics are kept deliberately separate: `allowlist` **narrows** (only these people, here), `allowlist_add` is a **union** (these people on top of the source-level list), and `allow_all` **opens this chat** (a globally closed source can still have one wide-open room). The unconditional union grant is checked *before* the narrowing gate, or a populated per-chat list would shadow it.

Id matching is exact string comparison, with one carve-out: ids containing `@` (email addresses) compare case-insensitively. The email side canonicalises senders to lowercase while operators naturally type mixed-case addresses into the yaml; an exact compare would silently miss — and a missed allowlist entry fails closed (tolerable) while a missed denylist entry fails **open** (not). Platform ids never contain `@`, so their exact match is untouched.

At load time, **a single unparseable file is fatal**: better a loud "this file is broken" for the operator than a source silently falling back to the closed default. A **typo'd top-level key**, on the other hand, is not fatal — one typo should not brick every source — but it raises a prominent warning, because it produces the same unexpectedly-closed source.

### The screener (`30-screen.go`)

The `Screener` implementation: it holds the hot-swappable policy table, the token facility, and the rejected ledger. The table is replaced atomically as a whole, so a reload is seamless with respect to concurrent inbound events.

A denylist hit is **not** recorded in the ledger. That ledger exists for bootstrap discovery of strangers to admit, and a deliberately blocked sender is its exact opposite: recording them would pollute a capped ledger, age out the ids that matter, and the panel's per-row "allow" button would be a silent no-op on them anyway (denial wins over everything).

Minting a bounce code also **retires the one it replaces**. Every re-arm mints anew, so without this a chatty stranger would accrue a live token per bounce. At most one live admit code exists per (source, chat, user), and its lifetime is short — a live grant code has no business lying around for days.

### Redeeming a batch (`36-batch.go`)

What a token carries is not a permission but a **batch of commands**. Redeeming dispatches them one by one through the ordinary slash path, with a capturing sink folding each reply into a single receipt.

Three things matter here:

- **Where the authority comes from.** Each command runs with "the token's own authority OR the redeemer's **real** admin status". The real status is resolved here, because the admin flag on the event is not stamped until after screening returns. So a non-admin bounce code only runs its admin-only command in a real admin's hands, while an admin-minted code explicitly marked "run as admin" works for whoever presents it (bearer pre-authorization).
- **An ineligible redemption stays inert.** An applicant pasting their own bounce code commits nothing — and in that case **no receipt is emitted**: it falls straight through to normal gating (token kept, sender bounced as usual). Emitting a receipt would be a liveness oracle for the code.
- **Burn only on a full success.** The token is consumed only when the whole batch committed; a partial failure keeps it at its original lifetime so the redeemer can retry. Every batched command is idempotent (an allowlist add is a no-op, an approval does not clobber), so a re-run simply re-applies the failed step.

Each command's dispatch is bounded by a timeout. Redemption runs on the **single inbound consume loop** and does storage and file I/O synchronously, so one hung round trip would head-of-line-block all inbound traffic.

### Policy writes (`50-write.go`)

The `AccessGranter` implementation. Two write paths — a source-level grant and a per-chat union grant — both read-modify-write with an atomic replace, serialised by one lock.

The write is **yaml node surgery**, not a round trip through the typed `Policy`. Re-marshalling the struct would silently drop everything the types do not model — per-group custom keys most of all. Node surgery rewrites only the target sequence and leaves the rest of the document (nested groups, unmodeled keys, comments) verbatim.

A few details:

- **User-controlled values are always quoted** on the way in. An id that happens to look like a yaml literal (all digits, or spelled like `true` / `null`) would come back on the next load as a typed scalar and the entry would silently vanish. Structural keys stay bare.
- A blank `allowlist:` line is a null scalar, not a sequence; appending to its content would be silently ignored on marshal, so it is upgraded to an empty sequence in place first.
- **Denial beats a grant.** Writing an allowlist row for a denied user would produce a self-contradictory record the screen ignores anyway, plus a false success. It is refused instead, with a dedicated sentinel error so callers can say "take them off the denylist first".
- **"Already present" is not a terminal success.** A previous attempt may have landed the write and then failed the reload (loading is all-or-nothing, so one broken sibling file can fail it), leaving the live policy stale. So that path reloads and clears the ledger row too.

The panel's whole-file save takes the same lock as the surgical path — an atomic rename only prevents a truncated file, not a lost update.

### The rejected ledger (`20-rejected.go`)

Bounded, deduplicated, concurrency-safe. Eviction is by **first seen**, not last seen: the latter would let a noisy repeat-rejector keep refreshing itself and pin its own entry while ageing out the one-shot bootstrap id the operator is actually here for — the exact opposite of the ledger's purpose.

Rows are forgotten after a grant. A source-level grant clears that person's rows across **all** chats; a per-chat grant clears only that **one** chat's row, so someone turned away in several groups is still queued in the ones where they remain unauthorized.

### Assembly and panel (`40-module.go` / `90-panel.go`)

The module wires the pieces together, provides both capabilities, registers `/gate`, and hangs the panel.

`/gate` has two forms — source-level and per-chat. The panel's per-row "allow" button prefills the form **matching where the sender was turned away**: a group sender gets the per-chat form, so one click does not over-grant them across every chat on that source. Same discipline as the token path: a one-click admit never grants more broadly than a redeemed code.

The panel also carries one yaml editor per source. Its save deliberately validates by unmarshalling into the **typed** struct rather than a loose map: a type-mismatched value passes a map probe, gets committed, and then fails the load on the **next** startup — and since gate is non-optional, that means the process does not come up at all.

---

## Design rationale

**Closed is the default *and* the zero value.** An uninitialised Go struct here means "nobody may talk". Every "file missing / parse failed / key misspelled" path therefore lands on the closed side by construction, with no check that anyone has to remember to write.

**An admit code is a batch of commands, not a permission.** The alternative was a token carrying "permission X", granted at redeem time. With a batch of **frozen commands** instead, the token's maximum effect is fixed and fully legible the moment it is minted — and redemption goes through exactly the same slash commands and gating an admin uses by hand, so there is no second authorization path to audit separately.

**Discovery before configuration.** The rejected ledger is not strictly access control; it is **operability**. Without it, step one of a fresh deployment is grepping logs for your own id. It is capped, it evicts, and it is empty after a restart — deliberately a volatile bootstrap aid rather than a record.

**The screen owns the access axis only.** "May this person talk" and "is this person's account approved" are two different questions. gate answers the first; the second is an entitlement downstream on the account. The onboarding path is precisely the product of that split — the allowlist degrades into a roster, while the denylist stays the hard block.

**Policy is a file, not a table.** Access policy stays in yaml on disk rather than in the database. The cost is having to do atomicity and serialisation by hand; the payoff is that an operator can read, edit, and back it up with bob out of the loop entirely — including when bob will not start *because* the policy is broken. That is why the write path is surgical: it has to put ink safely onto a file a human has been editing, comments and unmodeled keys included.

---

See also: [Architecture overview](../architecture.md) · [flows](../flows.md) · [accounts](accounts.md) · [claimtoken](small-modules.md#claimtoken) · [slash](small-modules.md#slash) · [webui](webui.md)
