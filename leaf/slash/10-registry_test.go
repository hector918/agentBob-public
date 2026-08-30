package slash

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/i18n"
)

// capSink records the one notice a slash command delivers. It declares no
// contract.LineBreakSink, i.e. a plain-text channel: dispatch must hand its
// text through untouched.
type capSink struct {
	finished string
	streamed string // ContentDelta accumulation, for the line-break prefix check
	done     bool
}

func (s *capSink) ContentDelta(text string) { s.streamed += text }
func (s *capSink) TraceDelta(string)        {}
func (s *capSink) Finish(full string) error {
	s.finished = full
	s.done = true
	return nil
}
func (s *capSink) SendFile(string, string) error { return nil }
func (s *capSink) LastSent() string              { return "" }

func dispatch(r *registry, text string, admin bool) *capSink {
	sink := &capSink{}
	r.Dispatch(context.Background(), contract.SlashContext{
		Event:   contract.MessageEvent{Text: text},
		Sink:    sink,
		IsAdmin: admin,
	})
	return sink
}

func TestDispatch_RunsHandlerWithArgs(t *testing.T) {
	r := newRegistry()
	var gotArgs string
	ran := false
	r.Register(contract.SlashCommand{Name: "echo", Description: "d", Handler: func(_ context.Context, sc contract.SlashContext) error {
		ran = true
		gotArgs = sc.Args
		return sc.Sink.Finish("ok:" + sc.Args)
	}})

	sink := dispatch(r, "/echo hello world", false)
	if !ran || gotArgs != "hello world" {
		t.Fatalf("ran=%v args=%q, want true / 'hello world'", ran, gotArgs)
	}
	if sink.finished != "ok:hello world" {
		t.Fatalf("reply = %q", sink.finished)
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	r := newRegistry()
	sink := dispatch(r, "/nope", false)
	if sink.finished != i18n.T("slash.dispatch.unknown", "") {
		t.Fatalf("reply = %q, want the unknown notice", sink.finished)
	}
}

func TestDispatch_AdminOnlyRejectsNonAdmin(t *testing.T) {
	r := newRegistry()
	ran := false
	r.Register(contract.SlashCommand{Name: "secret", Description: "d", AdminOnly: true, Handler: func(_ context.Context, _ contract.SlashContext) error {
		ran = true
		return nil
	}})

	sink := dispatch(r, "/secret", false)
	if ran {
		t.Fatal("admin-only command ran for a non-admin sender")
	}
	if sink.finished != i18n.T("slash.dispatch.admin_only", "") {
		t.Fatalf("reply = %q, want the admin-only notice", sink.finished)
	}
	// An admin sender runs it.
	dispatch(r, "/secret", true)
	if !ran {
		t.Fatal("admin-only command did not run for an admin sender")
	}
}

func TestDispatch_NameCaseInsensitive(t *testing.T) {
	r := newRegistry()
	ran := false
	r.Register(contract.SlashCommand{Name: "new", Description: "d", Handler: func(_ context.Context, sc contract.SlashContext) error {
		ran = true
		return sc.Sink.Finish("ok")
	}})
	dispatch(r, "/NEW", false)
	if !ran {
		t.Fatal("/NEW should match the registered 'new' command")
	}
}

func TestHelp_ListsVisibleCommands(t *testing.T) {
	r := newRegistry()
	r.registerBuiltins() // /help
	r.Register(contract.SlashCommand{Name: "new", Description: "开启新会话", Handler: func(_ context.Context, sc contract.SlashContext) error { return nil }})
	r.Register(contract.SlashCommand{Name: "admincmd", Description: "管理员", AdminOnly: true, Handler: func(_ context.Context, sc contract.SlashContext) error { return nil }})

	// Non-admin: sees help + new, NOT admincmd.
	sink := dispatch(r, "/help", false)
	if !strings.Contains(sink.finished, "/new") || !strings.Contains(sink.finished, "/help") {
		t.Fatalf("help = %q, want /new + /help", sink.finished)
	}
	if strings.Contains(sink.finished, "/admincmd") {
		t.Fatalf("help leaked an admin-only command to a non-admin: %q", sink.finished)
	}
	// Admin: also sees admincmd.
	if s := dispatch(r, "/help", true); !strings.Contains(s.finished, "/admincmd") {
		t.Fatalf("admin help missing the admin command: %q", s.finished)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	r := newRegistry()
	r.Register(contract.SlashCommand{Name: "x", Description: "d", Handler: func(_ context.Context, _ contract.SlashContext) error { return nil }})
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate command name should panic")
		}
	}()
	r.Register(contract.SlashCommand{Name: "x", Description: "d2", Handler: func(_ context.Context, _ contract.SlashContext) error { return nil }})
}

func TestParseCommand_StripsBotSuffix(t *testing.T) {
	cases := []struct{ in, name, args string }{
		{"/session@binserv00Bot", "session", ""},
		{"/session@binserv00Bot foo bar", "session", "foo bar"},
		{"/session", "session", ""},
		{"/New hi", "new", "hi"},
	}
	for _, c := range cases {
		n, a := parseCommand(c.in)
		if n != c.name || a != c.args {
			t.Errorf("parseCommand(%q) = (%q,%q), want (%q,%q)", c.in, n, a, c.name, c.args)
		}
	}
}

// mdSink is a capSink on a markdown-rendering channel: it declares the GFM hard
// break the way telegram's wire does (contract.LineBreakSink).
type mdSink struct{ capSink }

func (s *mdSink) LineBreak() string { return "  \n" }

var _ contract.LineBreakSink = (*mdSink)(nil)

// bareSink is an mdSink that ALSO carries the contract.BareProductSink marker —
// its Finish full is a product consumed verbatim, not text rendered to a reader.
type bareSink struct{ mdSink }

func (s *bareSink) BareProductFinish() {}

var _ contract.BareProductSink = (*bareSink)(nil)

func dispatchTo(r *registry, sink contract.Sink, text string) {
	r.Dispatch(context.Background(), contract.SlashContext{
		Event: contract.MessageEvent{Text: text},
		Sink:  sink,
	})
}

func registerLines(r *registry) {
	r.Register(contract.SlashCommand{Name: "lines", Description: "d", Handler: func(_ context.Context, sc contract.SlashContext) error {
		sc.Sink.ContentDelta("a\nb")
		return sc.Sink.Finish("a\nb\n\nc")
	}})
}

// TestDispatch_UsesChannelLineBreak: a slash reply is bob's own line-structured
// UI text, so dispatch re-joins its lines with the separator the CHANNEL
// declares. On a markdown channel a bare "\n" is a soft break that collapses the
// whole reply into one paragraph, so it goes out as a GFM hard break.
func TestDispatch_UsesChannelLineBreak(t *testing.T) {
	r := newRegistry()
	registerLines(r)

	sink := &mdSink{}
	dispatchTo(r, sink, "/lines")
	if want := "a  \nb  \n  \nc"; sink.finished != want {
		t.Fatalf("reply = %q, want %q", sink.finished, want)
	}
	// The streamed prefix must stay a byte-exact prefix of the final full, or a
	// streaming sink re-renders the reply from offset 0.
	if !strings.HasPrefix(sink.finished, sink.streamed) {
		t.Fatalf("streamed %q is not a prefix of full %q", sink.streamed, sink.finished)
	}
}

// TestDispatch_PlainChannelUntouched is the other half: the hard break belongs
// to a markdown channel, not to slash. A channel that declares nothing is plain
// text, where "\n" is already a line boundary — dispatch must not litter it with
// another platform's encoding.
func TestDispatch_PlainChannelUntouched(t *testing.T) {
	r := newRegistry()
	registerLines(r)

	sink := dispatch(r, "/lines", false) // capSink: no LineBreakSink
	if want := "a\nb\n\nc"; sink.finished != want {
		t.Fatalf("reply = %q, want %q (untouched)", sink.finished, want)
	}
}

// TestDispatch_BareProductSinkNotRewritten: a product sink's Finish is a
// payload (a bridge return event, a coalesced mail segment), so its line
// endings are left exactly as the handler wrote them — even on a channel that
// declares a separator.
func TestDispatch_BareProductSinkNotRewritten(t *testing.T) {
	r := newRegistry()
	registerLines(r)

	sink := &bareSink{}
	dispatchTo(r, sink, "/lines")
	if sink.finished != "a\nb\n\nc" {
		t.Fatalf("product = %q, want the handler's bytes untouched", sink.finished)
	}
}
