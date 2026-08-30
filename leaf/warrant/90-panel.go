package warrant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"agentbob/contract"

	"gopkg.in/yaml.v3"
)

// panel is the capability authority's self-description for the webui
// (docs/webui-trunk.md). It surfaces the live policy matrix — broken/ok health,
// principal count, total grants, and a capability→grantees table — so an admin can
// audit who may use what without grepping permissions.yaml. AdminOnly: the matrix
// is sensitive and is redacted from /api/state until authenticated.
//
// The matrix table is read-only (audit-at-a-glance); the editable surface is the
// "permissions.yaml" code setting below, which edits the SAME file an admin would
// hand-edit + /permission reload. This is the global single-tenant policy the
// normal flow runs on, mirroring agora's per-company perms editor (90-panel.go).
func (m *Manager) panel() contract.Panel {
	return contract.Panel{
		ID:        "warrant",
		Title:     "Permissions",
		AdminOnly: true,
		Upstreams: []string{"tools", "skills"},
		Settings:  []contract.SettingSpec{{ID: "permissions.yaml", Kind: "code", Label: "permissions.yaml", Lang: "yaml"}},
		State: func(_ context.Context) []contract.StateField {
			p := m.policy.Load()

			pval, pstatus := "ok", "ok"
			if m.policyBroken.Load() {
				pval, pstatus = "broken — deny-all", "down"
			}

			var nprincipals int
			caps := make([]string, 0)
			if p != nil {
				nprincipals = len(p.principals)
				for c := range p.granted {
					caps = append(caps, c)
				}
			}
			sort.Strings(caps)

			grants := 0
			rows := make([][]contract.Cell, 0, len(caps))
			for _, c := range caps {
				who := make([]string, 0)
				for principal, on := range p.granted[c] {
					if on {
						who = append(who, principal)
					}
				}
				sort.Strings(who)
				grants += len(who)
				rows = append(rows, []contract.Cell{{Text: c}, {Text: strings.Join(who, ", ")}})
			}
			return []contract.StateField{
				{Kind: "stat", Label: "policy", Value: pval, Status: pstatus},
				{Kind: "stat", Label: "principals", Value: strconv.Itoa(nprincipals)},
				{Kind: "stat", Label: "grants", Value: strconv.Itoa(grants)},
				{Kind: "table", Label: "matrix", Columns: []string{"capability", "granted to"}, Rows: rows},
			}
		},
		// The global permissions.yaml editor: the same file 12-store-file.go reads and
		// /permission reload re-reads. Read echoes the raw file (empty when absent —
		// reconcile seeds it on save); Apply validates, persists, then reloads through
		// the IDENTICAL path as the slash reload so the edit takes effect live.
		Read: func(_ context.Context, sid string) (string, error) {
			if sid != "permissions.yaml" {
				return "", fmt.Errorf("warrant: unknown setting %q", sid)
			}
			b, err := os.ReadFile(policyPath(m.home))
			if os.IsNotExist(err) {
				return "", nil // no policy yet — an empty editor is correct; save seeds it
			}
			// Scrub the $BOB_HOME-rooted path out of any other read error (info hygiene).
			return string(b), scrubRoot(err, m.home)
		},
		Apply: func(_ context.Context, sid, body string) contract.WriteResult {
			if sid != "permissions.yaml" {
				return contract.WriteResult{Phase: "validate", Error: "unknown setting"}
			}
			// Validate STRICTLY before it touches disk. A plain yaml.Unmarshal is too
			// lax for a one-click LIVE policy swap: a typo'd top-level key (`grant:` for
			// `grants:`, `principls:` …) or a cleared editor parses into an EMPTY matrix
			// with no error, and Apply would then reconcile deny-all + diff-cut every
			// principal's channels — wiping all grants instance-wide, reported as success.
			// KnownFields(true) rejects the typo'd key; the emptiness guard rejects the
			// cleared editor. To deliberately revoke everything, hand-edit + /permission reload.
			var pf policyFile
			dec := yaml.NewDecoder(strings.NewReader(body))
			dec.KnownFields(true)
			switch err := dec.Decode(&pf); {
			case errors.Is(err, io.EOF): // empty document → caught by the emptiness guard below
			case err != nil:
				return contract.WriteResult{Phase: "validate", Error: err.Error()}
			}
			if len(pf.Principals) == 0 && len(pf.Grants) == 0 {
				return contract.WriteResult{Phase: "validate", Error: "策略解析为空(疑似键名拼写错误或内容被清空)——已拒绝以防误清空授权"}
			}
			// Persist the admin's RAW text (comments + ordering preserved — this IS the
			// file they would hand-edit) then reload via the SAME reloadPolicy as
			// /permission reload: reconcile new tool/skill rows, atomic-swap the live
			// matrix, diff-cut revoked principals' pooled channels. Live now, no restart.
			// (reloadPolicy re-serializes the file only when reconcile adds rows — same
			// as the slash path; steady-state saves keep the admin's exact text.)
			// Hold writeMu across BOTH the raw write and the reload so a concurrent
			// /permission reload can't slip between them and clobber this save (the
			// lost-update race). reloadPolicyLocked runs the reload body under the held lock.
			m.writeMu.Lock()
			defer m.writeMu.Unlock()
			if err := newFileStore(m.home).SaveRaw([]byte(body)); err != nil {
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			if _, err := m.reloadPolicyLocked(); err != nil {
				return contract.WriteResult{Phase: "reload", Error: err.Error()}
			}
			return contract.WriteResult{Success: true, Phase: "reload"}
		},
	}
}
