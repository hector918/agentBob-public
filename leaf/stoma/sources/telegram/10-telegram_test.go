package telegram

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/heartwood/prompt"

	"github.com/go-telegram/bot/models"
)

// TestSafeEmit pins the shutdown-race guard: the SDK's Start doesn't join its
// per-update handler goroutines, so a handler (or a media-group AfterFunc) can
// still be emitting after Run returned and the Hub closed the events channel.
// safeEmit must drop the event in that window — never panic the process — and
// still deliver on the live path.
func TestSafeEmit(t *testing.T) {
	t.Run("live path delivers", func(t *testing.T) {
		ch := make(chan contract.MessageEvent, 1)
		s := &Source{out: ch}
		s.safeEmit(context.Background(), contract.MessageEvent{MessageID: "m0"})
		if len(ch) != 1 {
			t.Fatalf("live emit should deliver, buffered=%d", len(ch))
		}
	})

	t.Run("closed latch drops before the send", func(t *testing.T) {
		ch := make(chan contract.MessageEvent)
		close(ch)
		s := &Source{out: ch}
		s.closed.Store(true) // normal shutdown ordering: Run latched before the Hub closed the channel
		s.safeEmit(context.Background(), contract.MessageEvent{MessageID: "m1"})
	})

	t.Run("recover absorbs a send on a closed channel", func(t *testing.T) {
		// TOCTOU residue: the handler read closed=false just before the latch
		// flipped and the channel closed — recover must absorb the send panic.
		ch := make(chan contract.MessageEvent)
		close(ch)
		s := &Source{out: ch}
		s.safeEmit(context.Background(), contract.MessageEvent{MessageID: "m2"})
	})
}

// TestDescribeReplyTo_MediaNoteOrderAndReach pins the two properties the quoted-reply
// media note has to keep, both learned from the inbox-rummaging incident:
// the note leads (so ReplyLine's cut eats prose, never the note's tail clause), and it
// claims out-of-reach ONLY when this turn is not ingesting the parent's media.
func TestDescribeReplyTo_MediaNoteOrderAndReach(t *testing.T) {
	photo := &models.Message{Photo: []models.PhotoSize{{FileID: "f"}}, Caption: "看这个"}

	txt, _ := describeReplyTo(photo, false, false)
	if want := "（一张图片，不是本轮附件） 看这个"; txt != want {
		t.Errorf("unreachable parent: got %q, want %q", txt, want)
	}
	// Reachable: the file IS in this turn's attachment list, so no out-of-reach claim —
	// it would contradict the list AND suppress a legitimate look at the picture.
	txt, _ = describeReplyTo(photo, false, true)
	if want := "（一张图片） 看这个"; txt != want {
		t.Errorf("reachable parent: got %q, want %q", txt, want)
	}
	// The note must precede the prose whatever the prose length, so ReplyLine's
	// QuotedMax truncation can never reach the clause.
	long := &models.Message{Photo: []models.PhotoSize{{FileID: "f"}}, Caption: strings.Repeat("字", 300)}
	txt, _ = describeReplyTo(long, false, false)
	if !strings.HasPrefix(txt, "（一张图片，不是本轮附件）") {
		t.Errorf("note not leading: %.60q", txt)
	}
	line := prompt.ReplyLine(contract.MessageEvent{ReplyToUser: "Alice", ReplyToText: txt})
	if !strings.Contains(line, "不是本轮附件") {
		t.Errorf("clause lost to truncation: %q", line)
	}
	// No media, no note.
	if txt, _ := describeReplyTo(&models.Message{Text: "hi"}, false, false); txt != "hi" {
		t.Errorf("text-only parent got a note: %q", txt)
	}
}
