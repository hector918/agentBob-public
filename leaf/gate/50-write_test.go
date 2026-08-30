package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGranterWriteWhole pins the locked full-file write the webui panel save now
// routes through (the lost-update fix): it persists the body, backs up the prior
// content to .bak, and rejects a path-traversing source name — all under g.mu, so a
// panel save serializes with a concurrent /gate allow surgical write.
func TestGranterWriteWhole(t *testing.T) {
	dir := t.TempDir()
	g := &granter{dir: dir, reload: func() error { return nil }}
	path := filepath.Join(dir, "telegram.yaml")

	if err := g.WriteWhole("telegram", []byte("allowlist:\n  - u1\n")); err != nil {
		t.Fatalf("WriteWhole: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "u1") {
		t.Fatalf("body not persisted: %q", b)
	}

	// overwrite → prior content lands in .bak (best-effort backup before the write).
	if err := g.WriteWhole("telegram", []byte("admins:\n  - u2\n")); err != nil {
		t.Fatalf("WriteWhole overwrite: %v", err)
	}
	if b, _ := os.ReadFile(path + ".bak"); !strings.Contains(string(b), "u1") {
		t.Fatalf(".bak should hold the prior content, got %q", b)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "u2") {
		t.Fatalf("overwrite not persisted: %q", b)
	}

	// a path-traversing source name is refused before any fs touch (mirrors write()).
	if err := g.WriteWhole("../etc/passwd", []byte("x")); err == nil {
		t.Error("invalid source name must be rejected")
	}
}

// TestGranterAllowAlreadyPresent pins that already-present (changed=false) is not a
// terminal success (F116/F138): the reload still runs — repairing a live policy left
// stale by a prior "write landed, reload failed" attempt — and the rejected-feed row
// is still forgotten, clearing the ghost row of a sender allowlisted out-of-band
// (webui yaml editor / hand edit). A reload failure on this path must propagate so
// the admit redeem stays Transient (token kept) instead of a false success.
func TestGranterAllowAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "telegram.yaml"), []byte("allowlist:\n  - \"u1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rej := NewRejectedSenders(10)
	rej.Record("telegram", "c1", "u1", "", "")
	reloads := 0
	reloadErr := error(nil)
	g := &granter{dir: dir, rejected: rej, reload: func() error { reloads++; return reloadErr }}

	// uid already on disk: changed=false, but reload runs and the ghost row clears.
	changed, err := g.Allow(context.Background(), "telegram", "u1")
	if changed || err != nil {
		t.Fatalf("already-present: changed=%v err=%v, want false/nil", changed, err)
	}
	if reloads != 1 {
		t.Errorf("reload must run on the already-present path, ran %d times", reloads)
	}
	if len(rej.List()) != 0 {
		t.Error("ghost feed row must be cleared on the already-present path")
	}

	// a failing reload on the same path is surfaced, and the feed row survives
	// (the sender is NOT live-allowed yet — forgetting them would hide the queue).
	rej.Record("telegram", "c1", "u1", "", "")
	reloadErr = errors.New("sibling yaml broken")
	if _, err := g.Allow(context.Background(), "telegram", "u1"); err == nil {
		t.Error("reload failure must propagate on the already-present path (no false success)")
	}
	if len(rej.List()) != 1 {
		t.Error("feed row must survive a failed reload")
	}
}

// TestGranterDenylistRecheck pins F139: the granter refuses to allowlist an applicant
// who is on the denylist (source-level for Allow, chat-level for AllowInChat), returning
// the distinct errUserDenied and writing NOTHING — no contradictory yaml, no false
// success. A nil denied predicate (tests / no screener) skips the check.
func TestGranterDenylistRecheck(t *testing.T) {
	dir := t.TempDir()
	denied := map[string]bool{"telegram//bad": true, "telegram/g1/chatbad": true}
	g := &granter{dir: dir, reload: func() error { return nil }, rejected: NewRejectedSenders(10),
		denied: func(source, chatID, uid string) bool { return denied[source+"/"+chatID+"/"+uid] }}

	// source-wide Allow of a denied uid → errUserDenied, no file written.
	if _, err := g.Allow(context.Background(), "telegram", "bad"); !errors.Is(err, errUserDenied) {
		t.Fatalf("Allow of a denied uid must return errUserDenied, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "telegram.yaml")); statErr == nil {
		t.Fatal("a refused grant must not create/patch the policy file")
	}

	// per-chat AllowInChat of a chat-denied uid → errUserDenied.
	if _, err := g.AllowInChat(context.Background(), "telegram", "g1", "chatbad"); !errors.Is(err, errUserDenied) {
		t.Fatalf("AllowInChat of a chat-denied uid must return errUserDenied, got %v", err)
	}

	// a non-denied uid still writes normally.
	if changed, err := g.Allow(context.Background(), "telegram", "good"); err != nil || !changed {
		t.Fatalf("non-denied Allow must succeed, got changed=%v err=%v", changed, err)
	}
}

// TestGranterAllowInChatAlreadyPresent mirrors the Allow pin for the per-chat path:
// already-present still reloads and forgets THIS chat's row only (L-gate-D1).
func TestGranterAllowInChatAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	body := "groups:\n  \"g1\":\n    allowlist_add:\n      - \"u1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "telegram.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rej := NewRejectedSenders(10)
	rej.Record("telegram", "g1", "u1", "", "")
	rej.Record("telegram", "g2", "u1", "", "")
	reloads := 0
	g := &granter{dir: dir, rejected: rej, reload: func() error { reloads++; return nil }}

	changed, err := g.AllowInChat(context.Background(), "telegram", "g1", "u1")
	if changed || err != nil {
		t.Fatalf("already-present: changed=%v err=%v, want false/nil", changed, err)
	}
	if reloads != 1 {
		t.Errorf("reload must run on the already-present path, ran %d times", reloads)
	}
	left := rej.List()
	if len(left) != 1 || left[0].ChatID != "g2" {
		t.Errorf("only THIS chat's row is forgotten; left %+v", left)
	}
}

// TestPatchListAlreadyPresentEmailCase pins the write-side twin of the IDList
// carve-out (F25): a lowercased grant against a mixed-case email entry already in
// the yaml is already-present, not a duplicate append.
func TestPatchListAlreadyPresentEmailCase(t *testing.T) {
	raw := []byte("allowlist:\n  - \"John.Doe@Example.com\"\n")
	if _, changed, err := patchPolicyList(raw, "allowlist", "john.doe@example.com"); err != nil || changed {
		t.Errorf("mixed-case email already present: changed=%v err=%v, want false/nil", changed, err)
	}
	// non-@ ids stay exact: a case-differing platform id is a REAL new entry.
	raw = []byte("allowlist:\n  - \"U0AB\"\n")
	if _, changed, err := patchPolicyList(raw, "allowlist", "u0ab"); err != nil || !changed {
		t.Errorf("case-differing platform id must append: changed=%v err=%v, want true/nil", changed, err)
	}
}

// TestPatchListNullValue pins the null-scalar upgrade: an operator writing a blank
// `allowlist:` line (the natural empty-list spelling) yields a null SCALAR node —
// appending to its Content used to be silently dropped by Marshal (changed=true,
// err=nil, uid never lands, admit token burned). Both patchers must upgrade the
// null in place and actually land the uid; any other non-sequence value errors.
func TestPatchListNullValue(t *testing.T) {
	out, changed, err := patchPolicyList([]byte("allow_all: false\nallowlist:\n"), "allowlist", "9999")
	if err != nil || !changed {
		t.Fatalf("patchPolicyList blank allowlist: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "9999") {
		t.Fatalf("uid must land in the output, got %q", out)
	}

	out, changed, err = patchGroupAllowlistAdd([]byte("groups:\n  \"g1\":\n    allowlist_add:\n"), "g1", "9999")
	if err != nil || !changed {
		t.Fatalf("patchGroupAllowlistAdd blank allowlist_add: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "9999") {
		t.Fatalf("uid must land in the output, got %q", out)
	}

	// a non-null, non-sequence value (mapping) is operator error, not a silent no-op.
	if _, _, err := patchPolicyList([]byte("allowlist:\n  a: b\n"), "allowlist", "9999"); err == nil {
		t.Error("non-sequence allowlist must be rejected")
	}
}
