package compose

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentbob/contract"
)

// defaultReadCap bounds Read when the caller passes max<=0.
const defaultReadCap = 32 << 20

// turnAttachments is the flow's contract.AttachmentSet over THIS turn's batch, with a
// space-inbox fallback to an earlier turn's file. It binds the turn's ChannelOpener so
// Read and the inbox scan go through the authorized FileChannel. A session-wide variant
// (over persisted history) will implement the same interface later, swapping in behind
// the tools with no tool change.
type turnAttachments struct {
	atts []contract.Attachment
	ch   contract.ChannelOpener
}

// NewTurnAttachments builds the turn's attachment set from the event batch and the bound
// channels. The batch is the placed (downloaded) attachments the prompt listed.
func NewTurnAttachments(events []contract.MessageEvent, ch contract.ChannelOpener) contract.AttachmentSet {
	return turnAttachments{atts: BatchAttachments(events), ch: ch}
}

// Pick resolves the model's reference to the attachment(s) it meant — see the interface
// doc for the precedence (named-in-batch → named-in-inbox → batch want-matches).
func (t turnAttachments) Pick(ctx context.Context, want func(contract.Attachment) bool, hint string) []contract.Attachment {
	// 1. hint names a batch attachment of ANY kind → that one (caller gates on want, so a
	// named non-image surfaces a precise error instead of a silent swap).
	if m := matchAttachment(t.atts, hint); m != nil {
		return []contract.Attachment{*m}
	}
	// 2. a non-empty hint that missed the batch names an EARLIER-turn inbox file → that
	// one. Checked BEFORE the batch fallback: describeAttachments promises the path
	// stays resolvable across turns, so "no, the first one" must win over silently
	// substituting this turn's sole image.
	if strings.TrimSpace(hint) != "" {
		if ch := t.openInbox(ctx); ch != nil {
			defer ch.Close()
			if a := findInboxByName(ctx, ch, hint, want); a != nil {
				return []contract.Attachment{*a}
			}
		}
	}
	// 3. no hint, or the hint resolved nowhere (batch AND inbox missed) → the batch's
	// want-matches: sole → it; several → all (caller asks to name one). Empty when the
	// batch has none (the caller lists candidates via Suggest).
	return t.batchMatches(want)
}

// Suggest lists want-matching candidates (batch + recent inbox) for a "did you mean"
// error — it never resolves a subject.
func (t turnAttachments) Suggest(ctx context.Context, want func(contract.Attachment) bool) []contract.Attachment {
	if byWant := t.batchMatches(want); len(byWant) > 0 {
		return byWant
	}
	ch := t.openInbox(ctx)
	if ch == nil {
		return nil
	}
	defer ch.Close()
	return recentInbox(ctx, ch, want, 8)
}

func (t turnAttachments) Read(ctx context.Context, a contract.Attachment, max int64) ([]byte, error) {
	if max <= 0 {
		max = defaultReadCap
	}
	if t.ch == nil {
		return nil, fmt.Errorf("no file space")
	}
	ch, err := t.ch.OpenFile(ctx, "")
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	// Resolve to the local path and stat BEFORE reading, so an over-sized file never
	// reaches memory (local backend; a remote backend's AbsPath errors → reported up).
	abs, err := ch.AbsPath(a.Path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		// Don't return the raw *PathError: it embeds the $BOB_HOME-rooted absolute
		// path, which must never reach the model (the channel's own errors are
		// scrubbed for the same reason). Report the space-relative path + bare cause.
		return nil, fmt.Errorf("stat %s: %s", a.Path, pathCause(err))
	}
	if st.Size() > max {
		return nil, fmt.Errorf("file too large (max %dMB)", max/(1<<20))
	}
	// Read through the channel, not os.ReadFile(abs), so its error scrubbing applies.
	return ch.Read(ctx, a.Path)
}

// pathCause strips the path from an os error (a *fs.PathError embeds the absolute
// host path, which must not leak to the model); non-path errors pass through.
func pathCause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (t turnAttachments) batchMatches(want func(contract.Attachment) bool) []contract.Attachment {
	var out []contract.Attachment
	for _, a := range t.atts {
		if a.Path != "" && want(a) {
			out = append(out, a)
		}
	}
	return out
}

func (t turnAttachments) openInbox(ctx context.Context) contract.FileChannel {
	if t.ch == nil {
		return nil
	}
	ch, err := t.ch.OpenFile(ctx, "")
	if err != nil {
		return nil
	}
	return ch
}

// matchAttachment maps the model's reference onto THIS turn's attachment it names, across
// ALL kinds (so the caller can tell "named a non-image" from "named nothing"), or nil.
// Exact-ish matches first (full path, path base, file name), then a tolerant suffix on
// the cleaned base — anchored on the arg so a no-match still falls through.
func matchAttachment(atts []contract.Attachment, arg string) *contract.Attachment {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil
	}
	argBase := contract.CleanFileName(filepath.Base(arg))
	for i := range atts {
		a := &atts[i]
		switch {
		case a.Path != "" && a.Path == arg:
			return a
		case a.Path != "" && contract.CleanFileName(filepath.Base(a.Path)) == argBase:
			return a
		case a.FileName != "" && contract.CleanFileName(a.FileName) == argBase:
			return a
		}
	}
	for i := range atts {
		a := &atts[i]
		if n := contract.CleanFileName(a.FileName); n != "" && strings.HasSuffix(argBase, n) {
			return a
		}
		if p := contract.CleanFileName(filepath.Base(a.Path)); a.Path != "" && p != "" && strings.HasSuffix(argBase, p) {
			return a
		}
	}
	return nil
}

// imageExts are the extensions the inbox scan treats as image content when synthesising
// an Attachment for an earlier-turn file (cheaper than sniffing every file's bytes; the
// set mirrors the media tools' accepted formats, HEIC included).
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".bmp": true,
	".heic": true, ".heif": true, ".gif": true,
}

// inboxAttachment builds a synthetic Attachment for an inbox file so a want predicate
// (image / epub / …) can match an EARLIER-turn file by name. Kind/MIME are derived from
// the extension; Path is the space-relative inbox path.
func inboxAttachment(name string) contract.Attachment {
	ext := strings.ToLower(filepath.Ext(name))
	a := contract.Attachment{
		FileName: name,
		Path:     contract.InboxSubdir + "/" + name,
		MIME:     mime.TypeByExtension(ext),
	}
	if imageExts[ext] {
		a.Kind = "image"
	} else {
		a.Kind = "document"
	}
	return a
}

type inboxEntry struct {
	name string
	mod  int64
}

// inboxEntries lists the files under the space inbox with their mtimes (newest-wins
// across turns). A missing inbox → empty.
func inboxEntries(ctx context.Context, ch contract.FileChannel) []inboxEntry {
	listed, err := ch.List(ctx, contract.InboxSubdir)
	if err != nil {
		return nil
	}
	var out []inboxEntry
	for _, fe := range listed {
		if fe.IsDir {
			continue
		}
		var mod int64
		if abs, aerr := ch.AbsPath(contract.InboxSubdir + "/" + fe.Name); aerr == nil {
			if st, serr := os.Stat(abs); serr == nil {
				mod = st.ModTime().Unix()
			}
		}
		out = append(out, inboxEntry{name: fe.Name, mod: mod})
	}
	return out
}

// findInboxByName matches a model-supplied name against want-matching inbox files (an
// earlier turn's attachment); newest wins on a tie. Placement names inbox files from the
// clean display name, so the common case is an exact base match; the suffix tolerance
// catches a nameless attachment still on its unique staged base.
func findInboxByName(ctx context.Context, ch contract.FileChannel, hint string, want func(contract.Attachment) bool) *contract.Attachment {
	argBase := contract.CleanFileName(filepath.Base(strings.TrimSpace(hint)))
	if argBase == "" {
		return nil
	}
	best := contract.Attachment{}
	bestMod := int64(-1)
	for _, e := range inboxEntries(ctx, ch) {
		a := inboxAttachment(e.name)
		if !want(a) {
			continue
		}
		clean := contract.CleanFileName(e.name)
		if clean != argBase && !strings.HasSuffix(clean, argBase) {
			continue
		}
		if e.mod > bestMod {
			best, bestMod = a, e.mod
		}
	}
	if bestMod < 0 {
		return nil
	}
	return &best
}

// recentInbox returns up to n want-matching inbox files, newest first, for "did you mean"
// errors.
func recentInbox(ctx context.Context, ch contract.FileChannel, want func(contract.Attachment) bool, n int) []contract.Attachment {
	entries := inboxEntries(ctx, ch)
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod > entries[j].mod })
	var out []contract.Attachment
	for _, e := range entries {
		a := inboxAttachment(e.name)
		if !want(a) {
			continue
		}
		out = append(out, a)
		if len(out) >= n {
			break
		}
	}
	return out
}
