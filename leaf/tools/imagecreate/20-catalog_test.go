package imagecreate

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
)

// fakePool serves a fixed snapshot; only Snapshot is exercised by the catalog.
type fakePool struct{ entries []contract.ModelInfo }

func (p *fakePool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil
}
func (p *fakePool) ChatStreamWatch(context.Context, contract.ModelRequest, []contract.Message, contract.StreamWatcher) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil
}
func (p *fakePool) Snapshot() contract.PoolSnapshot { return contract.PoolSnapshot{Entries: p.entries} }
func (p *fakePool) FlushUsage(context.Context)      {}
func (p *fakePool) FlushUsageFinal(context.Context) {}
func (p *fakePool) Close()                          {}

func imageEntry(name, state string, tags ...string) contract.ModelInfo {
	return contract.ModelInfo{Name: name, Kind: contract.KindImage, State: state, Tags: tags}
}

// A style exists only where BOTH halves agree: the pool can run it and a guide
// says how to drive it. Either half alone must not produce an offer.
func TestLiveCapabilitiesIsAnIntersection(t *testing.T) {
	pool := &fakePool{entries: []contract.ModelInfo{
		imageEntry("comfy-klein", "live", "comfyui-klein"),                                      // both halves → offered
		imageEntry("comfy-mystery", "live", "surreal"),                                          // live, no guide → not offered
		imageEntry("comfy-anima", "cooling", "comfyui-anima"),                                   // guide, not live → not offered
		{Name: "smart", Kind: contract.KindLLM, State: "live", Tags: []string{"comfyui-klein"}}, // wrong kind
	}}
	caps := liveCapabilities(pool, testCatalog())
	if len(caps) != 1 {
		t.Fatalf("got %d capabilities %+v, want exactly photo", len(caps), caps)
	}
	if caps[0].Style != "comfyui-klein" || caps[0].Summary == "" {
		t.Errorf("got %+v, want photo on comfy-klein", caps[0])
	}
}

// tentative is pick-eligible: the heartbeat re-admitted the entry, so the pool would
// route to it. Excluding it made a flapping GPU look like a style that does not
// exist — and split this catalog from modelgate's, which draws by the same rule
// (docs/modelgate-tags.md §7). cooling and paused stay out: those genuinely will not
// take the request.
func TestLiveCapabilitiesIncludesTentativeButNotCooling(t *testing.T) {
	pool := &fakePool{entries: []contract.ModelInfo{
		imageEntry("comfy-klein", "tentative", "comfyui-klein"),
		imageEntry("comfy-anima", "cooling", "comfyui-anima"),
	}}
	caps := liveCapabilities(pool, testCatalog())
	if len(caps) != 1 || caps[0].Style != "comfyui-klein" {
		t.Fatalf("got %+v, want only the tentative style offered", caps)
	}
}

// One entry may carry several styles (the tiers of one backend), and all of them
// must surface — that is what lets the model discuss the trade-off.
func TestLiveCapabilitiesExpandsAllTagsOfAnEntry(t *testing.T) {
	pool := &fakePool{entries: []contract.ModelInfo{imageEntry("comfy-anima", "live", "comfyui-anima", "comfyui-anima-hq")}}
	caps := liveCapabilities(pool, testCatalog())
	if len(caps) != 2 {
		t.Fatalf("got %d capabilities, want both tiers of the entry", len(caps))
	}
	for _, c := range caps {
		if c.ETA == "" {
			t.Errorf("style %q has no ETA — the catalog could not warn about a slow tier", c.Style)
		}
	}
}

// A deployed-but-undocumented backend is skipped, but must be reportable: silent
// omission looks identical to the backend being down.
func TestUndocumentedEntriesAreReported(t *testing.T) {
	pool := &fakePool{entries: []contract.ModelInfo{imageEntry("comfy-mystery", "live", "surreal")}}
	if got := liveCapabilities(pool, testCatalog()); len(got) != 0 {
		t.Errorf("undocumented entry was offered: %+v", got)
	}
	bare := undocumentedStyles(pool, testCatalog())
	if len(bare) != 1 || bare[0] != "comfy-mystery:surreal" {
		t.Errorf("undocumentedStyles = %v, want [comfy-mystery:surreal]", bare)
	}
}

func TestLiveCapabilitiesHandlesNilPool(t *testing.T) {
	if got := liveCapabilities(nil, testCatalog()); got != nil {
		t.Errorf("liveCapabilities(nil, testCatalog()) = %v, want nil", got)
	}
}

// The catalog groups by backend so alternatives read as alternatives, and carries
// each tier's cost so the model can quote it before committing the user to a wait.
func TestRenderCatalogGroupsByEngineAndShowsCost(t *testing.T) {
	pool := &fakePool{entries: []contract.ModelInfo{
		imageEntry("comfy-anima", "live", "comfyui-anima", "comfyui-anima-hq"),
		imageEntry("comfy-klein", "live", "comfyui-klein"),
	}}
	out := renderCatalog(liveCapabilities(pool, testCatalog()))
	if n := strings.Count(out, "\n- "); n != 2 {
		t.Errorf("got %d engine lines in:\n%s\nwant one per engine", n, out)
	}
	animaLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "comfyui-anima-hq") {
			animaLine = l
		}
	}
	if animaLine == "" {
		t.Fatalf("no line mentions anime_hq:\n%s", out)
	}
	if !strings.Contains(animaLine, "comfyui-anima（") {
		t.Errorf("fast tier has no cost annotation: %q", animaLine)
	}
	if !strings.Contains(animaLine, "分钟") {
		t.Errorf("slow tier does not advertise its cost: %q", animaLine)
	}
}

func TestFindIsCaseInsensitive(t *testing.T) {
	caps := []capability{{Style: "comfyui-klein"}}
	if _, ok := find(caps, "COMFYUI-Klein"); !ok {
		t.Error("find did not match a differently-cased style")
	}
	if _, ok := find(caps, "comfyui-anima"); ok {
		t.Error("find matched a style that is not offered")
	}
}
