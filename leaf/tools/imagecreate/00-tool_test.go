package imagecreate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

func newTestTool(entries ...contract.ModelInfo) *Tool {
	pool := &fakePool{entries: entries}
	return New(func() contract.ModelPool { return pool }, testCatalog, func() contract.Gateway { return nil }, "")
}

func run(t *testing.T, tl *Tool, args string) contract.ToolResult {
	t.Helper()
	return tl.Run(context.Background(), contract.ToolContext{}, json.RawMessage(args))
}

// Level 1: an omitted style returns the catalog rather than an error. This is what
// makes discovery self-triggering — a model that has never used the tool cannot
// know a style name, so the empty call has to be the useful one.
func TestRunWithoutStyleReturnsCatalog(t *testing.T) {
	tl := newTestTool(imageEntry("comfy-klein", "live", "comfyui-klein"))
	res := run(t, tl, `{}`)
	if !res.OK {
		t.Fatalf("res = %+v, want the catalog", res)
	}
	if !strings.Contains(res.Data, "comfyui-klein") {
		t.Errorf("catalog does not mention the live style:\n%s", res.Data)
	}
}

// Level 2: a style with no prompt returns THAT engine's manual — and only that
// one, so adding backends never inflates the read.
func TestRunWithStyleButNoPromptReturnsThatGuideOnly(t *testing.T) {
	tl := newTestTool(
		imageEntry("comfy-klein", "live", "comfyui-klein"),
		imageEntry("comfy-anima", "live", "comfyui-anima", "comfyui-anima-hq"),
	)
	res := run(t, tl, `{"style":"comfyui-anima"}`)
	if !res.OK {
		t.Fatalf("res = %+v, want a guide", res)
	}
	if res.Data != fakeGuide("comfyui-anima") {
		t.Errorf("did not return comfy-anima's body verbatim")
	}
	if strings.Contains(res.Data, fakeGuide("comfyui-klein")) {
		t.Errorf("the other engine's guide leaked into the answer")
	}
}

// Both tiers of one engine share a guide, so asking about either returns the same
// document — that is what lets the model compare them in one read.
func TestRunReturnsSharedGuideForBothTiers(t *testing.T) {
	tl := newTestTool(imageEntry("comfy-anima", "live", "comfyui-anima", "comfyui-anima-hq"))
	fast := run(t, tl, `{"style":"comfyui-anima"}`)
	slow := run(t, tl, `{"style":"comfyui-anima-hq"}`)
	if fast.Data != slow.Data {
		t.Error("the two tiers of one engine returned different manuals")
	}
}

// An unknown style must name what IS available, so the next call can succeed.
func TestRunRejectsUnknownStyleAndListsAvailable(t *testing.T) {
	tl := newTestTool(imageEntry("comfy-klein", "live", "comfyui-klein"))
	res := run(t, tl, `{"style":"oil_painting","prompt":"x"}`)
	if res.OK {
		t.Fatal("accepted a style that no live backend offers")
	}
	if !strings.Contains(res.Error, "comfyui-klein") {
		t.Errorf("error does not list the available styles: %q", res.Error)
	}
}

// With nothing live the tool must say so plainly rather than half-working.
func TestRunReportsNoBackend(t *testing.T) {
	res := run(t, newTestTool(), `{"style":"comfyui-klein","prompt":"x"}`)
	if res.OK || !strings.Contains(res.Error, "生图后端") {
		t.Errorf("res = %+v, want a clear no-backend error", res)
	}
}

// A `change` with no picture to change would be silently dropped, and a dropped
// parameter reads exactly like an honoured one. Refuse loudly instead — the same
// lesson the image tool's strayFields records.
func TestRunRefusesChangeWithoutInitImage(t *testing.T) {
	tl := newTestTool(imageEntry("comfy-klein", "live", "comfyui-klein"))
	res := run(t, tl, `{"style":"comfyui-klein","prompt":"x","change":"slight"}`)
	if res.OK {
		t.Fatal("accepted change without init_image — it would have been ignored")
	}
	if !strings.Contains(res.Error, "init_image") {
		t.Errorf("error does not explain the fix: %q", res.Error)
	}
}

// Generation needs somewhere to put the picture and something to send it through;
// discovering that AFTER a minute of rendering wastes the wait.
func TestRunRequiresSinkAndSpaceBeforeGenerating(t *testing.T) {
	tl := newTestTool(imageEntry("comfy-klein", "live", "comfyui-klein"))
	res := run(t, tl, `{"style":"comfyui-klein","prompt":"a car"}`)
	if res.OK {
		t.Fatal("started a generation with no sink and no space")
	}
	if !strings.Contains(res.Error, "发送通道") {
		t.Errorf("error = %q, want it to name the missing capability", res.Error)
	}
}

func TestRunRejectsMalformedArgs(t *testing.T) {
	tl := newTestTool(imageEntry("comfy-klein", "live", "comfyui-klein"))
	if res := run(t, tl, `{"style":`); res.OK {
		t.Error("accepted malformed arguments")
	}
}

// The spec is the only thing paid on every request, so its size is a standing
// cost. This is a guard against the drift that made the old image tools 21% of
// the tool budget, not a style rule.
func TestSpecStaysSmall(t *testing.T) {
	spec := newTestTool().Spec()
	size := len(spec.Description) + len(spec.Parameters)
	if size > 2000 {
		t.Errorf("image_create spec is %d bytes — engine-specific detail belongs in the guides, not the spec", size)
	}
	if spec.Name != "image_create" {
		t.Errorf("Name = %q, want image_create", spec.Name)
	}
	// A produced picture is production, and a failed send is claimable as done.
	if !spec.Delivers || !spec.SideEffect {
		t.Errorf("Delivers=%v SideEffect=%v, want both true", spec.Delivers, spec.SideEffect)
	}
	if spec.SelectionHint == nil || spec.SelectionHint.When == "" {
		t.Error("no SelectionHint.When — it is the only always-resident signpost for this tool")
	}
}

// The guides carry style names; the spec must NOT, or adding a backend would mean
// editing Go.
func TestSpecDoesNotHardcodeStyles(t *testing.T) {
	spec := newTestTool().Spec()
	blob := spec.Description + string(spec.Parameters) + spec.SelectionHint.When + spec.SelectionHint.Then
	for _, style := range []string{"comfyui-klein", "comfyui-anima", "comfyui-anima-hq", "klein", "comfy"} {
		if strings.Contains(blob, style) {
			t.Errorf("spec mentions %q — style names come from the pool at runtime, not the schema", style)
		}
	}
}
