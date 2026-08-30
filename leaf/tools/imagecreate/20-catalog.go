package imagecreate

import (
	"fmt"
	"sort"
	"strings"

	"agentbob/contract"
)

// drawableState reports whether a pool entry in this state would actually take a
// request. It is the one rule both halves of the image surface answer by — this
// catalog and modelgate's (leaf/modelgate/35-images.go::drawableStyles) — because
// they answer the same question and once disagreed: a tentative GPU made a style
// drawable over HTTP and invisible in conversation.
func drawableState(state string) bool { return state == "live" || state == "tentative" }

// capability is one offerable style: a usable pool entry's tag, joined with what the
// image catalog says about it.
type capability struct {
	Style   string // the entry tag == the model-facing `style` value
	Summary string
	ETA     string
	Note    string
	Guide   string // grouping key: styles sharing it are alternatives to each other
}

// liveCapabilities is the ONE place that answers "what can be drawn right now".
//
// A style exists iff BOTH halves agree: the pool has a live image entry carrying
// that tag (can it run?) and the catalog declares that style (do we know how to
// drive it, and how should a prompt for it be written?). Neither half is
// hard-coded in Go, and neither names a backend — which is what makes adding or
// swapping a backend a models.yaml change with no rebuild.
//
// The catalog half used to be an embed inside this package. It moved to the model
// side because it has a SECOND consumer — modelgate hands the same guidance to
// external callers — and two leaf modules cannot import each other, so the one
// copy has to live below both (docs/image-create-tool.md §4).
//
// Entries that are cooling / paused are deliberately EXCLUDED: the catalog answers
// "what can I do for you now", and offering a style whose backend is in a cooldown
// produces a failure the model already promised the user.
//
// "tentative" is INCLUDED, and used not to be. A tentative entry has been re-admitted
// by the heartbeat and IS pick-eligible — it is simply unproven by a real request, so
// the pool would happily route to it. Excluding it made a flapping GPU look like a
// style that does not exist, which is a different and worse claim than "busy". It
// also split this catalog from modelgate's, which widened first
// (docs/modelgate-tags.md §7): the same GPU state made a style drawable over HTTP and
// invisible in conversation. Both halves answer the same question and must use the
// same rule (hector).
func liveCapabilities(pool contract.ModelPool, cat contract.ImageCatalog) []capability {
	if pool == nil || cat == nil {
		return nil
	}
	declared := map[string]contract.ImageStyleInfo{}
	for _, s := range cat.ImageStyles() {
		declared[s.Style] = s
	}
	var out []capability
	seen := map[string]bool{}
	for _, e := range pool.Snapshot().Entries {
		if !strings.EqualFold(e.Kind, contract.KindImage) || !drawableState(e.State) {
			continue
		}
		for _, tag := range e.Tags {
			// The catalog is the FILTER as well as the annotation. Tags are a general
			// routing facility — an image entry may legitimately carry operational
			// ones (fallback hints, capability marks) that are not画风 at all, and
			// offering those as styles would put words in the catalog that mean
			// nothing to a user and route to no declaration.
			d, ok := declared[tag]
			if !ok || seen[tag] {
				continue
			}
			seen[tag] = true // two live entries may serve one style; offer it once
			out = append(out, capability{Style: tag, Summary: d.Summary, ETA: d.ETA, Note: d.Note, Guide: d.Guide})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Style < out[j].Style })
	return out
}

// undocumentedStyles lists tags on live image entries that no declaration covers
// — a deployment mistake (a backend shipped without its declaration) worth one
// WARN at use time rather than silence. Operational tags land here too, which is
// why it is a log line and not an error.
func undocumentedStyles(pool contract.ModelPool, cat contract.ImageCatalog) []string {
	if pool == nil || cat == nil {
		return nil
	}
	declared := map[string]bool{}
	if cat != nil {
		for _, s := range cat.ImageStyles() {
			declared[s.Style] = true
		}
	}
	var out []string
	for _, e := range pool.Snapshot().Entries {
		if !strings.EqualFold(e.Kind, contract.KindImage) || !drawableState(e.State) {
			continue
		}
		for _, tag := range e.Tags {
			if !declared[tag] {
				out = append(out, e.Name+":"+tag)
			}
		}
	}
	return out
}

// find returns the capability for a style name.
func find(caps []capability, style string) (capability, bool) {
	for _, c := range caps {
		if strings.EqualFold(c.Style, style) {
			return c, true
		}
	}
	return capability{}, false
}

// renderCatalog is the DISCOVERY level: one line per DIALECT, listing its styles
// with their costs, so the model can pick — and can tell the user what a slow
// choice will cost — without pulling any full manual.
//
// Grouped rather than one line per style, because the styles sharing a manual are
// alternatives to each other: seeing "fast 10s / good 5min" on one line is what
// makes the trade-off visible, and listing them apart would repeat the same
// summary once per tier.
func renderCatalog(caps []capability) string {
	byGuide := map[string][]capability{}
	var order []string
	for _, c := range caps {
		key := c.Guide
		if key == "" {
			key = c.Style // undocumented grouping → stands alone
		}
		if _, seen := byGuide[key]; !seen {
			order = append(order, key)
		}
		byGuide[key] = append(byGuide[key], c)
	}
	sort.Strings(order)

	var b strings.Builder
	b.WriteString("当前可用的风格：\n")
	for _, key := range order {
		list := byGuide[key]
		names := make([]string, 0, len(list))
		for _, c := range list {
			name := c.Style
			switch {
			case c.ETA != "" && c.Note != "":
				name += fmt.Sprintf("（%s，%s）", c.ETA, c.Note)
			case c.ETA != "":
				name += fmt.Sprintf("（%s）", c.ETA)
			case c.Note != "":
				name += fmt.Sprintf("（%s）", c.Note)
			}
			names = append(names, name)
		}
		b.WriteString(fmt.Sprintf("- %s —— %s\n", strings.Join(names, " / "), list[0].Summary))
	}
	// The three rules ride the CATALOG, not just the guides, because the guides are
	// optional by design and a model that skips them still has to not trip over
	// these. Measured: with the dialect rules only in the guides, prompts came back
	// in Chinese half the time; lifted here, that went to zero — but lifting them
	// ALSO made the model stop pulling the guides at all, which is why the
	// picture-text rule (its own silent failure) had to come along, and why the
	// closing line now names what is still only answerable there
	// (docs/image-create-tool.md §4.7).
	//
	// ⚠️ Rule 1 states a fact about the ENGINES currently in service, from a place
	// that is supposed to know nothing about them (§4.4: adding a model is one yaml
	// plus one guide, no Go). It holds because both shipped engines are
	// English-only, and each style's own dialect already rides its yaml `summary`.
	// The day a style eats another language, this line becomes a lie told on that
	// style's behalf — move it into the declaration then, do not special-case it here.
	b.WriteString("\n三条铁律：\n" +
		"1. 提示词一律用英文 —— 中文不报错，但会画出跟要求毫不相干的东西。\n" +
		"2. 别指望画面里出现文字 —— 中文完全画不出，英文也只有很短的全大写才勉强稳，" +
		"而且这个失败不报错，你只会拿到一张带乱码的图。要「图 + 文案」就让它只画图、文字后期叠上去，并把这一点告诉用户。\n" +
		"3. 改图时（填了 init_image）：先用 image 工具（task=answer）看一眼原图 —— 你只拿到了文件名，看不见内容；" +
		"没有那个工具就让用户描述一句。别用 task=reverse_prompt，那个会把结果直接甩给用户并结束这一轮，图就画不成了。" +
		"提示词要描述最终画面、并把原图里要保留的东西写全，不要写「把这张改成…」这类指令。\n")
	b.WriteString("\n把选中的风格填进 style 再调一次即可出图。" +
		"要挑档位、要精修、或者拿不准怎么写，就只填 style、不要填 prompt，会返回那个引擎的完整说明书。")
	return b.String()
}

// styleNames is the flat list used in error messages.
func styleNames(caps []capability) string {
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		names = append(names, c.Style)
	}
	return strings.Join(names, " / ")
}
