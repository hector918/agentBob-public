# agentbob — commands reference

agentbob exposes two kinds of commands: **CLI subcommands** you run in a
terminal (`bob <cmd>`), and **slash commands** you type into a chat
(`/<cmd>`). Slash commands run directly in agentbob and never wake the
LLM — an unknown `/cmd` just gets a "no such command" reply. To talk to
the model, send a regular message (in a group, @-mention the bot or
reply to one of its messages).

For install / setup / `BOB_HOME` layout see [README.md](README.md).

---

## CLI subcommands (`bob …`)

Every subcommand honours `--home <path>` (or `$BOB_HOME`) for per-bot
home directories.

| Command | What it does |
|---|---|
| `bob` / `bob chat` | Start an interactive local chat (REPL against the configured model). |
| `bob gateway` / `bob gateway run` | Run the messaging gateway in the foreground (Ctrl-C to stop). |
| `bob gateway start` | Start the gateway detached in the background. |
| `bob gateway stop` | Stop the backgrounded gateway. |
| `bob gateway status` | Show whether a backgrounded gateway is running. |
| `bob gateway restart` | `stop` (if running) + `start`. |
| `bob doctor` | Diagnose the environment (home writable, config valid, store open, model resolved, …). |
| `bob version` | Print the version. |
| `bob sessions [--source <s>] [--limit <n>]` | List recent conversation sessions across the store. |
| `bob config path` | Print the home / config / `.env` paths. |
| `bob config show` | Print the effective config (defaults merged with `config.yaml`). |
| `bob config init` | Write a default `config.yaml`, `config.yaml.example`, `models.yaml.example`, and `.env.example` into `$BOB_HOME`. |
| `bob config set <key> <value>` | Set one config value. Supported keys: `agent.name`, `display.stream`, `attachments.{enabled,max_download_mb,retention_days,max_total_mb}`, `attachments.platforms.<name>.{enabled,max_download_mb,retention_days,max_total_mb}`, `cleanup.{sessions_retention_days,logs_max_size_mb,logs_keep,sweep_interval_hours}`, `logging.{level,target}`, `gateway.max_pending_batch`. Model endpoints + `defaults.{max_output_tokens}` + per-entry `max_streaming_output_tokens` live in `models.yaml` (`min_context_window`/`max_input_tokens` retired — the turn sizes to the serving entry's declared `context_window`) (use `bob model` or edit `defaults:` directly). Telegram admins / allowlist / denylist live in `sources/telegram.yaml` (edit directly). |
| `bob model` / `bob model list` | List entries in `$BOB_HOME/models.yaml` (name, provider, tags, context, concurrency). |
| `bob model show <name>` | Print one entry's full YAML record. |
| `bob model presets` | Show the built-in provider presets (default base URL + API-key env per provider). |
| `bob model add` | Interactively append a new entry to `models.yaml`. |
| `bob model edit <name>` | Interactively edit one entry (Enter keeps the current value). |
| `bob model remove <name>` | Remove an entry (asks y/N; if it was the last entry, deletes `models.yaml`). |
| `bob model import-openrouter <query>` | Fetch OpenRouter's public model list, substring-match `<query>`, append matches as disabled entries (priority -10001). |
| `bob model pause <name>` / `bob model resume <name>` | Stub — points you at the in-chat `/model pause` / `/model resume` (runtime state only lives in the running gateway). |
| `bob model stats [--since 168h]` | Per-entry usage (calls / errors / tokens) aggregated from the `model_usage` table over a `--since` window (default `168h`). |
| `bob db migrate [--session-db PATH --urllib-db PATH --batch N]` | Copy `bob_*` data from local sqlite (`state.db` + `urls.db`) into the configured Postgres DSN. Idempotent (INSERT-ON-CONFLICT-DO-NOTHING). |
| `bob db reattach` | POST to the running bob's loopback admin endpoint and ask each fallback store to flip back from sqlite secondary to primary pg (if pg is healthy). |
| `bob debug prompt <session_id> [--iter N] [--raw]` | Reconstruct + print the exact system prompt + assembled messages for one session (truncates to 600 runes unless `--raw`). |
| `bob skill list` | List installed skills under `$BOB_HOME/skills/`. |
| `bob skill install <path>` | Install a skill package from a local path. |
| `bob skill uninstall <name>` | Remove an installed skill. |
| `bob skill lint <path>` | Validate a skill's `SKILL.md` + structure without installing it. |
| `bob agora …` | Agora multi-agent management — see [docs/agora.md](docs/agora.md). Top-level CLI subcommands: `seed-yaml`, `bootstrap`, `roles list`, `reload`, `send`, `report`, `dispatch overview`, `company …`, `member …`, `hire`, `terminate`, `channel …`, `inbox …`. The same operations are available in chat under `/agora`. |
| `bob credential list` | List credentials known to the broker. |
| `bob credential get <name>` | Print one credential's metadata (secret fields masked). |
| `bob credential set <name> [--from-stdin] …` | Create / update a credential. Prefer `--from-stdin` so secrets don't hit shell history. |
| `bob credential delete <name>` | Remove a credential (idempotent). |
| `bob credential migrate` | Migrate legacy credential files into the current layout. |

---

## Slash commands (`/…` in a chat)

**Roles.** Some users are **admin** for a given source. Admin membership
is declared per-platform in `$BOB_HOME/sources/<platform>.yaml` (e.g.
the `admins:` field of `telegram.yaml`); the **local terminal user is
always admin**. Admin-only commands reply with a polite "admin
required" message to non-admins.

**`BotSafe` gate.** A few slashes that disrupt conversation state
(`/new`, `/close-session`) are refused when the message arrives via the
agora internal-bridge — a peer bob's LLM output is prompt-injectable
text, so we don't let it type those by accident. Read-only and
per-sink slashes stay open from any sender.

### Session management

| Command | Role | What it does |
|---|---|---|
| `/new` | user | Open a brand-new session in this scope and move the active cursor to it. Existing sessions stay alive. |
| `/session` | user | Show the scope + session id this message routes to. |
| `/whoami` | user | Show your identity — source, user id, role (user/admin) — and the routing scope this message resolves to. |
| `/close-session` | user | Stop the turn currently running in this scope, including any in-flight tool calls. (It cancels the live turn; the session itself stays addressable.) |
| `/trace [on\|off]` | user | Toggle per-scope trace rendering (tool progress / debug detail inline). No argument toggles. **In-memory only** — a process restart resets the override to the configured default. |
| `/prompt [text]` | admin | Dump the **exact assembled live prompt** this message would feed the model (system prompt + history + tools + skills) to you once — read-only: no model call, no persist, no history change. With `text`, builds the prompt as if you'd just sent that. Reply to an old bot message + `/prompt` to inspect THAT session's prompt. |

### Information

| Command | Role | What it does |
|---|---|---|
| `/tools` | user | List the tools currently available to you (the same per-identity projection the agent loop would be offered). |
| `/skills` | user | List the skills available to you. |
| `/skills reload` | admin | Re-scan external skills and reload the permission matrix. Admin-only. |
| `/help` | user | List the commands available to you (admin sees the admin-only commands too). |

### Accounts & identity

One **account** can own several channel handles (a Telegram user, a Feishu
open_id, an email address) so the same person is recognized across
entrypoints. An account carries a **flow** entitlement (which flow handles
it, e.g. `normal` / `agora`). See [docs/accounts.md](docs/accounts.md).

| Command | Role | What it does |
|---|---|---|
| `/accounts new <name> [--me] [--empty]` | user / admin | Create an account. **Non-admin** (or a first-time admin with no account on this handle): self-register — binds your current channel to a fresh account. **Admin** with an existing account: creates an empty account for someone/something else; `--me` forces binding your own channel, `--empty` forces an unbound account. |
| `/accounts` (no args) | user / admin | Which account **this channel** resolves to (id / name / flow / status), then the usage line. The way to learn your own `ac_id`. |
| `/accounts list` | admin | List accounts (id / name / flow / handle count). |
| `/accounts show [ac_id]` | user / admin | One account's handles + per-kind token usage. **No id = your own** (no admin rights needed — it's your row); naming someone else's id is admin-only. |
| `/accounts approve <ac_id>` | admin | Grant a pending (bare) account access in one action: `normal` flow, plus `agora` when its onboarding scope is an agora scope, plus an allowlist entry. A paused account stays suspended until `resume` (the reply says so). |
| `/accounts mint <ac_id>` | admin | **DM-only.** Mint a short-lived bind code for any account. Give it to the person; they paste it as a plain message on a connected channel to link that channel to the account. |
| `/accounts flow <ac_id> <flow[,flow…]>` | admin | Set an account's flow entitlement(s) — which flow(s) handle its messages. |
| `/accounts pause\|resume <ac_id>` | admin | Central response switch for a whole person (all bound handles). `pause` = a paused account falls back to the intro flow (a "suspended" notice); `resume` = back to normal routing. Distinct from a denylist (per-platform, per-raw-handle). See [docs/accounts.md](docs/accounts.md). |
| `/bindaccount` | user | **DM-only.** Mint a bind code to link **another of your own** channels to your account — paste the code as a plain message on that other channel. |
| `/accounts apikey new <ac_id> [kinds=llm,image \| models=x,y] [label…]` | admin | **DM-only.** Mint an API key for the OpenAI-compatible API (see [docs/modelgate-api-guide.md](docs/modelgate-api-guide.md)), billed to that account. `kinds=` admits it to those lanes (`llm` / `translate` / `ocr` / `asr` / `image` — `image` admits `/v1/images/*`, not chat); `models=` pins exact entry names instead. Nothing given → `kinds=llm`. The plaintext `sk-bob-…` is shown **once**. |
| `/accounts apikey list [ac_id]` | admin | Keys as `key-id · account-id · status · policy` — for one account, or all of them. |
| `/accounts apikey revoke <key-id>` | admin | Revoke a key (the row stays, so past billing remains readable). |

There is intentionally **no `/accounts bind`** — binding happens only via a
redeemed code or self-`new` (a typed command can't bind you to an arbitrary
account).

### Model pool

| Command | Role | What it does |
|---|---|---|
| `/model` / `/model list` | admin | List the running pool's entries with state + in-flight + tags + last-error marker. |
| `/model pause <name>` | admin | Mark an entry paused in the running gateway (in-memory; restart resets). |
| `/model resume <name>` | admin | Lift the pause flag. |
| `/model reload` | admin | Re-read `models.yaml` and rebuild the entry table. |

### Agora (multi-agent)

`/agora …` is admin-only. It mirrors the `bob agora` CLI tree —
the same operations are reachable both ways. See
[docs/agora.md](docs/agora.md) for the subsystem.

| Subcommand | What it does |
|---|---|
| `/agora` | Print the agora identity THIS chat resolves to (inbox → member → employments → grants), then the usage banner. |
| `/agora overview` | One-screen snapshot: companies, members, inboxes. |
| `/agora bootstrap <company> [--founder=<name>] [--bridge-to-chat=auto\|<source>:<dm\|group>:<chat>]` | Create a starter company + founder member + a chat-bridge inbox in one shot (idempotent). Company name is positional or `--company=<name>`. |
| `/agora company {create,list,disable,enable,pause,resume}` | Manage `bob_agora_companies` rows. |
| `/agora member create --name=<name> [--model-pref=…] [--prompt-style=…]` | Create a `bob_agora_members` row. |
| `/agora hire <company> <member> <role>` | Open an active employment row, allocating an inbox. |
| `/agora terminate <member> <company>.<role>` | End an active employment row. |
| `/agora change-role <member> <company> <oldRole> <newRole>` | Change an active employment's role in place (reuses the existing inbox). |
| `/agora inbox {add-source,list,create,wire,unwire}` | Manage inboxes + per-inbox source wiring. `wire` binds the current chat (derives source + matcher from the live event); `unwire` detaches it. |
| `/agora prompt <member>` | Dump a NAMED worker's live agora context — identity / directory / playbook / authorized tools + skills — for each of its active employments. (For the CURRENT chat's identity use bare `/agora`.) |
| `/agora reload` | Re-read `permissions.yaml` + agora roles + org cache, refresh the inbox router. Stage-then-commit: a partial failure leaves the live policy untouched and reports which step rolled back. |

### Permissions

| Command | Role | What it does |
|---|---|---|
| `/permission reload` | admin | Re-read `permissions.yaml` into the live permission matrix without a restart (the file backend has no watcher). |

### Gate (rejected senders)

| Command | Role | What it does |
|---|---|---|
| `/gate allow <token>` | admin | Allowlist a rejected sender (the webui "rejected senders" panel's per-row "allow" action prefills this). Also accepts the explicit form `/gate allow <source> <user-id>`. Writes the policy file, hot-reloads, and forgets the rejected record. |

### Web UI & takeover

| Command | Role | What it does |
|---|---|---|
| `/webui` | admin | Mint a one-time web-admin management token to unlock the management dashboard. The token is delivered out-of-band through the configured admin-line — never as a chat reply (a group reply would leak it). Supersedes any prior token. |
| `/takeover` | user | **DM-only.** Mint a per-scope, takeover-only webui capability token and return it **in the private reply** — paste it into the web panel to land on the live browser this conversation is driving (clear a login wall / 2FA the headless bob is stuck at). Refused outside a 1:1 DM (a group reply would leak the token) and when there's no live browser for the scope. Contrast admin `/webui` (full management, admin-line delivery). |

### Arrangements

| Command | Role | What it does |
|---|---|---|
| `/arrangement` | admin | Show arrangement status (defined pipelines + live/parked items). |
| `/arrangement tick <dur>` | admin | Set the select cadence (e.g. `30s`, `2m`). |
| `/arrangement push <dur>` | admin | Set the push interval (e.g. `8s`, `15s`). |
| `/arrangement maxconcurrent_for_arrangement <n>` | admin | Set the per-company arrangement concurrency limit. |
| `/arrangement cancel <id>` | admin | Cancel one arrangement by id. |

### User-defined slash commands

Drop an executable into `$BOB_HOME/commands/<name>` and it appears as
`/<name>` in chat — admin-only by default, 30 s timeout,
output capped at the strictest connected source's message budget. The
file's last extension is stripped (`git-status.sh` → `/git-status`).
Use `/reload-commands` after adding or editing a script. See
[docs/usercmds.md](docs/usercmds.md) for the discovery rules, env
injection, and the `# description:` header convention.

### Removed / superseded

The trunk-rebuild branch collapsed the slash surface down to the live
set above. Commands that earlier docs listed but that are **not
registered** in the current build (do not assume they work): `/switch`,
`/title`, `/stream`, `/approve`, `/deny`, `/todos`, `/commands`,
`/quit`, `/exit`, `/language`, `/skill` (singular — use `/skills` /
`/skills reload`), `/memory`, `/learn`, `/recall`, `/credential …`,
`/ssh-test`, `/i18n reload`, `/sessions`, `/cleanup`,
`/reload-commands`, `/reload-sources`, `/source-allow`,
`/source-allow-group`, `/permission-reconcile` (now `/permission
reload`), `/db_reattach`, `/branch`, `/reset`. The agora subcommands
`roles list`, `permission-reconcile`, `rerole` (now `change-role`),
`channel`, `session`, and `dispatch` are likewise gone.

---

## Group behavior recap

- **DM**: every message goes to the LLM; one rolling session per scope.
- **Group**: the bot only responds when **@-mentioned** or when the
  message **replies to one of its messages**. Everything else is
  ignored. Reply → continue that message's topic; @-mention → new topic.
  Some platforms also gate whether the bot even *receives* non-mention
  group messages: Telegram needs privacy mode off in @BotFather (or the
  bot made a group admin); Discord deliberately runs without the
  privileged MESSAGE CONTENT intent, so it only ever sees @-mentions /
  replies / DMs — which is exactly this gate.
- **Authorization**: per-source — see the source's YAML under
  `$BOB_HOME/sources/`. The README covers setup;
  [docs/sources.md](docs/sources.md) has the per-platform fields.

---

## See also

- [README.md](README.md) — install + setup
- [docs/slash.md](docs/slash.md) — slash registry internals (design)
- [docs/usercmds.md](docs/usercmds.md) — `$BOB_HOME/commands/` user-defined slash
- [docs/agora.md](docs/agora.md) — agora multi-agent subsystem
- [docs/pipeline-session.md](docs/pipeline-session.md) — sessions, scopes, todos, render prefs
