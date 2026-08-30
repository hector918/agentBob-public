package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"agentbob/contract"

	"gopkg.in/yaml.v3"
)

// errUserDenied is returned by the granter when a grant is refused because the
// applicant is on the source (or chat) denylist — a distinct sentinel so the
// redeem/slash callers can say "remove them from the denylist first" instead of
// writing a contradictory allowlist row + reporting a false success (F139).
var errUserDenied = errors.New("gate: applicant is denylisted")

// sourceNameRe guards the source name before it is joined into a file path. The
// invariant is "no path separators": any match is a single path component, and
// the constant ".yaml" suffix keeps it one — so even a bare ".." becomes the
// harmless file "...yaml" INSIDE dir, never the parent. A name containing "/" or
// "\" is refused outright. (".." matching the class is fine; the suffix neuters
// it — the protection is the separator ban, not a dot ban.)
var sourceNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// granter writes per-source access policy: it adds a sender to a source's
// allowlist (binding's auto-allowlist), then reloads the live policy and forgets
// the rejected record. It implements contract.AccessGranter.
//
// The rebuild gate uses ONE flat <source>.yaml per source name (no multi-bot
// bots: shape), so a write is a simple read-modify-write of that file's Policy.
type granter struct {
	dir      string
	reload   func() error // gate's live hot-swap (Module.Reload)
	rejected *RejectedSenders
	// denied reports whether uid is on source's (source-level or chat-level)
	// denylist — checked before every allowlist write so a grant can't contradict
	// an active block (F139). Closes over the live screener; nil in tests skips the
	// check. chatID "" = source-level only.
	denied func(source, chatID, uid string) bool
	mu     sync.Mutex // serializes concurrent writes to the same file (single process)
}

// Allow adds uid to source's allowlist, reloads, and forgets the rejected record.
//
// changed=false (uid already on disk) is NOT a terminal success: a prior attempt
// may have landed the write and then failed the reload (LoadAll is all-or-nothing,
// so a broken SIBLING yaml fails it), leaving the live policy stale — a retry must
// repair that, not report a false success. So the reload and the feed forget run
// on the already-present path too; reload is idempotent and cheap.
func (g *granter) Allow(_ context.Context, source, uid string) (bool, error) {
	// Denylist wins over an allowlist grant: refuse rather than write a
	// contradictory row that Screen would ignore anyway (F139). Source-level here
	// (a source-wide grant has no chat context).
	if g.denied != nil && g.denied(source, "", uid) {
		return false, errUserDenied
	}
	changed, err := g.write(source, uid)
	if err != nil {
		return false, err
	}
	if err := g.reload(); err != nil {
		// The uid is on disk but the live policy is stale — surface the failure
		// (the admit redeem stays Transient and keeps the token; the operator
		// fixes the broken yaml, or a re-save in the webui policy editor retries).
		return changed, fmt.Errorf("gate: allowlisted %s/%s but live reload failed: %w", source, uid, err)
	}
	// Forget on the already-present path too: a sender allowlisted out-of-band
	// (webui yaml editor, hand edit) never touched the feed, so their ghost row
	// would otherwise survive the panel row's own "allow" button forever.
	g.rejected.ForgetUser(source, uid)
	return changed, nil
}

// AllowInChat adds uid to source's groups[chatID].allowlist_add (the per-chat
// UNCONDITIONAL grant Authorized() honors), reloads, and forgets the rejected record.
// For a group-minted admit token whose frozen scope is that chat. Already-present
// is not a terminal success — reload + forget still run (same rationale as Allow).
func (g *granter) AllowInChat(_ context.Context, source, chatID, uid string) (bool, error) {
	// Denylist wins (F139): refuse if the applicant is denied source-wide OR in
	// this chat, rather than write a contradictory per-chat allowlist_add row.
	if g.denied != nil && g.denied(source, chatID, uid) {
		return false, errUserDenied
	}
	changed, err := g.writeInChat(source, chatID, uid)
	if err != nil {
		return false, err
	}
	if err := g.reload(); err != nil {
		return changed, fmt.Errorf("gate: allowlisted %s/%s in chat %s but live reload failed: %w", source, uid, chatID, err)
	}
	// A per-chat grant forgets only THIS chat's feed row — not the user's rows in
	// other chats where they remain unauthorized (L-gate-D1).
	g.rejected.ForgetUserInChat(source, chatID, uid)
	return changed, nil
}

// writeInChat read-modify-writes groups[<chatID>].allowlist_add in <source>.yaml.
// Same atomicity + serialization + surgical-node discipline as write().
func (g *granter) writeInChat(source, chatID, uid string) (bool, error) {
	if strings.TrimSpace(uid) == "" {
		return false, fmt.Errorf("gate: empty user id")
	}
	if strings.TrimSpace(chatID) == "" {
		return false, fmt.Errorf("gate: empty chat id")
	}
	if !sourceNameRe.MatchString(source) {
		return false, fmt.Errorf("gate: invalid source name %q", source)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	path := filepath.Join(g.dir, source+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("gate: read %s: %w", path, err)
	}
	if err := guardTopKeys(raw); err != nil {
		return false, fmt.Errorf("gate: %s: %w", path, err)
	}
	out, changed, err := patchGroupAllowlistAdd(raw, chatID, uid)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return false, fmt.Errorf("gate: mkdir %s: %w", g.dir, err)
	}
	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return false, fmt.Errorf("gate: write %s: %w", path, err)
	}
	return true, nil
}

// write read-modify-writes <dir>/<source>.yaml, adding uid to the allowlist.
// Returns changed=false when uid was already present (no write performed).
// Atomic (tmp + rename); serialized by g.mu.
//
// The patch is SURGICAL (yaml.Node level), not a round-trip through the typed
// Policy: re-marshaling Policy would silently drop anything the structs don't
// model — in particular per-group keys under `groups:` (the Group struct has no
// catch-all). Node surgery rewrites ONLY the target allowlist sequence and leaves
// the rest of the document — nested groups, unmodeled keys, comments — verbatim (B-8).
func (g *granter) write(source, uid string) (bool, error) {
	if strings.TrimSpace(uid) == "" {
		return false, fmt.Errorf("gate: empty user id")
	}
	if !sourceNameRe.MatchString(source) {
		return false, fmt.Errorf("gate: invalid source name %q", source)
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	path := filepath.Join(g.dir, source+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("gate: read %s: %w", path, err)
	}
	if err := guardTopKeys(raw); err != nil {
		return false, fmt.Errorf("gate: %s: %w", path, err)
	}
	out, changed, err := patchPolicyList(raw, "allowlist", uid)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return false, fmt.Errorf("gate: mkdir %s: %w", g.dir, err)
	}
	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return false, fmt.Errorf("gate: write %s: %w", path, err)
	}
	return true, nil
}

// WriteWhole replaces <source>.yaml with body wholesale, under g.mu — so the webui
// panel's full-file save serializes with the surgical write() path (/gate allow's
// RMW). Without the shared lock a panel save and a concurrent allow could clobber
// each other (atomic rename only stops temp-file corruption, NOT a lost update). The
// caller validates body first and reloads after (mirrors Allow's write-then-reload
// split, so reload never runs under the lock). Best-effort .bak before the write.
func (g *granter) WriteWhole(source string, body []byte) error {
	if !sourceNameRe.MatchString(source) {
		return fmt.Errorf("gate: invalid source name %q", source)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	path := filepath.Join(g.dir, source+".yaml")
	if b, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", b, 0o600) // best-effort backup
	}
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, body, 0o600)
}

// writeFileAtomic writes data to path via a tmp file + rename so a crash mid-write
// never leaves a truncated policy file. The parent dir must already exist. Both the
// granter's surgical write() and its WriteWhole (the webui editor's save) go through
// this AND hold g.mu, so writes to one <source>.yaml are fully serialized; the UNIQUE
// temp name (os.CreateTemp) is the extra guard against two writers colliding on a temp.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename below has moved it away
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// guardTopKeys fails loud on a top-level key this tool does not model, naming it —
// a typo'd key ('allowlst:') would otherwise silently fall to the closed default.
// Surgery (below) preserves unmodeled keys, but a top-level typo is still operator
// error worth surfacing. A missing/empty file is fine (closed default).
func guardTopKeys(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var top map[string]yaml.Node
	if yaml.Unmarshal(raw, &top) != nil {
		return nil // a parse error surfaces in patchPolicyList with full context
	}
	for k := range top {
		switch k {
		case "allow_all", "allowlist", "denylist", "admins", "groups", "onboarding":
		default:
			return fmt.Errorf("carries a top-level field this tool does not model (%q); edit it by hand", k)
		}
	}
	return nil
}

// patchPolicyList surgically adds uid to the top-level `key` sequence of a policy
// yaml document, preserving everything else verbatim. Returns the new bytes and
// whether anything changed.
func patchPolicyList(raw []byte, key, uid string) ([]byte, bool, error) {
	var doc yaml.Node
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, false, fmt.Errorf("gate: parse policy: %w", err)
		}
	}
	if doc.Kind == 0 { // empty / missing file → start a fresh document + mapping root
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("gate: policy yaml root is not a mapping")
	}
	root := doc.Content[0]

	var seq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			seq = root.Content[i+1]
			break
		}
	}
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	// The user-controlled uid VALUE is double-quoted so a uid that happens to be a YAML
	// literal (e.g. "null"/"~"/"true"/a bare number) round-trips back as the exact STRING
	// on the next load, never a typed/null scalar that would silently drop the entry. The
	// structural `key` stays bare. (Mirrors patchGroupAllowlistAdd's qscalar.)
	qscalar := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle}
	}

	if seq == nil {
		root.Content = append(root.Content, scalar(key),
			&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{qscalar(uid)}})
		return marshalDoc(&doc)
	}
	// A blank `allowlist:` line is a null SCALAR, not a sequence — appending to its
	// Content would be silently ignored by Marshal (changed=true, uid never lands).
	// Upgrade it to an empty sequence in place; any other non-sequence is an error.
	if seq.Kind == yaml.ScalarNode && seq.Tag == "!!null" {
		*seq = yaml.Node{Kind: yaml.SequenceNode}
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("gate: `%s` is not a sequence", key)
	}
	for _, it := range seq.Content {
		if idEqual(it.Value, uid) { // @-entries case-insensitive, like IDList.Contains
			return raw, false, nil // already present
		}
	}
	seq.Content = append(seq.Content, qscalar(uid))
	return marshalDoc(&doc)
}

// patchGroupAllowlistAdd surgically adds uid to groups[<chatID>].allowlist_add in a
// policy yaml, creating the groups mapping / the per-chat sub-mapping / the
// allowlist_add sequence as needed, and preserving everything else verbatim (B-8,
// same node-surgery rationale as patchPolicyList).
func patchGroupAllowlistAdd(raw []byte, chatID, uid string) ([]byte, bool, error) {
	var doc yaml.Node
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, false, fmt.Errorf("gate: parse policy: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("gate: policy yaml root is not a mapping")
	}
	root := doc.Content[0]
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	// User-controlled values (chatID, uid) are double-quoted so a value that happens to
	// be a YAML literal (e.g. "null"/"~"/"true"/a bare number) round-trips back as the
	// exact STRING on the next LoadAll, never a typed/null key that would silently drop
	// the grant. The fixed structural keys (groups/allowlist_add) are safe bare.
	qscalar := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle}
	}

	groups := childByKey(root, "groups")
	if groups == nil {
		groups = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, scalar("groups"), groups)
	}
	if groups.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("gate: `groups` is not a mapping")
	}
	chat := childByKey(groups, chatID)
	if chat == nil {
		chat = &yaml.Node{Kind: yaml.MappingNode}
		groups.Content = append(groups.Content, qscalar(chatID), chat)
	}
	if chat.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("gate: groups[%q] is not a mapping", chatID)
	}
	seq := childByKey(chat, "allowlist_add")
	if seq == nil {
		chat.Content = append(chat.Content, scalar("allowlist_add"),
			&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{qscalar(uid)}})
		return marshalDoc(&doc)
	}
	// A blank `allowlist_add:` line is a null scalar — upgrade it in place (same
	// rationale as patchPolicyList).
	if seq.Kind == yaml.ScalarNode && seq.Tag == "!!null" {
		*seq = yaml.Node{Kind: yaml.SequenceNode}
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("gate: groups[%q].allowlist_add is not a sequence", chatID)
	}
	for _, it := range seq.Content {
		if idEqual(it.Value, uid) { // @-entries case-insensitive, like IDList.Contains
			return raw, false, nil // already present
		}
	}
	seq.Content = append(seq.Content, qscalar(uid))
	return marshalDoc(&doc)
}

// childByKey returns the value node for key in a mapping node, or nil.
func childByKey(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func marshalDoc(doc *yaml.Node) ([]byte, bool, error) {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, false, fmt.Errorf("gate: marshal policy: %w", err)
	}
	return out, true, nil
}

var _ contract.AccessGranter = (*granter)(nil)
