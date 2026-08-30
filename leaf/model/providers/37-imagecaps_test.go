package providers

import (
	"os"
	"path/filepath"
	"testing"
)

// The override path is what makes "retune a workflow without a rebuild" true, so
// it is worth proving it actually replaces a built-in.
func TestOverrideReplacesBuiltin(t *testing.T) {
	base, _ := lookupImageCap("comfyui-klein")
	t.Cleanup(func() { LoadImageCapabilityOverrides(""); storeImageAssets(loadBuiltinImageAssets()) })

	dir := t.TempDir()
	write(t, filepath.Join(dir, "comfyui-klein.yaml"), `
unet:   {node: UnetLoaderGGUF, file: other.gguf}
clip:   {node: CLIPLoaderGGUF, file: enc.gguf, type: flux2}
vae:    v.safetensors
latent: EmptyLatentImage
sampling: {steps: 8, cfg: 1.0, sampler: euler, scheduler: simple}
negative: ""
sizes: {square: [512, 512]}
timeout_s: 30
denoise: {moderate: 0.5}
summary: 覆盖用
guide: flux2-klein
`)
	LoadImageCapabilityOverrides(dir)
	got, ok := lookupImageCap("comfyui-klein")
	if !ok {
		t.Fatal("style disappeared after an override")
	}
	if got.Sampler.Steps != 8 || got.UNet.File != "other.gguf" {
		t.Errorf("override did not take effect: %+v", got)
	}
	if base.Sampler.Steps == 8 {
		t.Fatal("test is vacuous — the built-in already had 8 steps")
	}
	// Styles the override did not mention must survive.
	if _, ok := lookupImageCap("comfyui-anima"); !ok {
		t.Error("an override for one style removed another")
	}
}

// Removing an override file must put the built-in back. Merging onto the LIVE
// table instead of the built-in baseline would make an override immortal: delete
// the file, reload, and the old values keep applying with nothing on disk to
// explain them.
func TestRemovingAnOverrideRestoresTheBuiltin(t *testing.T) {
	want, _ := lookupImageCap("comfyui-klein")
	t.Cleanup(func() { storeImageAssets(loadBuiltinImageAssets()) })

	dir := t.TempDir()
	path := filepath.Join(dir, "comfyui-klein.yaml")
	write(t, path, `
unet:   {node: UnetLoaderGGUF, file: other.gguf}
clip:   {node: CLIPLoaderGGUF, file: enc.gguf, type: flux2}
vae:    v.safetensors
latent: EmptyLatentImage
sampling: {steps: 99, cfg: 1.0, sampler: euler, scheduler: simple}
negative: ""
sizes: {square: [512, 512]}
timeout_s: 30
denoise: {moderate: 0.5}
summary: 覆盖用
guide: flux2-klein
`)
	LoadImageCapabilityOverrides(dir)
	if got, _ := lookupImageCap("comfyui-klein"); got.Sampler.Steps != 99 {
		t.Fatalf("override never applied (%d steps)", got.Sampler.Steps)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	LoadImageCapabilityOverrides(dir)
	if got, _ := lookupImageCap("comfyui-klein"); got.Sampler.Steps != want.Sampler.Steps {
		t.Errorf("steps = %d after removing the override, want the built-in's %d", got.Sampler.Steps, want.Sampler.Steps)
	}
}

// One bad file must not take the built-ins down with it.
func TestMalformedOverrideIsSkipped(t *testing.T) {
	t.Cleanup(func() { storeImageAssets(loadBuiltinImageAssets()) })
	dir := t.TempDir()
	write(t, filepath.Join(dir, "comfyui-klein.yaml"), "unet: {node: X}\n") // fails validate
	write(t, filepath.Join(dir, "notes.txt"), "not a declaration")
	LoadImageCapabilityOverrides(dir)
	got, ok := lookupImageCap("comfyui-klein")
	if !ok {
		t.Fatal("a malformed override removed the built-in")
	}
	if got.UNet.Node != "UnetLoaderGGUF" {
		t.Errorf("a malformed override was applied anyway: %+v", got.UNet)
	}
}

// A missing directory is the resting state and must be silent; an absent path
// must not disturb the loaded table.
func TestAbsentOverrideDirIsHarmless(t *testing.T) {
	before := len(imageCapStyles())
	LoadImageCapabilityOverrides(filepath.Join(t.TempDir(), "nope"))
	LoadImageCapabilityOverrides("")
	if after := len(imageCapStyles()); after != before {
		t.Errorf("style count %d → %d across no-op loads", before, after)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
