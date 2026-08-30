package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"agentbob/contract"

	"gopkg.in/yaml.v3"
)

// allowAction builds the /gate allow command the rejected-row "allow" button prefills,
// scoped to MATCH where the sender was rejected — mirroring grantFrozen so a one-click
// admit grants no broader than the token path. A GROUP sender (with a chat id) gets the
// per-chat form (/gate allow <source> <chat> <uid> → that chat's allowlist_add), so we
// don't over-grant them source-wide across every chat on the source; a DM (or a row with
// no chat id) gets the source-level form.
func allowAction(source, chatID string, ct contract.ChatType, uid string) string {
	if ct.IsGroupChat() && chatID != "" {
		return "/gate allow " + source + " " + chatID + " " + uid
	}
	return "/gate allow " + source + " " + uid
}

// clipScope shortens a scope string for the cell's display Text (the full scope
// rides along in Cell.Copy); ~24 chars keeps the column narrow.
func clipScope(s string) string {
	const max = 24
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// panel is the gate's self-description for the webui. It surfaces the rejected-
// senders log (the bootstrap aid for discovering ids to allow) AND a per-source
// policy-yaml editor (one `code` setting per source) — editing a source's
// allowlist/denylist/admins/groups and saving HOT-RELOADS the live policy
// (Module.Reload). To allowlist a rejected sender: open that source's editor, add
// the shown user id under `allowlist:`, save. AdminOnly throughout.
func (m *Module) panel() contract.Panel {
	// One editor per source that already has a loaded policy file. Sorted.
	names := m.scr.sourceNames()
	sort.Strings(names)
	settings := make([]contract.SettingSpec, 0, len(names))
	for _, name := range names {
		settings = append(settings, contract.SettingSpec{
			ID: name, Kind: "code", Label: name + ".yaml", Lang: "yaml",
		})
	}
	return contract.Panel{
		ID:        "gate",
		Title:     "Gate",
		AdminOnly: true,
		Settings:  settings,
		State: func(_ context.Context) []contract.StateField {
			rej := m.Rejected().List()
			rows := make([][]contract.Cell, 0, len(rej))
			for _, r := range rej {
				scope := contract.ScopeFor(r.Source, r.ChatType, r.ChatID, "")
				// The one-click "allow" prefills /gate allow for the admin to review + Enter,
				// scoped to MATCH where the sender was rejected (allowAction).
				allow := allowAction(r.Source, r.ChatID, r.ChatType, r.UserID)
				rows = append(rows, []contract.Cell{
					{Text: r.Source},
					{Text: r.ChatID},
					// the scope cell carries a "copy" affordance → copies the full chat
					// scope to the clipboard for pasting into /agora inbox wire.
					{Text: clipScope(scope), Copy: scope},
					// the user cell carries a one-click "allow" action (see `allow` above).
					{Text: r.UserID, Action: allow, ActionLabel: "allow"},
					{Text: r.UserName},
					{Text: strconv.Itoa(r.Count)},
				})
			}
			countStatus := ""
			if len(rej) > 0 {
				countStatus = "warn" // there are senders waiting to be allowlisted
			}
			fields := []contract.StateField{
				{Kind: "stat", Label: "rejected senders", Value: strconv.Itoa(len(rej)), Status: countStatus},
				{Kind: "table", Label: "rejected", Columns: []string{"source", "chat", "scope", "user", "name", "count"}, Rows: rows},
			}
			if len(rej) > 0 {
				fields = append(fields, contract.StateField{Kind: "text", Label: "to allow",
					Text: "click a row's 'allow' to let a sender in (prefills /gate allow — review + Enter), or edit the source yaml below. To route a chat to an agora inbox: allow it FIRST (else it's dropped here), then copy the row's scope and run /agora inbox wire <inbox> <scope>."})
			}
			return fields
		},
		Read: func(_ context.Context, sid string) (string, error) {
			// sid comes from the URL path — validate it as a bare source name (no path
			// separators) before joining, same guard granter.write uses, so it can't
			// traverse out of the policy dir.
			if !sourceNameRe.MatchString(sid) {
				return "", fmt.Errorf("gate: invalid source name %q", sid)
			}
			b, err := os.ReadFile(filepath.Join(m.dir, sid+".yaml"))
			if os.IsNotExist(err) {
				return "", nil // a never-edited source has no file yet — empty editor is correct
			}
			return string(b), err
		},
		Apply: func(_ context.Context, sid, body string) contract.WriteResult {
			if !sourceNameRe.MatchString(sid) { // no path traversal (mirrors granter.write)
				return contract.WriteResult{Phase: "validate", Error: "invalid source name"}
			}
			// validate: parseable yaml + only modeled top-level keys (a typo'd key
			// would otherwise silently fall to the closed default).
			if err := guardTopKeys([]byte(body)); err != nil {
				return contract.WriteResult{Phase: "validate", Error: err.Error()}
			}
			// Validate by unmarshaling into the TYPED Policy (the same struct LoadAll uses),
			// not a loose map: a type-mismatched value (e.g. a scalar where an IDList is
			// expected) passes a map probe, gets committed, then bricks the NEXT boot when
			// LoadAll fails (gate is non-Optional → StateFailed aborts trunk Start).
			var p Policy
			if err := yaml.Unmarshal([]byte(body), &p); err != nil {
				return contract.WriteResult{Phase: "validate", Error: err.Error()}
			}
			// Write under the granter's g.mu so a full-file panel save serializes
			// with a concurrent /gate allow RMW (no lost update). Reload after, off
			// the lock (mirrors Allow's write-then-reload split).
			if err := m.gr.WriteWhole(sid, []byte(body)); err != nil {
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			if err := m.Reload(); err != nil { // hot-swap the live policy
				return contract.WriteResult{Phase: "reload", Error: err.Error()}
			}
			return contract.WriteResult{Success: true, Phase: "reload"}
		},
	}
}
