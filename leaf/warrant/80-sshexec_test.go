package warrant

import (
	"context"
	"strings"
	"testing"
)

type fakeBroker struct {
	ret any
	err error
}

func (b fakeBroker) Build(context.Context, string) (any, error)            { return b.ret, b.err }
func (b fakeBroker) NamesByKind(context.Context, string) ([]string, error) { return nil, nil }

// TestRemoteRouting: a space declared ssh routes through the credential gate +
// broker; the local path is unaffected. (The real ssh dial needs a server — here
// we cover routing, the credential:use gate, and the broker/sftp guards.)
func TestRemoteRouting(t *testing.T) {
	ctx := context.Background()
	spaces := &spaceConfig{spaces: map[string]spaceBackend{
		"prod": {Backend: "ssh", Credential: "prod-ssh"},
	}}
	pol := fromFile(policyFile{
		Principals: []string{"admin", "user"},
		Grants:     map[string]map[string]string{"credential:use:prod-ssh": {"admin": "on", "user": "off"}},
	})
	m := &Manager{home: t.TempDir(), spaces: spaces}
	m.policy.Store(pol)

	// File on an ssh space → terminal-only by design (fs never speaks ssh).
	if _, err := m.File(ctx, "admin", "prod"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("File(ssh) = %v, want the terminal-only refusal", err)
	}
	// Exec on ssh as a principal without the credential grant → denied.
	if _, err := m.Exec(ctx, "user", "prod"); err == nil {
		t.Error("user without credential:use:prod-ssh must be denied")
	}
	// Granted but no broker wired → reports the broker is unavailable.
	if _, err := m.Exec(ctx, "admin", "prod"); err == nil || !strings.Contains(err.Error(), "broker") {
		t.Errorf("nil broker = %v, want a broker-unavailable error", err)
	}
	// Granted, broker returns a non-ssh value → typed-assert guard fires.
	m.broker = fakeBroker{ret: "not-a-client"}
	if _, err := m.Exec(ctx, "admin", "prod"); err == nil || !strings.Contains(err.Error(), "not an ssh") {
		t.Errorf("non-ssh build = %v, want a not-an-ssh-credential error", err)
	}
	// A local space is unaffected (admin runs a real local shell).
	if _, err := m.Exec(ctx, "admin", "scratch"); err != nil {
		t.Errorf("local exec should still work: %v", err)
	}
}
