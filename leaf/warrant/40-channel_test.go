package warrant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpaceLocalDirFoldsScopeSeparators: every real scope shape (incl. an agora member
// sub-scope with '#', which once errored "invalid space name") must resolve to a valid
// space dir under spaces/, with no separator surviving into the path leaf. Placement and
// the FileChannel both go through this, so a rejected scope breaks image/file reads.
func TestSpaceLocalDirFoldsScopeSeparators(t *testing.T) {
	home := "/home/bob"
	for _, sc := range []string{
		"telegram:group:-1004355956597#raytam-wp", // agora member sub-scope ('#' regression)
		"member:alice/inbox:1",                    // member/inbox
		"company:co_djdaz46sxekw_fc945acd",        // company space
		"telegram:dm:222222222",
	} {
		dir, err := SpaceLocalDir(home, sc)
		if err != nil {
			t.Errorf("SpaceLocalDir(%q) errored: %v", sc, err)
			continue
		}
		if !strings.HasPrefix(dir, home+"/spaces/") {
			t.Errorf("SpaceLocalDir(%q) = %q, not under spaces/", sc, dir)
		}
		if strings.ContainsAny(filepath.Base(dir), ":/#") {
			t.Errorf("SpaceLocalDir(%q) = %q still has a separator in the leaf", sc, dir)
		}
	}
	// Empty / pure-dot names stay rejected (the traversal guard).
	for _, bad := range []string{"", "..", "..."} {
		if _, err := SpaceLocalDir(home, bad); err == nil {
			t.Errorf("SpaceLocalDir(%q) should be rejected", bad)
		}
	}
}

// TestFileChannelErrorsHideAbsPath: a failed op must not leak the $BOB_HOME-rooted
// absolute path back to the caller — the model sees these errors and must only ever
// learn space-relative paths, never bob's on-disk layout. cat'ing a directory and
// reading a missing file are the common leak shapes.
func TestFileChannelErrorsHideAbsPath(t *testing.T) {
	home := t.TempDir()
	m := &Manager{home: home}
	m.policy.Store(emptyPolicy())
	ctx := context.Background()

	ch, err := m.File(ctx, "user", "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	noLeak := func(op string, err error) {
		if err == nil {
			t.Fatalf("%s: expected an error", op)
		}
		if strings.Contains(err.Error(), home) {
			t.Errorf("%s error leaks the absolute space path: %q", op, err)
		}
	}

	if err := ch.Write(ctx, "dir/f.txt", []byte("x")); err != nil { // make a dir to cat
		t.Fatal(err)
	}
	_, rerr := ch.Read(ctx, "dir") // cat a directory → "... is a directory"
	noLeak("read-dir", rerr)
	if !strings.Contains(rerr.Error(), "dir") {
		t.Errorf("read-dir error should name the space-relative path, got %q", rerr)
	}
	_, rerr = ch.Read(ctx, "missing.txt") // read missing → "... no such file"
	noLeak("read-missing", rerr)
	_, lerr := ch.List(ctx, "missing-dir")
	noLeak("list-missing", lerr)
}

// TestFileChannelOps: a local file channel does the verbs and confines all paths
// to the space root.
func TestFileChannelOps(t *testing.T) {
	m := &Manager{home: t.TempDir()}
	m.policy.Store(emptyPolicy())
	ctx := context.Background()

	ch, err := m.File(ctx, "user", "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	if err := ch.Write(ctx, "a/b.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := ch.Read(ctx, "a/b.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read = %q / %v, want hello", data, err)
	}
	ents, err := ch.List(ctx, "a")
	if err != nil || len(ents) != 1 || ents[0].Name != "b.txt" {
		t.Fatalf("list = %v / %v, want [b.txt]", ents, err)
	}
	if err := ch.Rename(ctx, "a/b.txt", "a/c.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := ch.Read(ctx, "a/c.txt"); err != nil {
		t.Errorf("rename target missing: %v", err)
	}
	if err := ch.Remove(ctx, "a/c.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := ch.Read(ctx, "a/c.txt"); err == nil {
		t.Error("removed file is still readable")
	}
}

// TestFileChannelConfines: ".." is clamped to the root (no escape) and distinct
// spaces don't see each other.
func TestFileChannelConfines(t *testing.T) {
	m := &Manager{home: t.TempDir()}
	m.policy.Store(emptyPolicy())
	ctx := context.Background()

	ch, _ := m.File(ctx, "user", "s1")
	defer ch.Close()
	// "../../esc.txt" is neutralized to the root, not the parent.
	if err := ch.Write(ctx, "../../esc.txt", []byte("y")); err != nil {
		t.Fatalf("clamped write: %v", err)
	}
	if data, err := ch.Read(ctx, "esc.txt"); err != nil || string(data) != "y" {
		t.Errorf("escape write should land at the root (read esc.txt = %q/%v)", data, err)
	}

	// A second space is isolated.
	ch2, _ := m.File(ctx, "user", "s2")
	defer ch2.Close()
	if _, err := ch2.Read(ctx, "esc.txt"); err == nil {
		t.Error("space s2 must not see s1's files")
	}

	// A space name with separators is FOLDED to one safe path component (e.g.
	// "member:alice/inbox:1" → "member_alice_inbox_1"), not rejected and not nested:
	// the '/' can never reach filepath.Join as a real separator, so it stays confined.
	chSep, err := m.File(ctx, "user", "member:alice/inbox:1")
	if err != nil {
		t.Fatalf("a separator space name must be accepted (folded to one component), got %v", err)
	}
	defer chSep.Close()
	if err := chSep.Write(ctx, "f.txt", []byte("z")); err != nil {
		t.Fatalf("write to folded space: %v", err)
	}
	if _, err := ch.Read(ctx, "f.txt"); err == nil {
		t.Error("a folded separator space must be isolated, not nested under space s1")
	}

	// Pure-dot names ("."/".."/"...") must be rejected — spaceNameRe admits ".",
	// so without the pure-dot guard ".." would join to $BOB_HOME itself (out of
	// spaces/, exposing credentials). Names with any non-dot char stay valid.
	for _, bad := range []string{".", "..", "..."} {
		if _, err := m.File(ctx, "user", bad); err == nil {
			t.Errorf("pure-dot space name %q must be rejected (sandbox escape)", bad)
		}
	}
	for _, ok := range []string{"a..b", "..env", ".env"} {
		if _, err := m.File(ctx, "user", ok); err != nil {
			t.Errorf("space name %q with a non-dot char must stay valid, got %v", ok, err)
		}
	}
}
