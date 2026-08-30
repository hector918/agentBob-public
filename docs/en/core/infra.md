# infra: entrypoint and process facilities

The assembly point (`cmd/`), config loading (`config/`), the string catalog (`i18n/`), logging (`logging/`), migrations (`migrate/`), and the guards that weld the architecture rules into tests (`arch/`).

## Where it sits

None of these six packages is a trunk module. They do not appear in the module connection graph — no Provides, no Needs, no place in the topological sort. They fall into two groups:

- **Process facilities** (`config` / `i18n` / `logging` / `migrate`): like [contract](contract.md), [trunk](trunk.md) and [heartwood](heartwood.md), any layer may import them directly. All are stateless or hold a single process-wide instance, and none carries business knowledge.
- **Assembly and guards** (`cmd` / `arch`): `cmd/bob` is the only place that knows which modules exist; `arch` contains tests and no product code.

The dependency direction is one-way: facility packages import no leaf or flow, and while `cmd/bob` imports every module, nothing imports `cmd/bob`.

## What it is for

Together they answer one question: **what a process needs in order to run, beyond the business modules themselves.** The answer is deliberately scattered — no central god-config, no central schema, no single "register all strings" initialiser. Each is a thin mechanism, with the content owned by whichever module uses it.

## Internals

### cmd — the single assembly point

`cmd/bob/main.go` does exactly five things: read the environment, install logging and the string catalog, construct the trunk, `Register` each module in order, then start and block until a signal.

The important property is that **the registration block is the authoritative inventory**. The package doc says so explicitly: the gateway→source→session→turn sketch is only a sketch, and the never-stale list of modules is the `t.Register` block below it. That is not just discipline — `arch/40-registration_test.go` parses `main.go` with `go/ast`, extracts the fully-qualified constructor of every `t.Register(...)` argument, and compares it against a fixture, failing on any drift in the set **or the order**. Order is locked because it is the lifecycle sequencer's FIFO tiebreak: among nodes that tie in the topological sort, registration order genuinely decides who starts first.

A handful of registrations carry ordering comments, and they all have the same shape: one module reads, during its own `Start`, something another module has already populated (the skill catalog must be in place before the authorization module; the panel registry before the modules that describe themselves to it). Such a start-time read is a soft edge invisible to the module graph, so it is held jointly by registration order and the soft-edge ledger.

Configuration is entirely environment-driven, and `main.go`'s package doc enumerates the variables. An integer variable that fails to parse falls back to its default *with a warning* — for a knob like a disk cap, a silent fallback would fail in the open direction.

After start, the process blocks on a signal; on receiving one it **restores the default signal disposition before running `Stop`**, so a second interrupt during a hung shutdown actually terminates the process instead of being swallowed by the still-registered notify channel.

### config — a loader for module-owned configuration

There is no central god-config. Each module defines its own `Config` struct, and heavy modules own their own yaml file; `config` supplies only the generic mechanism: load a yaml file over a defaults value.

The contract is that **files carry overrides only**. A missing file is not an error — it returns the defaults unchanged; a field absent from a present file keeps its default. With sane defaults in code, the on-disk config stays small regardless of how it is split.

One granularity difference must be known: scalar and struct fields override field by field and maps merge key by key, but **a slice present in the file replaces the default slice wholesale** rather than appending. A module with a non-empty default slice must therefore list the full set in its file, not just its additions.

The implementation also imposes one constraint on callers: `cfg := defaults` is a shallow copy, and the yaml decoder writes into an existing non-nil map **in place** — so defaults must not contain pre-populated map fields, or the file's keys would be merged into a default map shared across calls.

### i18n — user-facing strings

Every user-facing reply (slash-command output, the queue-overflow notice, the restart-missed banner, and so on) resolves through one key-addressed table. The base table is JSON embedded into the binary at compile time; an operator can overlay it under `$BOB_HOME` **per key**, changing only the strings they care about without copying the whole catalog, plus a second overlay pinning a language variant per chat.

The public API is four functions (`T` / `Detect` / `Override` / `Reload`), all delegating to one live catalog held in an `atomic.Pointer`, so readers never block during a reload.

Three design commitments are worth naming:

- **The fallback chain**: variant (`zh-funky`) → bare language (`zh`) → `default`, with `en` as an alias for `default`. A key missing throughout the chain **returns the key itself** — a missing string is visible breakage in logs and in chat, not a silent blank.
- **Degradation does not spread.** A broken optional overlay costs only that overlay. `Load` does not early-return: it records the error, skips the broken overlay's contributions, falls through to assemble the rest, and returns the fully assembled catalog alongside the joined error. Only a broken *embedded* catalog is a genuine build defect.
- **A formatting backstop.** A string with a literal `%`, a stray verb, or an argument-count mismatch makes `fmt` embed its error markers in the output. Detecting such a marker falls back to the unformatted string — unless an argument **itself** contains the marker (user- or provider-controlled data), in which case the marker proves nothing and the formatted output stands.

`Detect` is a deliberately minimal classifier: count Han runes against Latin letters, return `zh` when Han is at least Latin, and return an empty string when there is no script signal at all. The empty string is meaningful — it lets the caller (the [inbound flow](../flows.md)) continue down its cascade: an admin pin → this message's detection → the sender's remembered language → default. i18n itself holds **no** per-chat or per-sender state; the language memory lives in the accounts module, and the cascade lives in the flow layer.

### logging — process logs

`logging.Setup` installs the process-wide slog logger, targeting stdout, a file, or both. The default is **both**: a rotating file on disk plus stderr — container logs alone vanish when a container is recreated, while a file-only target breaks the conventional way of tailing a container.

A misspelled enum value (`wran`, `fle`) falls back to the default, but *loudly*: the warning is deferred until the new logger is installed, so it lands on the configured sink instead of vanishing with the pre-setup default.

Every record's timestamp is pinned to UTC whatever the container's timezone is. The reason is operational: the process writes more than one log file, and the others are already hard-UTC. Once a deployment sets a timezone, correlating one turn across those files — the first move when something breaks — would mean mentally adding the local offset to exactly one of them. RFC3339 ends every stamp in `Z`, so the scale is stated on each line instead of being something to remember. Panels are the deliberate opposite (`clock.Stamp` renders local — see [heartwood.md](heartwood.md)).

The rotating writer's central invariant is **never drop a line**, and the invariant is structural: `rotate` has no error return at all, because every failure path recovers in place.

- Rename fails (directory permissions, a full disk): **keep** the current, still-writable descriptor, arm a backoff window during which rotation is not retried, and go on appending. The file may exceed its soft size cap inside that window — which violates the size bound but never loses a line. Bubbling the error up instead would make the writer drop this line, so a persistent rename failure would black-hole the entire file log, the worst outcome on a deployment where nobody reads stderr.
- Rename succeeds but reopen fails: fall back to stderr permanently, and **the line that triggered the rotation still lands** on the fallback sink.
- Neither recovery path may log via slog: `rotate` runs while the writer holds its mutex, and when this writer *is* the installed slog sink, a `slog.Error` would route back into its own `Write` and re-take a non-reentrant lock — a self-deadlock. So they write to stderr directly.

Alongside it, `logging/ringfile` is a size-bounded JSONL append log shared by two audit streams (the authorization decision log and the credential-broker call log). It owns only the append/trim/cached-descriptor machinery; each caller marshals its own record format. Its error contract is **warn once, then self-disable**: an open or trim failure permanently short-circuits later writes, never returning an error to the caller and never blocking the hot path — a failing audit log must not take down the path it audits. Trim has one hard ordering rule: the cached descriptor must be closed before the rename, or the next write would go to the unlinked-but-still-open pre-trim inode.

### migrate — per-module schema evolution

There is no central schema. Each module owns its ordered migration steps and calls `migrate.Run` in its `Start`, once it has resolved `contract.DB`. One shared version table records the applied version **per module**, and only pending steps run. Cross-module ordering needs no separate expression — it falls out of the trunk's start order: each module migrates its own tables when it starts, and by then the modules it depends on have started.

Two guarantees:

**Per-step atomicity.** A step's DDL and its version bump run in one transaction (Postgres DDL is transactional), so the pair is all or nothing. A crash mid-step rolls the DDL back and leaves the recorded version untouched, and a re-run re-applies that step from a clean slate. Steps therefore **do not need to be individually idempotent** — a real reduction in what each step author must reason about.

**Concurrent boots serialise.** When two processes start against one database, the version row itself does the serialising: the bump is a compare-and-swap (`UPDATE ... WHERE version = <the value this process read>`). The loser's CAS matches zero rows, so **its whole transaction — including that step's DDL — rolls back**; it then re-reads the version and skips what the winner applied. No advisory lock is used: that would be backend-specific SQL, while the CAS gives the same no-double-apply guarantee on either backend.

Steps are validated purely before the database is touched: versions must start at 1 (a `<= 0` version would sort to the front and be silently skipped by the apply loop, so it is rejected outright) and must not repeat.

### arch — the rules, welded into tests

`arch/` contains no product code. It is the machine guard for every architecture rule above, and it is deliberately strict: a red test here is not a fault but a review gate — a new inter-module connection must be seen and approved here before it ships.

Six guards:

| Guard | What it locks | 
|---|---|
| `wantGraph` | The approved **hard** connection graph (every module's `Provides` / `Needs`). Any module gaining or losing an edge, or being added or removed, fails |
| Import boundary | No package under `leaf/` or `flow/` may import a **different** module directly (sub-packages of the same module are fine). Such a connection bypasses the trunk and is invisible to `wantGraph`, so it is forbidden outright — no approval ledger |
| `wantOptional` | The ledger of `TryRequire` **soft** edges. A soft edge is not a declared Need, so reflection cannot see it — the guard scans every module's `TryRequire` call sites instead |
| `wantProvides` | The provider-side ledger, scanned from real `Provide` call sites. `wantGraph` sees only **static** declarations, so a module publishing a capability conditionally inside `Start` is a blind spot for it |
| `heartwoodAllowed` | The foundation-layer admission list (see [heartwood.md](heartwood.md)) |
| scrub parity | The hand-copied redaction fork in a separate Go module must stay byte-identical to the canonical copy |

How the guards are written also carries a commitment. The registration guard **parses the real source** rather than diffing a hand-written list, and it treats any registration shape it cannot resolve as a fatal error — the scan must fail loud, not fail open, or a new registration style would quietly escape the sync gate. The soft-edge ledger makes the point most sharply: it used to be a hand-written comment list, and it rotted silently (five edges were unlisted by the time it was checked), which is why it became machine-checked.

## Design rationale

**Why there is no central config, schema, or string registry.** All three use the same move: the mechanism lives in a thin shared package, and **ownership of the content stays in the module**. A module carries its own defaults and loads its own file; carries its own migration steps and runs them in its own `Start`; carries its own string keys and resolves them at the render site. This is what makes "modules never import each other" hold in practice — a central config struct or a central schema file would immediately become a shared type every module must import, letting a forbidden coupling in through the back door.

**Why the authority is parsed source, not documentation.** The module inventory, the connection graph, the soft edges and the provider edges are all scanned out of real source and compared against a fixture. Any list that depends on a human to update it eventually rots — the soft-edge ledger proved that in practice. Recorded in a test, this document instead stays true by construction, because a mismatch turns the build red.
