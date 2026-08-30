# webui

The browser-side admin surface. It is a **generic renderer**: it knows nothing about any subsystem and only draws the panels other modules register into it.

---

## Place in the architecture

**Provides**

| Capability | Consumers |
|---|---|
| `contract.PanelRegistry` | nearly every module with runtime state |
| `contract.TakeoverMinter` | [tools](tools.md) (`escalate_to_coo` mints a token when handing off to a human) |

**Needs**

- `contract.SlashRegistry` — see [slash](small-modules.md#slash). webui publishes `/webui` and `/takeover` into the shared command table.
- `contract.ClaimTokens` — see [claimtoken](small-modules.md#claimtoken), which owns the lifecycle of the one-time unlock tokens.

**Soft edges (`TryRequire`; absent → degraded)**

- `contract.AdminLine` — pushes a fresh unlock token to the admin channel at startup.
- `contract.BrowserTakeover` / `contract.BrowserControlHold` — provided by [tools](tools.md); the backend behind the takeover endpoints.
- `contract.SessionResume` — provided by [session](session.md); wakes the right session once a human hands the browser back.

All four resolve **lazily**. Their providers all `Need` the `PanelRegistry` webui provides, so the topological order necessarily puts them after webui; resolving during `Start` would only ever see nil and silently kill takeover. Resolving on first use happens long after boot instead.

---

## What it does

**The panel registry.** This is the module's largest effect on the rest of the project; see below.

**A read-only dashboard.** A background poller re-reads every panel's `State()` on a fixed tick and publishes the result as one snapshot. HTTP handlers only ever read that snapshot; they never trigger a fresh poll. An unauthenticated visitor gets the snapshot with `AdminOnly` panels stripped out.

**One-time token unlock.** There are no usernames or passwords. An admin runs `/webui` in chat, receives a short-lived single-use token, pastes it into the page's one input box, and the server exchanges it for a session cookie.

**A single input box (the dock) that runs commands.** An unlocked admin can execute slash commands right there; the reply is captured and rendered in place. The runnable set is an explicit allowlist — only commands that are global, administrative, and bounded by their own arguments. Commands that need a real chat context (opening a session, inspecting the current one, and so on) are deliberately excluded.

**Live browser handoff.** When the agent hits a login wall or a captcha it cannot clear, it can hand the browser to a human. webui relays the live view (a server-pushed stream of frames), forwards input, holds a heartbeat-leased human-control lease, and drives the save-the-login writeback. Authorization is entirely webui's job — the browser service itself knows nothing about user business.

**Public legal pages.** The `legal/` subdirectory holds two static HTML documents (terms of service, privacy policy) served on **unauthenticated** routes so the connected chat platform and its users can fetch them. No logic beyond that.

---

## Internal structure

### The panel registry

The implementation in `10-registry.go` is just a locked `id → contract.Panel` table that returns its entries sorted by id. It deliberately mirrors [slash](small-modules.md#slash):

- webui provides the registry; each panel-owning module pushes once during its own `Start`;
- a panel is a **struct of closures** (`State` / `Read` / `Apply` / `Page` / `View`) built over the owning module's own state. So webui imports nothing but `contract`, and no subsystem knowledge ever seeps into the renderer;
- a duplicate id or a nil `State` panics outright — the same posture as registering a capability on the trunk: wiring bugs must surface at startup.

This is why so many modules in `wantGraph` carry `needs contract.PanelRegistry`: a dozen-odd leaf modules and two flows each hang a panel on it. **Every module describes itself**, and webui only understands one fixed field vocabulary:

| Kind | Rendered as |
|---|---|
| `stat` / `status` | a value, or a status lamp coloured `ok` / `warn` / `down` |
| `table` | a table, with optional server-side paging, tag filtering, per-row buttons |
| `text` | a multi-line explanation |
| `graph` | a node/edge graph banded into tiers; horizontal bands or a radial half-disk |

A single table cell may carry a "why" detail, a copy affordance, a drill-down link, or a **prefill button** — clicking which executes nothing: it writes a slash command into the dock for the admin to read and press Enter on. Writes are therefore always prefill-then-confirm, never an implicit side effect hidden behind a control.

`Panel.Upstreams` lets each module declare who it depends on; the home dependency graph is assembled from those self-described edges. No topology is maintained centrally anywhere.

### The HTTP layer and the snapshot

`20-server.go` builds the routes, runs two background loops, and publishes the snapshot. The whole HTTP surface falls into a few gating tiers:

| Tier | Endpoints | Notes |
|---|---|---|
| Public | index · frontend script · `legal/*` · splash text | static assets and the legal pages |
| Public but redacted | the state snapshot · panel paging · panel views | `AdminOnly` panels are removed wholesale for an unauthenticated caller |
| Session exchange | submit a token · log out · ask your own coverage | "who am I" returns a coverage, never an identity |
| Admin session required | panel setting read/write · running a dock command | every write lives here |
| Per-scope authorized | the takeover endpoints | the coverage must cover the target scope; an absent backend answers "unavailable" uniformly |

The index route is **pinned to exactly one path**. The frontend has no client-side routing (the script only ever fetches registered endpoints), so a catch-all index would answer 200-with-HTML on every mistyped path — a fat-fingered API URL would then surface as a confusing JSON parse error instead of a plain 404.

Polling is a **single sequential worker**. A panel whose `State` panics is isolated into empty fields with a warning; it cannot take the poller down or cause later panels to be skipped. There is deliberately **no** per-panel timeout or goroutine fan-out: a Go call you are already blocked inside cannot be skipped — only abandoned on another goroutine, which is exactly the shape that leaked before. The real isolation is **between concerns**: expiring tokens and sessions are swept on their own ticker, so a wedged panel can never delay it. And even when the snapshot stops refreshing, handlers keep serving the last published copy and never block.

The startup posture is equally deliberate. webui is **non-optional** (so other modules may hard-depend on its registry), yet its `Start` **never fails on a bind problem**: a port conflict is logged, no pages are served, the registry is provided anyway, and the agent carries on. Binding to a non-loopback address raises an extra warning.

### Authorization and "coverage"

`30-auth.go` has exactly one concept: **coverage**, a string the auth code never interprets.

- coverage `*` — the global management session; may reach every write/admin endpoint.
- coverage `<scope>` — a **capability session** that may take over that one scope's live browser and reaches no admin endpoint at all.

Per coverage there is at most one pending token and one live session: re-minting is a **hard reset of that coverage** (the previous token is voided, the previous session killed). Because coverage is opaque, this code carries no organisational or browser knowledge — it only ever compares strings. The population of live coverages is capped; `*` is always allowed (it re-mints in place), and only a *new* scoped coverage is refused past the cap.

The one-time tokens themselves live in the [claimtoken](small-modules.md#claimtoken) facility. webui keeps only a coverage → current-pending-token mirror, so a re-mint can void the previous one, plus the long-lived session cookies it owns outright.

### The frontend shell

`40-page.go` embeds the whole frontend (a single script) into the binary at build time. The home page's DOM has exactly two elements: a full-window canvas and the one input box. Every form — layers, the editor, drill-downs, the takeover viewport — is painted on the canvas; code editing borrows the same input box, which grows multi-line in edit mode.

### Running commands from the dock

`60-run.go` synthesises one dock line into a "webui-origin admin DM" event, dispatches it through the ordinary slash path with a **capturing sink** (accumulating the reply instead of delivering it), and returns the captured text plus a success/failure marker. The allowlist lives here, not in the frontend.

### Takeover endpoints

`70-takeover.go` is a handful of handlers relaying the browser service's takeover face to a human: a frame stream, input, the control toggle, save-the-login, and an admin listing of which scopes currently have a live browser. Every per-scope endpoint runs a coverage check first (`*` sees all; a capability session sees only its own).

Two details are worth recording:

- **Saving requires currently holding control.** A watch-only viewer clicking save would write a login-less copy back over the master, so the request is refused outright; the frontend surfaces the refusal and the retry passes once control mode is entered.
- **Save and "toggle control off" judge hand-back differently.** Toggling off counts as an explicit hand-back only if a live lease was actually released — a lapsed lease must not be mistaken for the human walking away, or one heartbeat gap would be read as a hand-back. Clicking save, on the other hand, *is* an explicit hand-back, so it must wake the waiting worker even if the heartbeat happened to lapse; otherwise the human has already cleared the login while the agent sits idle forever.

`50-slash.go` is the chat-side entrance to the same path: `/takeover` confirms a live browser exists before minting a scope-locked token. For non-admins it is DM-only — the token rides the reply, and replying in a group would leak it. It also handles one wrinkle: a fanned-out member's browser copy is keyed under a `group#member` sub-scope that the bare group scope cannot see, so live copies are enumerated and prefix-matched. Crucially, **an explicit member name that matches nothing fails loudly** rather than falling back to the group scope: that fallback would mint a token for the wrong browser, byte-identical to a legitimate reply.

---

## Design rationale

**The renderer knows no subsystems.** The alternative — webui importing modules and knowing how to draw the model pool, how to draw sessions — would make the UI an inverted dependency sink and break the hard rule that leaf modules never import each other. With self-description, adding a panel costs webui zero lines. Adding a new *field kind* is the thing that requires review; the fixed vocabulary is the point.

**Every write is prefill-then-confirm.** A button in the UI carries no authority; it only writes a command into the input box. Execution still goes through the same slash command and its own gating, so the dashboard never becomes a second authorization bypass.

**Degrade, don't abort.** A port conflict, a wedged panel, a missing takeover backend — each only makes the interface worse and never touches the agent's ability to handle messages. Conversely, wiring bugs (a duplicate panel id, a nil `State`) panic at startup: those are programmer errors, not runtime conditions.

**Coverage is an opaque string.** The auth code could have understood "this is a group", "this is a member" — which would have copied the organisational model into the UI layer. Comparing strings instead means the takeover capability can redefine what a scope means without webui changing a line.

---

See also: [Architecture overview](../architecture.md) · [slash](small-modules.md#slash) · [claimtoken](small-modules.md#claimtoken) · [tools](tools.md) · [session](session.md)
