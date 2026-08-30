package compose

import (
	"context"
	"errors"
	"testing"

	"agentbob/contract"
)

// TestTurnAttachmentsPick covers the batch resolution precedence: named-any-kind first
// (so a named non-image is returned for the caller to reject), then the want-matches
// (sole or several). The inbox fallback needs a channel and is not exercised here.
func TestTurnAttachmentsPick(t *testing.T) {
	atts := []contract.Attachment{
		{Kind: "image", Path: "inbox/photo-99.jpg", FileName: "photo-99.jpg"},
		{Kind: "image", Path: "inbox/pic.png", FileName: "pic.png"},
		{Kind: "document", MIME: "application/pdf", Path: "inbox/report.pdf", FileName: "report.pdf"},
	}
	set := NewTurnAttachments([]contract.MessageEvent{{Attachments: atts}}, nil)
	img := contract.Attachment.IsImageContent
	ctx := context.Background()

	// named image → that one
	if got := set.Pick(ctx, img, "photo-99.jpg"); len(got) != 1 || got[0].Path != "inbox/photo-99.jpg" {
		t.Errorf("named image: %+v, want [inbox/photo-99.jpg]", got)
	}
	// named via a scrambled-prefix suffix → still matches
	if got := set.Pick(ctx, img, "9999-xxxx-photo-99.jpg"); len(got) != 1 || got[0].Path != "inbox/photo-99.jpg" {
		t.Errorf("suffix match: %+v, want [inbox/photo-99.jpg]", got)
	}
	// named NON-image (pdf) → returned across all kinds, caller gates: not image content
	got := set.Pick(ctx, img, "report.pdf")
	if len(got) != 1 || got[0].Path != "inbox/report.pdf" || got[0].IsImageContent() {
		t.Errorf("named non-image: %+v, want the pdf (caller rejects on want)", got)
	}
	// no hint, several images → all of them (caller asks to name one)
	if got := set.Pick(ctx, img, ""); len(got) != 2 {
		t.Errorf("multi images: len %d, want 2", len(got))
	}
	// a hint that misses BOTH the batch and the inbox (nil channel → no inbox) still
	// surfaces the want-matches (the caller's "name one" path) — same as the no-hint case.
	if got := set.Pick(ctx, img, "nope.gif"); len(got) != 2 {
		t.Errorf("double-miss hint: len %d, want the 2 images (name-one path)", len(got))
	}
	// no want-match at all (+ no channel for inbox) → empty
	none := func(contract.Attachment) bool { return false }
	if got := set.Pick(ctx, none, "anything"); len(got) != 0 {
		t.Errorf("no want-match: %+v, want empty", got)
	}
}

// TestTurnAttachmentsSoleImage: exactly one image (+ a non-image) → the lone image is the
// subject ONLY when the hint is empty or resolves nowhere (missed the batch AND the
// inbox); a hint naming a real earlier-turn inbox file wins over the sole batch image.
func TestTurnAttachmentsSoleImage(t *testing.T) {
	atts := []contract.Attachment{
		{Kind: "image", Path: "inbox/only.jpg", FileName: "only.jpg"},
		{Kind: "document", MIME: "application/pdf", Path: "inbox/x.pdf", FileName: "x.pdf"},
	}
	img := contract.Attachment.IsImageContent
	ctx := context.Background()

	// No channel (no inbox reachable): empty hint and a nowhere-resolving hint both
	// fall back to the sole batch image.
	set := NewTurnAttachments([]contract.MessageEvent{{Attachments: atts}}, nil)
	if got := set.Pick(ctx, img, ""); len(got) != 1 || got[0].Path != "inbox/only.jpg" {
		t.Errorf("empty hint, sole image: %+v, want [inbox/only.jpg]", got)
	}
	if got := set.Pick(ctx, img, "guessed-wrong.png"); len(got) != 1 || got[0].Path != "inbox/only.jpg" {
		t.Errorf("double-miss hint, sole image: %+v, want [inbox/only.jpg]", got)
	}

	// With an inbox holding an earlier turn's photo-A.jpg: a hint naming it resolves
	// there — never silently swapped for this turn's sole image (D25).
	setIn := NewTurnAttachments(
		[]contract.MessageEvent{{Attachments: atts}},
		fakeOpener{ch: &fakeInbox{names: []string{"photo-A.jpg"}}},
	)
	if got := setIn.Pick(ctx, img, "inbox/photo-A.jpg"); len(got) != 1 || got[0].Path != "inbox/photo-A.jpg" {
		t.Errorf("inbox-named hint: %+v, want [inbox/photo-A.jpg]", got)
	}
	// A hint missing both the batch and the inbox still falls back to the sole image.
	if got := setIn.Pick(ctx, img, "nowhere.png"); len(got) != 1 || got[0].Path != "inbox/only.jpg" {
		t.Errorf("double-miss with inbox present: %+v, want [inbox/only.jpg]", got)
	}
}

// fakeInbox is a minimal FileChannel whose List serves the inbox names; everything
// else errors (findInboxByName only needs List; a failing AbsPath just zeroes mtimes).
type fakeInbox struct{ names []string }

func (f *fakeInbox) Alive() bool  { return true }
func (f *fakeInbox) Close() error { return nil }
func (f *fakeInbox) List(context.Context, string) ([]contract.FileEntry, error) {
	out := make([]contract.FileEntry, 0, len(f.names))
	for _, n := range f.names {
		out = append(out, contract.FileEntry{Name: n})
	}
	return out, nil
}
func (f *fakeInbox) Read(context.Context, string) ([]byte, error) { return nil, errNoOp }
func (f *fakeInbox) Write(context.Context, string, []byte) error  { return errNoOp }
func (f *fakeInbox) Mkdir(context.Context, string) error          { return errNoOp }
func (f *fakeInbox) Remove(context.Context, string) error         { return errNoOp }
func (f *fakeInbox) Rename(context.Context, string, string) error { return errNoOp }
func (f *fakeInbox) AbsPath(string) (string, error)               { return "", errNoOp }

var errNoOp = errors.New("not implemented")

// fakeOpener vends the fake inbox as the default space's FileChannel.
type fakeOpener struct{ ch contract.FileChannel }

func (o fakeOpener) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return o.ch, nil
}
func (o fakeOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, errNoOp
}
