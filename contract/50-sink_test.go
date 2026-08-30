package contract

import (
	"errors"
	"testing"
)

// bareSink implements nothing optional: pictures can only go out as attachments.
type bareSink struct {
	files   []string
	sendErr error
}

func (s *bareSink) ContentDelta(string) {}
func (s *bareSink) TraceDelta(string)   {}
func (s *bareSink) Finish(string) error { return nil }
func (s *bareSink) LastSent() string    { return "" }
func (s *bareSink) SendFile(p, _ string) error {
	s.files = append(s.files, p)
	return s.sendErr
}

// holdingSink defers every picture and reports nothing until released.
type holdingSink struct {
	bareSink
	pending []func(error)
}

func (s *holdingSink) HoldPicture(_, _ string, onSent func(error)) {
	s.pending = append(s.pending, onSent)
}

// A sink that does not hold sends immediately, and the caller learns the outcome
// synchronously — through BOTH channels. This is the branch every non-looping turn
// takes, and the clause a future holder's author is most likely to get wrong: a
// producer that trusted `held` to always be true would report a picture as handled
// while its send was already known to have failed.
func TestDeliverPictureWhenReady_NonHolderSendsNowAndReportsBothWays(t *testing.T) {
	sink := &bareSink{sendErr: errors.New("chat is gone")}

	var got error
	calls := 0
	held, err := DeliverPictureWhenReady(sink, "/tmp/a.png", "cap", func(e error) {
		calls++
		got = e
	})

	if held {
		t.Error("held = true for a sink with no HoldPicture — the picture was already sent")
	}
	if err == nil {
		t.Error("err = nil though the send failed — the producer cannot report what it is not told")
	}
	if calls != 1 {
		t.Fatalf("onSent ran %d times, want exactly 1", calls)
	}
	if got == nil || got.Error() != err.Error() {
		t.Errorf("onSent got %v, want the same failure the caller got (%v)", got, err)
	}
	if len(sink.files) != 1 {
		t.Errorf("files = %q, want the picture attempted once", sink.files)
	}
}

// A holder takes the picture and nothing is sent yet: held says so, err is nil
// (there is no outcome), and onSent has not run. A producer must not read that nil
// as success.
func TestDeliverPictureWhenReady_HolderDefersTheOutcome(t *testing.T) {
	sink := &holdingSink{}

	calls := 0
	held, err := DeliverPictureWhenReady(sink, "/tmp/a.png", "", func(error) { calls++ })

	if !held || err != nil {
		t.Fatalf("held=%v err=%v, want held with no outcome yet", held, err)
	}
	if calls != 0 {
		t.Error("onSent ran while the picture was still held")
	}
	if len(sink.files) != 0 {
		t.Errorf("a held picture reached the wire anyway: %q", sink.files)
	}

	sink.pending[0](nil) // the turn ends
	if calls != 1 {
		t.Errorf("onSent ran %d times after release, want 1", calls)
	}
}
