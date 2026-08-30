package files

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// filesSweepPeriod is how often the attachment sandbox is swept. Retention is
// coarse (whole days), so a few-hour cadence is plenty.
const filesSweepPeriod = 6 * time.Hour

// Sweeper is the file-store's housekeeping module: it registers a periodic sweep
// (age-prune + optional total-byte cap) with the trunk Housekeeper so captured
// inbound attachments don't grow on disk without bound. The Store itself stays a
// plain struct shared with the sources (built before the trunk); this module only
// owns the maintenance schedule.
//
// retentionDays<=0 disables the age sweep; maxBytes<=0 disables the cap. Both off
// → the module starts and registers nothing (a no-op, not an error).
type Sweeper struct {
	store         *Store
	retentionDays int
	maxBytes      int64
	spacesRoot    string       // $BOB_HOME/spaces — age-prune per space (see SweepSpaces)
	state         atomic.Int32 // trunk.State — atomic so Health() can be polled cross-goroutine
}

// NewSweeper builds the file-store housekeeping module over an existing Store.
// spacesRoot is $BOB_HOME/spaces — the per-space prune (SweepSpaces) reuses
// retentionDays; "" disables it.
func NewSweeper(store *Store, retentionDays int, maxBytes int64, spacesRoot string) *Sweeper {
	return &Sweeper{store: store, retentionDays: retentionDays, maxBytes: maxBytes, spacesRoot: spacesRoot}
}

func (s *Sweeper) Name() string             { return "files-sweeper" }
func (s *Sweeper) Provides() []reflect.Type { return nil }
func (s *Sweeper) Needs() []reflect.Type    { return nil }
func (s *Sweeper) Optional() bool           { return true }
func (s *Sweeper) Health() trunk.State      { return trunk.State(s.state.Load()) }

func (s *Sweeper) Start(_ context.Context, reg *trunk.Registry) error {
	s.state.Store(int32(trunk.StateReady))
	if s.store == nil || (s.retentionDays <= 0 && s.maxBytes <= 0) {
		return nil // nothing to sweep / nothing configured
	}
	hk, ok := trunk.TryRequire[trunk.Housekeeper](reg)
	if !ok {
		return nil // no housekeeper (minimal config) — sweep just doesn't run
	}
	hk.Register(trunk.Task{
		Name:   "files-sweep",
		Period: filesSweepPeriod,
		Run: func(ctx context.Context) error {
			if s.retentionDays > 0 {
				if n := s.store.SweepOlderThan(s.retentionDays); n > 0 {
					slog.Info("files: swept attachments past retention", "removed", n, "days", s.retentionDays)
				}
			}
			if s.maxBytes > 0 {
				if n, remaining := s.store.EnforceTotalCap(s.maxBytes); n > 0 {
					slog.Info("files: enforced sandbox total cap", "removed", n, "remaining_bytes", remaining)
				}
			}
			if s.retentionDays > 0 && s.spacesRoot != "" {
				if n := SweepSpaces(ctx, s.spacesRoot, s.retentionDays); n > 0 {
					slog.Info("files: swept space files past retention", "removed", n, "days", s.retentionDays)
				}
			}
			return nil
		},
	})
	return nil
}

func (s *Sweeper) Stop(context.Context) error {
	s.state.Store(int32(trunk.StateStopped))
	return nil
}

// companySpacePrefix marks a file_storage SHARED space on disk. Company shares — the
// ONLY durable, model-writable spaces (reachable only via BoundChannels.Allowed, which is
// company: shares only; every other write resolves to the ephemeral DefSpace) — are
// minted as "company:<id>" (leaf/agora/40-impl.go::memberSpaces) and land as "company_<id>"
// after sanitizeSpace folds ':' → '_'. So this prefix has NO durable-but-swept gap: nothing
// persistent lives in a non-company_ space.
//
// The reverse is a soft convention, not an invariant: a per-scope WORKING space is
// "<source>_<kind>_<id>" and a source operator-named "company*" would fold to a
// "company_"-prefixed scratch dir that this test then over-PROTECTS (only its inbox/ is
// pruned, root scratch grows unbounded). That is a wrong-KEEP (wasted disk), never a
// wrong-DELETE — acceptable, since sources are normally platform names.
const companySpacePrefix = "company_"

// SweepSpaces age-prunes disk under <spacesRoot>/<space>/ older than retentionDays, with
// two policies told apart by space TYPE (the dir-name prefix; see docs/spaces.md §8):
//
//   - company_* (file_storage SHARED stores): PERSISTENT collaboration data whose
//     lifecycle outlives any session — prune ONLY the ephemeral inbox/ subdir, never the
//     shared files at / under the space root.
//   - everything else (per-scope WORKING spaces: <source>_<kind>_<id>,
//     member_<name>_inbox_<id>, default, ...): a session scratch area — recursively prune
//     the WHOLE tree by mtime. Deliverables already left via deliver_file (they live in
//     the chat); anything meant to persist belongs in a company_ shared space, and
//     regenerable material (skill_view scripts, browser screenshots) re-materializes on
//     next use.
//
// Touches only regular files, never follows symlinks, and leaves each space's ROOT dir in
// place (recreated lazily on next write). retentionDays<=0 / a missing root → a no-op.
// Returns the count removed.
func SweepSpaces(ctx context.Context, spacesRoot string, retentionDays int) int {
	if retentionDays <= 0 || spacesRoot == "" {
		return 0
	}
	spaces, err := os.ReadDir(spacesRoot)
	if err != nil {
		return 0
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	removed := 0
	for _, sp := range spaces {
		if ctx.Err() != nil {
			return removed // housekeeper shutting down — stop cleanly
		}
		if !sp.IsDir() {
			continue
		}
		dir := filepath.Join(spacesRoot, sp.Name())
		if strings.HasPrefix(sp.Name(), companySpacePrefix) {
			removed += sweepSpaceInbox(dir, cutoff) // shared store: protect the root, prune inbox/ only
			continue
		}
		removed += sweepSpaceTree(ctx, dir, cutoff) // working space: prune the whole tree
	}
	return removed
}

// sweepSpaceInbox prunes one space's inbox and returns the count removed. The space
// dir is model-writable (its exec/fs channels root here), so a model could swap
// inbox with a symlink between the Lstat check and the later OpenRoot/reads/removes,
// making the sweep unlink files OUTSIDE the space. os.OpenRoot pins real directory fds,
// but it only refuses symlinks that ESCAPE the root — an IN-root link (inbox -> .) is
// followed, which would open the space ROOT and let the prune loop unlink top-level
// files there. So we pin identity across the Lstat→OpenRoot gap: the opened inbox dir
// must be os.SameFile as the directory we Lstat'd, otherwise we bail before pruning.
func sweepSpaceInbox(spaceDir string, cutoff time.Time) int {
	spaceRoot, err := os.OpenRoot(spaceDir)
	if err != nil {
		return 0
	}
	defer spaceRoot.Close()
	// Refuse a symlinked inbox outright — even one pointing within the space; the
	// sweep must only ever touch the real inbox subdir, never the space root.
	li, err := spaceRoot.Lstat(contract.InboxSubdir)
	if err != nil || li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
		return 0 // no inbox in this space (or it's a link)
	}
	inbox, err := spaceRoot.OpenRoot(contract.InboxSubdir)
	if err != nil {
		return 0
	}
	defer inbox.Close()
	dir, err := inbox.Open(".")
	if err != nil {
		return 0
	}
	// Close the Lstat→OpenRoot TOCTOU window: os.Root follows an in-root symlink
	// (inbox -> .), so confirm the dir we actually opened is the SAME inode we
	// Lstat'd above before unlinking anything through it.
	if fi, serr := dir.Stat(); serr != nil || !os.SameFile(li, fi) {
		dir.Close()
		return 0
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return 0
	}
	removed := 0
	for _, f := range entries {
		// Only age-prune REGULAR files — never a dir or a symlink (which would
		// delete the link's target, outside the space).
		if !f.Type().IsRegular() {
			continue
		}
		info, err := f.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := inbox.Remove(f.Name()); err == nil {
			removed++
		}
	}
	return removed
}

// sweepSpaceTree recursively age-prunes one WORKING space's whole tree. The space dir is
// model-writable (its fs/exec channels root here), so the walk pins directory fds with
// os.OpenRoot — a path-string walk would let a dir be swapped for a symlink between the
// check and the remove, unlinking files OUTSIDE the space (same TOCTOU guard as
// sweepSpaceInbox). os.Root refuses to traverse symlinked path components, so resolving
// every entry against the ONE pinned root fd is safe without re-pinning subdirs. Mirrors
// leaf/warrant/45-exechome-sweep.go::pruneDirOld — duplicated because heartwood is the
// base layer and cannot import leaf. Returns the count removed.
func sweepSpaceTree(ctx context.Context, spaceDir string, cutoff time.Time) int {
	root, err := os.OpenRoot(spaceDir)
	if err != nil {
		return 0 // no such space (or unreadable) — nothing to sweep
	}
	defer root.Close()
	n, _ := pruneDirOld(ctx, root, ".", cutoff)
	return n
}

// pruneDirOld age-prunes regular files under dir (relative to the pinned root), recurses
// into real subdirs, then removes a subdir left empty ONLY when its own mtime aged out (so
// a just-created dir mid-write is never yanked). Symlinks / special files are left alone —
// removing a symlink is pointless and following one is the escape this walk prevents. Bails
// on ctx cancellation (housekeeper shutdown) like the exec-home walk it mirrors. Returns
// (removed, dirEndedEmpty).
func pruneDirOld(ctx context.Context, root *os.Root, dir string, cutoff time.Time) (int, bool) {
	d, err := root.Open(dir)
	if err != nil {
		return 0, false
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return 0, false
	}
	removed, empty := 0, true
	for _, e := range entries {
		if ctx.Err() != nil {
			return removed, false
		}
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			n, subEmpty := pruneDirOld(ctx, root, p, cutoff)
			removed += n
			if subEmpty {
				if fi, lerr := root.Lstat(p); lerr == nil && fi.ModTime().Before(cutoff) && root.Remove(p) == nil {
					removed++
					continue
				}
			}
			empty = false
		case e.Type().IsRegular():
			if fi, lerr := root.Lstat(p); lerr == nil && fi.ModTime().Before(cutoff) && root.Remove(p) == nil {
				removed++
				continue
			}
			empty = false
		default:
			empty = false // symlink / device / socket — left alone
		}
	}
	return removed, empty
}

var _ trunk.Module = (*Sweeper)(nil)
