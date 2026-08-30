package imagecreate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// The write-ahead log exists for one failure: bob dies between "the backend
// accepted the job" and "the user got the picture". ComfyUI keeps rendering
// either way — what is lost is bob's KNOWLEDGE that it owes someone an image, and
// the user is left staring at nothing, unsure whether to ask again.
//
// So the value here is not saving a render. It is being able to say something.
// Recovery delivers the image when it can and says "that one didn't make it" when
// it can't; both beat silence (docs/image-create-tool.md §9.5).
//
// Files, not a table. Records are single-process, always a handful, and live 10-45
// seconds — the same rubric that kept credentials on the filesystem
// (docs/credentials.md), and the same atomic tmp+rename idiom as the tool failure
// store in leaf/tools/70-learn.go.

const (
	// walMaxAge bounds how long a record is worth chasing. Past it the backend has
	// long since dropped the job from its in-memory history, so the only honest
	// outcome left is to tell the user and forget it.
	walMaxAge = 6 * time.Hour
	// walSweepPeriod is the Housekeeper cadence. Short on purpose: the Housekeeper
	// seeds a task's FIRST run at min(Period, 10min), and a restart-recovery that
	// waits ten minutes to speak has already lost the user.
	walSweepPeriod = 2 * time.Minute
	// walSweepBudget caps one whole pass. The Housekeeper's single worker runs every
	// module's sweep serially, so an unbounded pass here starves all of them.
	walSweepBudget = 45 * time.Second
	// walRecoverProbe caps ONE record's "is it done yet". Long enough to ride out a
	// poll interval or two, far short of a generation — a still-running job is meant
	// to fall through to the next tick, not be waited on.
	walRecoverProbe = 15 * time.Second
)

// walRecord is one in-flight generation. Scope (not Target) is stored because a
// scope is what a tool is given; contract.TargetForScope turns it back into a
// chat coordinate at recovery time.
type walRecord struct {
	PromptID string `json:"prompt_id"`
	Entry    string `json:"entry"` // pool entry that accepted the job — pinned again on recovery
	Scope    string `json:"scope"`
	Sid      string `json:"sid"`
	Style    string `json:"style"`
	Prompt   string `json:"prompt"`
	Created  int64  `json:"created_unix"`
}

// wal is the on-disk set of in-flight jobs, plus — in memory — which of them THIS
// process is still answering for.
//
// The two halves are ONE concept: a record's ownership. They live in one struct so
// the pairing cannot be got wrong — as two sibling fields on the tool, "clear the
// mark but keep the record" was a half-step that existed only in the caller's memory
// and that the compiler could not see. Here a record is written and claimed by a
// single call, and retired by a single call.
//
// Ownership is deliberately in memory: a crash empties it, which is exactly when
// those records become orphans worth recovering (§9.5).
type wal struct {
	dir string

	mu    sync.Mutex
	owned map[string]bool
}

func newWAL(home string) *wal {
	if strings.TrimSpace(home) == "" {
		return nil // no writable home → no recovery, but generation still works
	}
	return &wal{dir: filepath.Join(home, "image_create", "inflight"), owned: map[string]bool{}}
}

// claim records a job AND marks it as this process's own. Called the instant the
// backend names it and BEFORE any image can exist — that ordering is what makes
// this a write-AHEAD log.
//
// The mark is taken FIRST, never after the write: the sweep reads the directory,
// so a record that appeared without its mark could be adopted in the window
// between the two.
//
// Best-effort on the disk half: a write failure must not fail a generation that is
// already running. The cost of losing a record is losing recovery for that one job.
func (w *wal) claim(r walRecord) {
	if w == nil || r.PromptID == "" {
		return
	}
	w.mu.Lock()
	w.owned[r.PromptID] = true
	w.mu.Unlock()

	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		slog.Warn("image_create: cannot create wal dir", "err", err)
		return
	}
	final := filepath.Join(w.dir, safeName(r.PromptID)+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("image_create: cannot write wal record", "err", err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		slog.Warn("image_create: cannot commit wal record", "err", err)
		_ = os.Remove(tmp)
	}
}

// drop forgets a job for good: the picture REACHED the user, or the job is known
// to have failed. Nothing is left for the sweep to finish, and a leftover record
// would surface later as a duplicate image or a bogus "it didn't make it".
//
// Note what is NOT a drop any more: a picture generated but not yet sent. Delivery
// now happens at the end of the turn (the pictures are held for the acceptance
// gate), so "the tool call returned" no longer means "the user has it" — the record
// must outlive the call and is dropped by the delivery itself.
func (w *wal) drop(promptID string) {
	if w == nil || promptID == "" {
		return
	}
	w.mu.Lock()
	delete(w.owned, promptID)
	w.mu.Unlock()

	_ = os.Remove(filepath.Join(w.dir, safeName(promptID)+".json"))
}

// isOwned reports whether THIS process is still answering for the job — the sweep's
// orphan test, and this mark's only reader.
func (w *wal) isOwned(promptID string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.owned[promptID]
}

// list reads every pending record, reaping unreadable ones and leftover .tmp
// files (a crash between WriteFile and Rename).
func (w *wal) list() []walRecord {
	if w == nil {
		return nil
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil // no dir = nothing in flight (the common case)
	}
	var out []walRecord
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(w.dir, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") {
			_ = os.Remove(p)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r walRecord
		if json.Unmarshal(raw, &r) != nil || r.PromptID == "" {
			_ = os.Remove(p) // corrupt: nothing to chase
			continue
		}
		out = append(out, r)
	}
	return out
}

// safeName keeps a backend-supplied id from escaping the wal directory. The id is
// a server-generated uuid in practice, but it crosses a trust boundary and lands
// in a filename, so it is constrained here rather than assumed.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('.')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// sweep finishes what a previous process started. Registered on the trunk
// Housekeeper (persistent state → the shared scheduler, never a private timer).
//
// Runs through the POOL rather than talking to a backend directly: the record
// pins the entry by name, and the provider recognises a recover request and skips
// straight to polling. base_url therefore stays where it belongs — inside the
// pool — and a recovered job goes through exactly the same completion and error
// handling as a fresh one.
func (t *Tool) sweep(ctx context.Context) error {
	records := t.wal.list()
	if len(records) == 0 {
		return nil
	}
	pool := t.pool()
	gw := t.gw()
	// Nothing can be delivered or retired without both — and the gateway is exactly
	// what may still be starting (leaf/tools/00-module.go). Bail BEFORE the age check
	// so an expired record is never dropped in the one window where its "it didn't
	// make it" notice cannot be sent: silently deleting it is the failure this whole
	// mechanism exists to prevent.
	if pool == nil || gw == nil {
		return nil
	}
	slog.Info("image_create: recovering in-flight generations", "count", len(records))

	// The Housekeeper runs ONE worker over ALL modules' sweeps, serially
	// (trunk/30-housekeeping.go). Every second spent here is a second the other
	// modules' persistence sweeps do not run, so the whole pass is bounded and any
	// leftovers simply wait for the next tick — records are idempotent by design.
	ctx, cancel := context.WithTimeout(ctx, walSweepBudget)
	defer cancel()

	for _, r := range records {
		if ctx.Err() != nil {
			return nil // budget spent; the rest keep for the next tick
		}
		// A job THIS process is still waiting on is not an orphan. Its record is on
		// disk because the log is written ahead of the work, and the sweep tick lands
		// inside a generation often enough to matter (2-minute cadence, ~45-second
		// renders). Adopting it does real damage twice over: the probe queues behind
		// the very generation it is asking about — one slot per entry — and then times
		// out, and the record is dropped while the picture is still coming, which is
		// precisely the window the log exists to cover. Observed in production
		//: the user got the picture AND "刚才那张图没画成".
		if t.wal.isOwned(r.PromptID) {
			continue
		}
		age := clock.Now().Sub(time.Unix(r.Created, 0))
		if age > walMaxAge {
			t.notify(ctx, gw, r, "刚才那张图没能画完（等太久了）。要我重画一张吗？")
			t.wal.drop(r.PromptID)
			continue
		}
		img, err := t.recoverOne(ctx, pool, r)
		if err != nil {
			// Inconclusive → say nothing, try again next tick. Only a real backend
			// error is terminal (the backend lost the job — its history is in memory
			// and it restarts too); "we could not find out" must never be reported to
			// a user as "it didn't make it".
			if inconclusive(err) && age <= walMaxAge {
				slog.Debug("image_create: could not reach the backend for an in-flight job", "prompt_id", r.PromptID, "err", err)
				continue
			}
			slog.Warn("image_create: giving up on in-flight generation", "prompt_id", r.PromptID, "err", err)
			t.notify(ctx, gw, r, "刚才那张图没画成。要我重画一张吗？")
			t.wal.drop(r.PromptID)
			continue
		}
		if err := t.deliverRecovered(ctx, gw, r, img); err != nil {
			slog.Warn("image_create: recovered image but could not deliver", "prompt_id", r.PromptID, "err", err)
			continue // keep the record; delivery may work next tick
		}
		t.wal.drop(r.PromptID)
	}
	return nil
}

// recoverOne asks the pinned entry to finish a job it already accepted.
//
// PROBE, not a wait: a job that is genuinely still rendering must return quickly
// so the sweep can move on and try again in two minutes. Blocking here for the
// engine's full generation timeout would hold the shared Housekeeper worker for
// minutes per record — and a backend that restarted and lost its in-memory
// history never answers at all, which would burn the budget on every tick until
// walMaxAge.
func (t *Tool) recoverOne(ctx context.Context, pool contract.ModelPool, r walRecord) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, walRecoverProbe)
	defer cancel()
	body, err := json.Marshal(map[string]any{"recover": r.PromptID, "style": r.Style})
	if err != nil {
		return nil, err
	}
	resp, err := pool.Chat(ctx, contract.ModelRequest{
		Kind:        contract.KindImage,
		PinnedEntry: r.Entry,
	}, []contract.Message{{Role: "user", Content: string(body)}})
	if err != nil {
		return nil, err
	}
	var reply struct {
		ImageB64 string `json:"image_b64"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &reply); err != nil || reply.ImageB64 == "" {
		return nil, fmt.Errorf("image_create: recovered reply carried no image")
	}
	return base64.StdEncoding.DecodeString(reply.ImageB64)
}

// inconclusive reports whether an error means "we did not find out" rather than
// "this will never finish". Only a backend verdict is terminal — a job that
// errored there errors identically forever.
//
// Two shapes count as not-finding-out, and the second one is the one that bit us:
//
//   - the provider polled the job and it was still rendering;
//   - the probe's own deadline fired. That can happen WITHOUT the backend ever
//     being asked — an image entry runs one job at a time, so a probe issued while
//     any generation holds the slot waits in the pool's queue and expires there.
//     Reading that as "the backend lost it" turned a running job into a "没画成"
//     notice, and dropped the record that was the only way to recover it.
//
// Both are bounded by walMaxAge at the call site, so nothing retries forever.
func inconclusive(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// The pool and the provider both format ctx failures into messages rather than
	// wrapping them, so the string is load-bearing here, not a shortcut.
	msg := err.Error()
	return strings.Contains(msg, "did not finish in time") ||
		strings.Contains(msg, context.DeadlineExceeded.Error()) ||
		strings.Contains(msg, context.Canceled.Error())
}

// deliverRecovered writes the bytes somewhere real and sends them to the chat the
// job came from. There is no turn and no space here, so the file lands in bob's
// own scratch and is removed once the platform has it.
//
// Sends through the SOURCE (as a picture where the platform has a picture send —
// contract.DeliverPicture), NOT through a Sink. A Sink is a turn's rendering
// machinery: on a channel that can type, NewSink starts a background loop that
// only stops on Finish or ctx end (leaf/stoma/sources/streamsink/10-sink.go), and
// this ctx is the Housekeeper's — process-lifetime. A recovery has no turn to
// Finish, so building one would leak a goroutine per recovered image and leave
// that chat showing "typing…" forever. A one-off send is exactly what Source
// offers for asynchronous feedback.
func (t *Tool) deliverRecovered(ctx context.Context, gw contract.Gateway, r walRecord, img []byte) error {
	target, src, err := t.route(gw, r)
	if err != nil {
		return err
	}
	dir := filepath.Join(t.home, "image_create", "recovered")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, safeName(r.PromptID)+".png")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		return err
	}
	defer os.Remove(path)
	return contract.DeliverPicture(ctx, src, target, path,
		"刚才那张图（"+r.Style+"）画好了：\n"+truncateRunes(r.Prompt, 200))
}

// route resolves a record's stored scope to a chat and the source that serves it.
func (t *Tool) route(gw contract.Gateway, r walRecord) (contract.Target, contract.Source, error) {
	target, ok := contract.TargetForScope(r.Scope)
	if !ok {
		return contract.Target{}, nil, fmt.Errorf("image_create: scope %q does not name a deliverable chat", r.Scope)
	}
	src := gw.SourceByName(target.Source)
	if src == nil {
		return contract.Target{}, nil, fmt.Errorf("image_create: source %q is not registered", target.Source)
	}
	return target, src, nil
}

// notify tells the chat a job will never arrive. Best-effort by nature — if even
// this cannot be delivered, the record is dropped anyway rather than retried
// forever against a chat that may no longer exist.
func (t *Tool) notify(ctx context.Context, gw contract.Gateway, r walRecord, msg string) {
	if gw == nil {
		return
	}
	target, src, err := t.route(gw, r)
	if err != nil {
		slog.Warn("image_create: nowhere to send the failure notice", "prompt_id", r.PromptID, "err", err)
		return
	}
	if err := src.Send(ctx, target, msg); err != nil {
		slog.Warn("image_create: could not deliver failure notice", "prompt_id", r.PromptID, "err", err)
	}
}

func truncateRunes(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
