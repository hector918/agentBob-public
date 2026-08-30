package providers

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"agentbob/contract"
)

// An image capability is "what to draw", declared ONCE and shared by every
// backend that can draw it. It is keyed by the STYLE name — the same string that
// appears as a tag in models.yaml and as a key in the image_create tool's guides.
//
// Why not on the pool entry: a ComfyUI instance is not "the klein engine", it is a
// GPU running a generic renderer. Both deployed containers mount the same model
// directory and can each load every model the other can (verified) — which model
// runs is decided entirely by the workflow submitted. Attaching the workflow to
// the ENTRY therefore encoded a fiction, and it denormalised the data: a second
// instance serving the same style had to copy the whole definition, and changing
// a workflow meant editing every entry that used it.
//
// With the capability keyed by style, an entry declares only WHERE it is and
// WHICH capabilities it can serve. Adding a second renderer for an existing style
// is a base_url and a tag; retagging an instance makes it draw something else
// with no other change.
//
// The pool's tag matching already routes requests to entries carrying the style,
// so every client holds the whole table and simply looks up what it was asked for.

// imageCapsFS holds the built-in capability declarations.
//
//go:embed all:imagecaps
var imageCapsFS embed.FS

// imageCap declares one style: which nodes to build the graph from, what to load
// into them, and the sampling parameters that make this style what it is.
//
// It is a DECLARATION, not a graph. The two deployed engines differ in exactly
// four places — the unet loader class, the clip loader class + type, the empty
// latent class, and the file names — while sharing every other node, both text
// encoders, the sampler, the tiled decode and the whole img2img chain. Storing
// 160 lines of JSON per engine to express four differences was the wrong
// representation; the graph is synthesised per call from this instead
// (docs/image-create-tool.md §5.3).
type imageCap struct {
	UNet    imageNodeRef `yaml:"unet"`
	CLIP    imageNodeRef `yaml:"clip"`
	VAE     string       `yaml:"vae"`
	Latent  string       `yaml:"latent"` // EmptyLatentImage / EmptySD3LatentImage / …
	Sampler imageSampler `yaml:"sampling"`
	// Negative is fixed per style, never a caller parameter: a distilled engine at
	// CFG 1.0 ignores it entirely, so exposing it would be a control that silently
	// does nothing (docs/image-create-tool.md §3.3).
	Negative string `yaml:"negative"`
	// Sizes / TimeoutS / Denoise are DEFAULTS. The first two are really properties
	// of the card the renderer sits on (a 960² ceiling is the 1070's 8 GB talking,
	// not the model's), so a pool entry may override them per instance.
	Sizes    map[string][]int   `yaml:"sizes"`
	TimeoutS int                `yaml:"timeout_s"`
	Denoise  map[string]float64 `yaml:"denoise"`

	// Outward metadata — what a CALLER needs to choose this style, as opposed to
	// what the provider needs to run it. Two consumers read it through
	// contract.ImageCatalog: the image_create tool (to offer styles in a chat) and
	// modelgate (to describe them to an external caller). It lives here, beside the
	// declaration, so there is one copy to keep true.
	Summary string `yaml:"summary"`
	ETA     string `yaml:"eta"`
	Note    string `yaml:"note"`
	// Guide names the prompt manual in imagecaps/guides/<name>.md. SEVERAL styles
	// may name the same one: the tiers of a model family share their prompt rules
	// and are chosen by comparing them, which only works if they are in one document.
	Guide string `yaml:"guide"`
}

// imageNodeRef is a loader: which ComfyUI node class, which file it loads, plus
// any class-specific inputs (UNETLoader wants weight_dtype, UnetLoaderGGUF does
// not).
type imageNodeRef struct {
	Node   string         `yaml:"node"`
	File   string         `yaml:"file"`
	Type   string         `yaml:"type"`   // CLIP loaders only
	Inputs map[string]any `yaml:"inputs"` // extra class-specific inputs
}

type imageSampler struct {
	Steps     int     `yaml:"steps"`
	CFG       float64 `yaml:"cfg"`
	Sampler   string  `yaml:"sampler"`
	Scheduler string  `yaml:"scheduler"`
}

// validate rejects a declaration that could not produce a runnable graph. Caught
// at load time so a broken capability is a startup complaint rather than a failed
// generation minutes into someone's request.
func (c imageCap) validate() error {
	switch {
	case c.UNet.Node == "" || c.UNet.File == "":
		return fmt.Errorf("unet: both node and file are required")
	case c.CLIP.Node == "" || c.CLIP.File == "":
		return fmt.Errorf("clip: both node and file are required")
	case c.VAE == "":
		return fmt.Errorf("vae is required")
	case c.Latent == "":
		return fmt.Errorf("latent node class is required")
	case c.Sampler.Steps <= 0:
		return fmt.Errorf("sampling.steps must be > 0")
	case c.Sampler.Sampler == "":
		return fmt.Errorf("sampling.sampler is required")
	// sizes and denoise ARE required, even though each has a fallback downstream —
	// because an override REPLACES a declaration wholesale rather than merging into
	// it. Someone retuning a step count by writing a short override would otherwise
	// pass validation and then quietly lose both: every text-to-image would drop to
	// the 512² last-resort (pictures just get worse, nothing logged) and every edit
	// would fail with "declares no edit strength". Catching it here is the whole
	// point of validating at load time.
	case len(c.Sizes) == 0:
		return fmt.Errorf("sizes is required (at least a square)")
	case len(c.Denoise) == 0:
		return fmt.Errorf("denoise is required (img2img has no edit strengths without it)")
	case c.TimeoutS <= 0:
		return fmt.Errorf("timeout_s is required")
	// Outward metadata is required for the same reason as the rest: a style with no
	// summary renders a blank line in every caller's catalog, and one with no guide
	// answers "how do I write for this" with nothing. Both degrade silently.
	case strings.TrimSpace(c.Summary) == "":
		return fmt.Errorf("summary is required (it is the catalog line every caller sees)")
	case strings.TrimSpace(c.Guide) == "":
		return fmt.Errorf("guide is required (name a manual in guides/<name>.md)")
	}
	if wh := c.Sizes["square"]; len(wh) != 2 || wh[0] <= 0 || wh[1] <= 0 {
		return fmt.Errorf("sizes.square is required and must be [w, h]")
	}
	return nil
}

// imageGuides holds the prompt manuals, keyed by FILE stem (not by style —
// several styles share one). Swapped together with the capability table.
var imageGuides atomic.Pointer[map[string]string]

// imageCaps is the live capability table (style → declaration). Both are swapped
// atomically so an override reload never tears a concurrent lookup.
var imageCaps atomic.Pointer[map[string]imageCap]

func init() { storeImageAssets(loadBuiltinImageAssets()) }

func storeImageAssets(caps map[string]imageCap, guides map[string]string) {
	imageCaps.Store(&caps)
	imageGuides.Store(&guides)
}

// loadBuiltinImageAssets reads the embedded declarations and the manuals they
// point at. Built-ins ship inside the binary, so anything wrong with them is a
// build mistake and panics immediately rather than surfacing on someone's first
// picture.
func loadBuiltinImageAssets() (map[string]imageCap, map[string]string) {
	caps := map[string]imageCap{}
	entries, err := fs.ReadDir(imageCapsFS, "imagecaps")
	if err != nil {
		panic("providers: image capability directory missing from embed: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		style, cap, err := parseImageCap(func() ([]byte, error) {
			return fs.ReadFile(imageCapsFS, "imagecaps/"+e.Name())
		}, e.Name())
		if err != nil {
			panic("providers: " + err.Error())
		}
		if style != "" {
			caps[style] = cap
		}
	}
	guides := map[string]string{}
	gentries, err := fs.ReadDir(imageCapsFS, "imagecaps/guides")
	if err == nil {
		for _, e := range gentries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			raw, rerr := fs.ReadFile(imageCapsFS, "imagecaps/guides/"+e.Name())
			if rerr != nil {
				panic("providers: unreadable image guide " + e.Name() + ": " + rerr.Error())
			}
			guides[strings.TrimSuffix(e.Name(), ".md")] = string(raw)
		}
	}
	// A declaration pointing at a manual that does not exist would advertise a
	// style whose "how do I write for this" answer is empty — catch it at build
	// time, where it is a typo, rather than at use time, where it is a mystery.
	for style, c := range caps {
		if c.Guide != "" && guides[c.Guide] == "" {
			panic("providers: image capability " + style + " names guide " + c.Guide + ", which does not exist")
		}
	}
	return caps, guides
}

// LoadImageCapabilityOverrides overlays declarations (and manuals, from a guides/
// subdirectory) from dir onto the built-in set, mirroring how the skills module
// takes an external directory beside its embedded one. It lets an operator retune
// a workflow — or rewrite prompt guidance, which is the part that gets tuned most
// — without a rebuild.
//
// Called at model-module start AND on every /model reload, so adjusting either is
// the same gesture as adjusting models.yaml. A missing directory is the normal
// case and is silent; a malformed file is skipped with a WARN so one bad override
// cannot take the built-ins down with it.
func LoadImageCapabilityOverrides(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		// An ABSENT directory is the normal case and says nothing. Anything else —
		// wrong permissions inside the container, a stale mount, a typo in the path —
		// is a silent loss of every override the operator thought they had installed.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("providers: image capability override directory is unreadable", "dir", dir, "err", err)
		}
		return
	}
	// Rebuild from the BUILT-IN baseline every time, never from the live table:
	// starting from what is currently loaded would make an override immortal —
	// delete the file, reload, and the old values would still be in effect with
	// nothing on disk to explain them.
	caps, guides := loadBuiltinImageAssets()

	n := 0
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		path := filepath.Join(dir, f.Name())
		style, cap, perr := parseImageCap(func() ([]byte, error) { return os.ReadFile(path) }, f.Name())
		if perr != nil {
			slog.Warn("providers: ignoring bad image capability override", "file", path, "err", perr)
			continue
		}
		if style == "" {
			continue
		}
		caps[style] = cap
		n++
	}
	// Deferred to after the guides load below: an override may bring its own manual.
	if gfiles, gerr := os.ReadDir(filepath.Join(dir, "guides")); gerr == nil {
		for _, f := range gfiles {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(dir, "guides", f.Name()))
			if rerr != nil {
				slog.Warn("providers: unreadable image guide override", "file", f.Name(), "err", rerr)
				continue
			}
			guides[strings.TrimSuffix(f.Name(), ".md")] = string(raw)
			n++
		}
	}
	// A declaration naming a manual that does not exist would advertise a style
	// whose "how do I write for this" answer is empty — exactly the silent
	// degradation this whole catalog exists to prevent. Built-ins panic on it; an
	// override cannot take the process down, so it is dropped with a WARN, which
	// leaves the built-in (or nothing) rather than a half-usable style.
	for style, c := range caps {
		if c.Guide != "" && strings.TrimSpace(guides[c.Guide]) == "" {
			slog.Warn("providers: dropping image capability whose guide is missing",
				"style", style, "guide", c.Guide, "dir", dir)
			delete(caps, style)
		}
	}
	if n > 0 {
		slog.Info("providers: image capability overrides loaded", "dir", dir, "count", n)
	}
	// Store unconditionally: n==0 is a meaningful state too (every override was
	// removed), and it must put the built-ins back rather than leave the last
	// successful overlay standing.
	storeImageAssets(caps, guides)
}

// parseImageCap reads one declaration. The STYLE is the filename stem, so the
// same string keys the models.yaml tag, this file, and what a caller asks for.
func parseImageCap(read func() ([]byte, error), filename string) (string, imageCap, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".yaml" && ext != ".yml" {
		return "", imageCap{}, nil // not a declaration; ignore quietly
	}
	raw, err := read()
	if err != nil {
		return "", imageCap{}, fmt.Errorf("unreadable image capability %s: %w", filename, err)
	}
	var cap imageCap
	if err := yaml.Unmarshal(raw, &cap); err != nil {
		return "", imageCap{}, fmt.Errorf("malformed image capability %s: %w", filename, err)
	}
	if err := cap.validate(); err != nil {
		return "", imageCap{}, fmt.Errorf("image capability %s: %w", filename, err)
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename)), cap, nil
}

// lookupImageCap returns the declaration for a style.
func lookupImageCap(style string) (imageCap, bool) {
	c, ok := (*imageCaps.Load())[style]
	return c, ok
}

// imageCapStyles lists every declared style (for diagnostics).
func imageCapStyles() []string {
	var out []string
	for k := range *imageCaps.Load() {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ImageCatalog is the contract.ImageCatalog implementation the model module
// provides. It is a value type with no state: every read goes through the atomic
// tables above, so a reload is visible to every holder at once.
type ImageCatalog struct{}

// ImageStyles lists what can be drawn, sorted by style name.
func (ImageCatalog) ImageStyles() []contract.ImageStyleInfo {
	caps := *imageCaps.Load()
	out := make([]contract.ImageStyleInfo, 0, len(caps))
	for style, c := range caps {
		out = append(out, contract.ImageStyleInfo{
			Style: style, Summary: c.Summary, ETA: c.ETA, Note: c.Note, Guide: c.Guide,
			// COPY, never the live map: consumers hold this past the call, and the
			// atomic swap only keeps a reload from tearing a read — it does not make
			// the table safe to hand out by reference.
			Sizes:   maps.Clone(c.Sizes),
			Changes: sortedDenoiseKeys(c.Denoise),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Style < out[j].Style })
	return out
}

// sortedDenoiseKeys lists a style's edit strengths weakest first, so a caller
// reading the list can tell which end is which.
func sortedDenoiseKeys(d map[string]float64) []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return d[out[i]] < d[out[j]] })
	return out
}

// ImageGuide returns a style's prompt manual, resolving the declaration's guide
// reference. Styles sharing a manual resolve to the same text.
func (ImageCatalog) ImageGuide(style string) (string, bool) {
	c, ok := (*imageCaps.Load())[style]
	if !ok || c.Guide == "" {
		return "", false
	}
	body, ok := (*imageGuides.Load())[c.Guide]
	return body, ok && strings.TrimSpace(body) != ""
}
