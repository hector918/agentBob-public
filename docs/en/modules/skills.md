# skills

The skill catalog: a **dumb set** of installed skills that only loads and reads them. Who may see which, and when one is used, is somebody else's job.

A skill is fundamentally **text plus metadata** — an instruction body the model reads, plus a few typed switches enforced mechanically upstream. Only the minority that ship scripts carry files at all.

---

## Place in the architecture

**Provides**

| Capability | Consumers |
|---|---|
| `contract.SkillCatalog` | the flows (which read the full set), [warrant](warrant.md) (which reconciles grant rows against it) |
| `contract.SkillFailureSink` | the agora flow (records a snapshot of a failed turn) |

**Needs**

- `contract.PanelRegistry` — see [webui](webui.md); purely to hang a panel at `Start`. It is a hard Need precisely because webui is non-optional, so it cannot drag an optional module into the "provider absent → whole module skipped" trap.

**Soft edge:** `contract.LearnRegistry` (see [learn](small-modules.md#learn)) — absent simply means skills never learn; everything else is unaffected.

The module itself is optional: without it the flows still run, the model just has no skills to draw on. It is the symmetric twin of [tools](tools.md) — one side actions that can be executed, the other side instructions that can be followed; both dumb catalogs, both projected per identity by warrant.

---

## What it does

**Two sources.** Built-in skills are compiled into the binary (embed); external skills are directories an admin drops under `skills/external/` in the data home. Internal is scanned first, then external, and **external wins a name clash** — so a deployment can replace a built-in skill without rebuilding the binary.

**One skill = one directory.** It must contain a `SKILL.md`: a leading YAML frontmatter block followed by the body — the text the model works from. A few conventional files may sit beside it:

| File | Role | Who reads it |
|---|---|---|
| `SKILL.md` | frontmatter plus the instruction body | the model (index in the prompt, body on demand) |
| `RUBRIC.md` | grading criteria applied before delivery | the turn's judging gate — **never the model** |
| `INSIGHT.md` | accumulated learned notes | appended to the body the model receives |
| anything else | scripts, templates | materialised into the session workspace and executed |

**The index lives in the prompt; the body is pulled on demand.** The system prompt carries only the **index** — name, description, recall cues. The model decides which skill applies and fetches its body through the `skill_view` tool. That means skills are **always manually triggered**: there is no auto-activation mode switch anywhere; whether to use one is the model's decision within the turn.

The frontmatter's **recall cues** are a field designed specifically for that index. They are not a keyword pile but three kinds of thing: anchors (jargon, proper nouns, command names — language-agnostic), discriminators (what sets *this* skill apart from its same-domain siblings), and at most a few generic domain words. The goal is narrow: raise the hit rate of "task → remember this skill" without the model having to read the body first. The field tolerates both shapes people naturally hand-write it in (a list, or one comma-separated line), because the cost of writing it wrong is not an error but the skill **silently vanishing from the index** — precisely the failure it exists to fight.

**Auxiliary files are materialised.** When a script-bearing skill is read, its files are written into that turn's session workspace, and the model runs them from a terminal. Three reserved files never reach the workspace: `SKILL.md` (its body is already in the prompt), `INSIGHT.md` (already appended to the body), and `RUBRIC.md` — handing the grading criteria to the model being graded is handing it the answer key.

**Grading criteria.** A skill may ship a `RUBRIC.md`. When such a skill has been read during a turn, the turn core mechanically judges the final product against those criteria before delivering; a failure means redo. The catalog loads the file **verbatim** and never summarises it.

**Learned notes.** Every skill is exposed to the learning engine as one optimisable target: its current text is the notes it has accumulated, and those notes are appended to the body the model receives. They persist as `INSIGHT.md` and are read back on restart.

**Failure snapshots.** When an agora member's turn ends degraded, the flow records a medium-grain trajectory snapshot for each skill that was read in that turn. These snapshots are the failure signal for the learning engine, so distillation reflects on a real trajectory rather than a thin label.

---

## Internal structure

### The catalog and its scan (`10-registry.go`)

`FileSkills` implements `contract.SkillCatalog`: a table keyed by name, plus a table of learned notes keyed by `(origin, name)`.

The bar for loading is three checks — a size cap, parseable frontmatter, and **the frontmatter name must equal the directory name**. Failing any of them means warn-and-skip; that is the single isolation channel in this package, so one bad skill can never fail the whole load.

The notes table is keyed by `(origin, name)` rather than the bare name on purpose: what a built-in skill has learned must never latch onto a later, coincidentally same-named external skill, or vice versa.

**Symlinks are never followed.** A plain read would follow one and ingest a file from outside the skill tree — a credential, say — as prompt text. The loader and the auxiliary-file walk share one guard.

### Reliability posture

Three cases are kept distinct here, and the distinction is the point:

| Situation | Behaviour |
|---|---|
| The external directory **does not exist** | Not an error — there are simply no external skills |
| The external scan hits a **read error** (mount race, permission blip) | **Fail closed**: keep the current state untouched and report. An I/O hiccup must never be read as "those skills were removed", which would prune their accumulated notes along with them |
| Still failing at boot after retries | **Fall open** to built-ins only, and mark the catalog `Degraded` |

That `Degraded` flag exists for downstream consumers: it declares "this `List()` is known-incomplete". warrant checks it before reconciling `skill:use:<name>` grant rows — otherwise a transient mount blip would silently prune permissions an admin genuinely granted.

A **successful** rescan, conversely, does perform the removal-side cleanup: a skill that disappeared takes its notes and failure snapshots with it. One more distinction sits inside that: a skill still **present on disk but skipped as invalid** (mid-edit broken frontmatter) does not count as removed. Otherwise saving a half-finished edit once would delete everything it had ever learned — irrecoverably.

### The learning surface (`20-learn.go`)

Two things. First, each skill is wrapped as an optimisable target: read the current notes, accept a rewritten version, persist it on the way through. Second, a ring store of failure snapshots on disk — one directory per skill, one file per snapshot, oldest dropped past a cap, consumed ones swept on the next read.

Reading that store is **read-with-delete**, which is why the implementation is explicitly marked "the learning engine is the sole caller": a second consumer would destroy evidence that had not been distilled yet.

The same small ring-store mechanism exists as a byte-identical copy inside tools and agora. That is deliberate: the architecture rules forbid one leaf importing another, and `contract` is the interface seam and holds no filesystem mechanism — so this bit is **mirrored** rather than shared, at the cost of having to diff all copies whenever one is touched.

### Auxiliary files (`30-files.go`)

Walks the embed or the disk depending on origin, with per-file and total size caps, reserved files excluded and symlinks refused. Reserved-file matching is not exact-only: a crash-orphaned temp file, an editor backup, a swap file — each is a **variant** of a reserved name and must be excluded just the same, or the grading criteria ship out inside a `.swp`.

### The panel (`90-panel.go`)

An admin-only panel listing the loaded skills with their origin, whether they carry grading criteria, and whether they have learned anything; each row has an edit button that opens that skill's notes inline. Editing reuses **the same write path** as a distilled rewrite, so a hand edit and an automated one never diverge; saving an empty body clears the notes.

### Subpackage: `builtin/`

The built-in skills *are* this subdirectory, embedded whole at build time. One subdirectory per skill, structured **identically** to an external one — the same `SKILL.md`, the same optional `RUBRIC.md`, the same script directory. The loader differs in exactly one place: which filesystem it reads bytes from.

The set that ships with the binary spans a few directions:

| Direction | Shape |
|---|---|
| General writing, summarisation | pure text; output structure and what to cut |
| Academic search | ships scripts; standard library only, no extra dependencies and no keys |
| Remote operations | pure text; collapses "which machine" down to one space name |
| Corpus import | pure text; companion to an import tool |
| Commerce / content platform operation | pure text; companion to their respective request tools |

The last two are the typical shape — a companion document for a tool: the tool handles the request and its authentication, the skill tells the model which endpoint to hit, how to fill the fields, and when **not** to reach for it. Almost all of them carry a "do NOT use for …" line in the frontmatter, fencing same-domain siblings off from each other; that is exactly the job the "discriminator" class of recall cues exists to do.

Built-in skills have one structural awkwardness: they own no directory on disk, yet learned notes need somewhere writable to land. The resolution is an origin-keyed mirror directory under the data home where a built-in's notes are written.

---

## Design rationale

**A dumb catalog; projection lives elsewhere.** The catalog answers only "what is installed". Which of them an identity may use is projected by [warrant](warrant.md); when they enter the prompt is decided by the flow. Because the prompt index and `skill_view` both read **the same projection**, visibility and usability share one source — you never get a skill listed in the index that refuses on invocation.

**Skills are always manually triggered.** A per-skill mode switch (auto / semi / manual) was designed and then cut: a mode is policy, and policy belongs to the flow, not to the catalog. What remains is one shape — index in the prompt, body on demand — with the judgement left entirely inside the model's reasoning for that turn.

**Distinguish "couldn't read" from "isn't there".** The failure mode this module is most exposed to is mistaking a transient I/O fault for a deletion. Most of the catalog's complexity sits on that one distinction, because "removed" is a **destructive** verdict: it takes notes, snapshots, and downstream grant rows with it.

**Grading criteria never enter the workspace.** This is not an access-control concern but an evaluation-validity one: a rubric the graded party can read is no longer a rubric. So it is hard-excluded from the materialisation path, filename variants included.

**Symmetry with tools is deliberate.** The catalog shape, the projection, the learning surface, the panel — each corresponds one-to-one with [tools](tools.md). The cost is a few small mechanisms maintained as mirrored copies; the payoff is that "a tool" and "a skill" can be reasoned about with one mental model in the flow and authorization layers — one grants a capability, the other grants an instruction, and both derive visibility from the same projection.

---

See also: [Architecture overview](../architecture.md) · [tools](tools.md) · [warrant](warrant.md) · [learn](small-modules.md#learn) · [webui](webui.md)
