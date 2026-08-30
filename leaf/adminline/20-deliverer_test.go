package adminline

import (
	"testing"

	"agentbob/contract"
)

// TestNewDeliverer covers the outlet-spec parsing paths that don't need a live
// source: unattached, agora (deferred → error), bad format, unknown source.
func TestNewDeliverer(t *testing.T) {
	nilByName := func(string) contract.Source { return nil }

	if d, err := NewDeliverer("", nilByName); d != nil || err != nil {
		t.Errorf(`"" → (%v, %v), want (nil, nil) unattached`, d, err)
	}
	if _, err := NewDeliverer("agora:inbox-x", nilByName); err == nil {
		t.Errorf("agora outlet should error (deferred)")
	}
	if _, err := NewDeliverer("no-colon", nilByName); err == nil {
		t.Errorf("malformed outlet should error")
	}
	if _, err := NewDeliverer("telegram:", nilByName); err == nil {
		t.Errorf("empty chat id should error")
	}
	if _, err := NewDeliverer("telegram:123", nilByName); err == nil {
		t.Errorf("unknown source should error (not enabled)")
	}
}
