package normal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"agentbob/contract"

	"gopkg.in/yaml.v3"
)

// panel is the normal flow's self-description for the webui. The flow is the
// "main layer" that owns the whole-app config.yaml (no leaf has a natural claim
// on it — see docs/webui-trunk.md §四). State is minimal (the flow is active);
// the substance is the config.yaml editor.
//
// NOTE: config.yaml is loaded at boot (sources, etc.); there is no live reload in
// trunk yet, so an edit takes effect on the next restart. The write still
// validates (yaml parse) + backs up so a bad edit can't corrupt the file blind.
func (m *Module) panel() contract.Panel {
	path := filepath.Join(m.home, "config.yaml")
	return contract.Panel{
		ID:        "flow-normal",
		Title:     "Flow: normal",
		AdminOnly: true,
		Tier:      "flow",              // home-graph flow band (策略层)
		Upstreams: []string{"sources"}, // the flow delivers replies out through the gateway (stoma) bus
		Settings:  []contract.SettingSpec{{ID: "config.yaml", Kind: "code", Label: "config.yaml", Lang: "yaml"}},
		State: func(_ context.Context) []contract.StateField {
			return []contract.StateField{{Kind: "stat", Label: "flow", Value: "active", Status: "ok"}}
		},
		Read: func(_ context.Context, sid string) (string, error) {
			if sid != "config.yaml" {
				return "", fmt.Errorf("unknown setting %q", sid)
			}
			b, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				return "", nil // no overrides yet — an empty editor is correct
			}
			return string(b), err
		},
		Apply: func(_ context.Context, sid, body string) contract.WriteResult {
			if sid != "config.yaml" {
				return contract.WriteResult{Phase: "validate", Error: "unknown setting"}
			}
			var probe map[string]any
			if err := yaml.Unmarshal([]byte(body), &probe); err != nil {
				return contract.WriteResult{Phase: "validate", Error: err.Error()}
			}
			// An empty/whitespace-only body parses as a nil map — reject it: saving
			// it (twice) would also clobber the .bak, losing the config permanently.
			if len(probe) == 0 {
				return contract.WriteResult{Phase: "validate", Error: "config.yaml 不能为空"}
			}
			// Back up the current config before overwriting. If the original EXISTS but
			// the backup can't be written, ABORT — overwriting without a good backup would
			// discard the last-good config with no recovery. A missing original (first
			// save) has nothing to back up.
			b, rerr := os.ReadFile(path)
			switch {
			case rerr == nil:
				if err := os.WriteFile(path+".bak", b, 0o600); err != nil {
					return contract.WriteResult{Phase: "write", Error: "backup failed: " + err.Error()}
				}
			case !os.IsNotExist(rerr):
				return contract.WriteResult{Phase: "write", Error: "read for backup failed: " + rerr.Error()}
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			return contract.WriteResult{Success: true, Phase: "write"}
		},
	}
}
