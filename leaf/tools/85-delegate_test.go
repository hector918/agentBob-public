package tools

import (
	"context"
	"encoding/json"
	"testing"

	"agentbob/contract"
)

// fakeSub records the SubSpec it received and returns a canned result.
type fakeSub struct {
	got contract.SubSpec
	res contract.SubResult
}

func (f *fakeSub) RunSub(_ context.Context, spec contract.SubSpec) contract.SubResult {
	f.got = spec
	return f.res
}

func TestDelegate(t *testing.T) {
	d := delegate{}

	// Happy path: a known suite → the suite's tools/guide are passed and the product
	// comes back.
	sub := &fakeSub{res: contract.SubResult{Product: "做完了"}}
	r := d.Run(context.Background(), contract.ToolContext{Sub: sub},
		json.RawMessage(`{"task":"读配置并总结","suite":"files"}`))
	if !r.OK || r.Data != "做完了" {
		t.Fatalf("delegate result = %+v, want OK 做完了", r)
	}
	if len(sub.got.Suite) != 2 || sub.got.Guide == "" || sub.got.Task != "读配置并总结" {
		t.Errorf("sub spec not built from the suite: %+v", sub.got)
	}

	// Partial product → a hint is attached.
	sub2 := &fakeSub{res: contract.SubResult{Product: "半成品", Partial: true}}
	if r := d.Run(context.Background(), contract.ToolContext{Sub: sub2},
		json.RawMessage(`{"task":"x","suite":"files"}`)); r.Hint == "" {
		t.Error("a partial product should carry a hint")
	}

	// Unknown suite → error (model can't pass a free tool list).
	if r := d.Run(context.Background(), contract.ToolContext{Sub: sub},
		json.RawMessage(`{"task":"x","suite":"ghost"}`)); r.OK {
		t.Error("an unknown suite must error")
	}

	// No sub-runner wired (or inside a sub) → graceful error, not a nil panic.
	if r := d.Run(context.Background(), contract.ToolContext{},
		json.RawMessage(`{"task":"x","suite":"files"}`)); r.OK {
		t.Error("nil Sub must error, not delegate")
	}

	// Empty task → error.
	if r := d.Run(context.Background(), contract.ToolContext{Sub: sub},
		json.RawMessage(`{"suite":"files"}`)); r.OK {
		t.Error("an empty task must error")
	}
}
