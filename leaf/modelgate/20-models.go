package modelgate

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"agentbob/contract"
)

// handleModels serves GET /v1/models — the capability catalog. What a key may
// SELECT depends on its form: a lane-form key addresses capabilities, so it gets
// TAGS; a models-form key pins by name, so it gets the names it may pin. The pool's
// entry names are bob's internal inventory and never travel outward on the first
// path. Mirrors the OpenAI list shape either way (`data` / `id` / `object` /
// `owned_by` are what a model-dropdown client parses — see buildModelRequest).
//
// Lane names are deliberately NOT listed. A lane (`kind`) partitions by payload
// shape, key admission, queueing and accounting — four things that live entirely
// inside bob. Outside, the only place a lane surfaces is WHICH ENDPOINT you call,
// and the endpoint already says that. What a caller chooses between is capabilities,
// and those are tags. (Requests still accept a lane name for compatibility;
// docs/modelgate-tags.md §5.)
//
// Nothing here describes what a capability is FOR: the catalog answers "what is
// there and can I use it now", and the manual is one hop away at
// GET /v1/models/<id>.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	info, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	snap := s.pool.Snapshot()
	created := time.Now().Unix()

	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		// The one non-standard field, and the one thing a caller cannot work out for
		// itself: whether this is usable RIGHT NOW. Pool vocabulary verbatim, no
		// outward-facing re-spelling — a translation layer would be one more model to
		// learn. "live" and "tentative" are usable; see docs/modelgate-tags.md §2.
		State string `json:"state"`
	}
	// Data is initialized, not nil: a key that reaches nothing must render as
	// "data": [] rather than "data": null — an SDK iterating the field would blow up
	// on null, and a policy-less key (every pre-v8 row) hits exactly that path.
	list := struct {
		Object string     `json:"object"`
		Data   []modelObj `json:"data"`
	}{Object: "list", Data: []modelObj{}}

	if keyIsLaneForm(info) {
		// Image styles need no special case any more: a style IS a tag (models.yaml
		// spells it `tags: [comfyui-klein]`), so it arrives through the same door as
		// `vision` or `novel-write`.
		for _, t := range s.addressableTags(info, snap) {
			list.Data = append(list.Data, modelObj{
				ID: t.Tag, Object: "model", Created: created, OwnedBy: "bob", State: t.State,
			})
		}
	} else {
		for _, e := range laneEntries(info, snap) {
			list.Data = append(list.Data, modelObj{
				ID: e.Name, Object: "model", Created: created, OwnedBy: "bob", State: e.State,
			})
		}
	}
	writeJSON(w, list)
}

// handleModel serves GET /v1/models/<id>. For an image style it carries the FULL
// prompt manual — the reason this whole catalog is shared rather than embedded in
// the tool: an external caller writes its own prompts and needs the same guidance
// the in-conversation tool reads, from the same single copy.
func (s *server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	info, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if id == "" {
		s.handleModels(w, r)
		return
	}
	snap := s.pool.Snapshot()
	var tags []tagState
	if keyIsLaneForm(info) {
		// Capabilities are the lane-form vocabulary only. A pin-form key addresses
		// entry NAMES, and answering 200 here for a tag its own listing omits (and its
		// chat requests reject) would be the same catalog-vs-endpoint contradiction
		// this endpoint exists to remove.
		tags = s.addressableTags(info, snap)
	}

	// A style's manual, for a key that admits the lane. NOT gated on the renderer
	// being up: a manual is worth reading while its GPU cools, and the catalog
	// already says whether it can be drawn right now.
	if keyAdmitsImages(info) {
		if st, found := findStyle(s.imageStyles(), id); found {
			out := map[string]any{
				"id": st.Style, "object": "model", "created": time.Now().Unix(), "owned_by": "bob",
				"state": stateOf(tags, st.Style),
				// The full prompt manual is the reason this catalog is shared rather
				// than embedded in the image_create tool: an external caller writes its
				// own prompts and needs the same guidance, from the same single copy.
				"description": st.Summary, "eta": st.ETA, "sizes": sizeStrings(st),
			}
			if st.Note != "" {
				out["note"] = st.Note
			}
			out["changes"] = st.Changes
			if cat := s.imageCatalog(); cat != nil {
				if guide, has := cat.ImageGuide(st.Style); has {
					out["guide"] = guide
				}
			}
			writeJSON(w, out)
			return
		}
	}
	// Any other capability: its name and whether it is usable. No prose is invented
	// for a tag that has no manual — silence beats a generated description.
	if t, found := findTagState(tags, id); found {
		writeJSON(w, map[string]any{
			"id": t.Tag, "object": "model", "created": time.Now().Unix(), "owned_by": "bob",
			"state": t.State,
		})
		return
	}
	// Lane names remain addressable though unlisted (docs/modelgate-tags.md §5).
	if slices.Contains(selectableModels(info, snap), id) {
		writeJSON(w, map[string]any{
			"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "bob",
		})
		return
	}
	writeError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("unknown model %q", id), "model_not_found")
}

// sizeStrings renders a style's accepted sizes as "aspect (WxH)".
func sizeStrings(st contract.ImageStyleInfo) []string {
	out := make([]string, 0, len(st.Sizes))
	for aspect, wh := range st.Sizes {
		if len(wh) == 2 {
			out = append(out, fmt.Sprintf("%s (%dx%d)", aspect, wh[0], wh[1]))
		}
	}
	sort.Strings(out)
	return out
}

// tagState is one addressable capability and the runtime state of the best backend
// carrying it.
type tagState struct {
	Tag   string
	State string
}

// addressableTags is the capability catalog of a lane-form key: every tag carried by
// an entry it can reach, with the state of the best backend carrying it (any of them
// may take the request, so the best is what a request would land on).
//
// An entry carrying NO tags is unreachable from outside, and that is the intended
// reading rather than a gap to guard: outside traffic addresses capabilities, and an
// untagged entry declares none. Nothing validates it, because there is nothing to
// catch — the entry is simply not on offer.
func (s *server) addressableTags(info contract.APIKeyInfo, snap contract.PoolSnapshot) []tagState {
	reachable := laneEntries(info, snap)
	// Tags are a general routing facility, and an image entry may legitimately carry
	// operational ones (fallback hints, capability marks) that are not styles at all.
	// Only a DECLARED style is drawable, so an undeclared image tag would be offered
	// as a capability that /v1/images/generations then refuses as unknown — the two
	// endpoints pointing at each other over a name this catalog invented. The
	// in-conversation catalog filters the same way and for the same reason
	// (leaf/tools/imagecreate/20-catalog.go::liveCapabilities).
	declaredStyles := map[string]bool{}
	for _, st := range s.imageStyles() {
		declaredStyles[strings.ToLower(st.Style)] = true
	}

	out := make([]tagState, 0, len(reachable))
	for _, tag := range laneTags(reachable) {
		var carrying []contract.ModelInfo
		imageOnly := true
		for _, e := range reachable {
			if !slices.Contains(e.Tags, tag) {
				continue
			}
			carrying = append(carrying, e)
			if !strings.EqualFold(e.Kind, contract.KindImage) {
				imageOnly = false
			}
		}
		if imageOnly && !declaredStyles[strings.ToLower(tag)] {
			continue
		}
		st, _ := aggregate(carrying)
		out = append(out, tagState{Tag: tag, State: st})
	}
	return out
}

// tagLanes reports which lanes carry a tag, among the entries this key can reach.
// Empty → not a capability of this key. One → the lane a `model: <tag>` request
// routes into, which is the whole reason `kind` never has to be spelled outside.
// More than one → ambiguous, and the caller must refuse rather than pick: guessing
// would silently route a request into a different payload shape, queue and bill.
// No tag spans two lanes today; this exists so that the day one does, it says so.
func tagLanes(info contract.APIKeyInfo, snap contract.PoolSnapshot, tag string) []string {
	var out []string
	for _, e := range laneEntries(info, snap) {
		if slices.Contains(e.Tags, tag) && !slices.Contains(out, e.Kind) {
			out = append(out, e.Kind)
		}
	}
	slices.Sort(out)
	return out
}

// stateOf is findTagState's answer for a name known to exist elsewhere (a declared
// style, say): "unavailable" when nothing in the pool carries it, which is exactly
// what a declaration with no matching models.yaml tag amounts to.
func stateOf(tags []tagState, name string) string {
	if t, ok := findTagState(tags, name); ok {
		return t.State
	}
	return "unavailable"
}

// findTagState looks a capability up in a catalog. Case-insensitive to be forgiving
// at the door; callers route by the canonical spelling they get back, since tag
// matching in the picker is exact.
func findTagState(tags []tagState, name string) (tagState, bool) {
	for _, t := range tags {
		if strings.EqualFold(t.Tag, name) {
			return t, true
		}
	}
	return tagState{}, false
}

// selectableModels is the LANE vocabulary of a key: a lane-form key's lane names
// (what its `kind` may be, and what a `model`-only client may still put there as a
// shorthand); a models-form key's reachable entry names. The single truth behind
// GET /v1/key and the chat resolver's lane check.
//
// It is no longer what GET /v1/models lists for a lane-form key — that lists
// capabilities now (addressableTags). Lane names stay ACCEPTED but unlisted:
// docs/modelgate-tags.md §5.
func selectableModels(info contract.APIKeyInfo, snap contract.PoolSnapshot) []string {
	if keyIsLaneForm(info) {
		// Deduped: a lane list is a set, and a repeated lane would list (and later
		// report runtime for) the same capability twice.
		out := make([]string, 0, len(info.Kinds))
		for _, k := range info.Kinds {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
		return out
	}
	var out []string
	for _, e := range laneEntries(info, snap) {
		out = append(out, e.Name)
	}
	return out
}

// keyIsLaneForm reports whether the key routes by lane (vs. an explicit entry-name
// list). Models takes precedence when both are somehow set (the mint path forbids
// that).
func keyIsLaneForm(info contract.APIKeyInfo) bool {
	return len(info.Models) == 0 && len(info.Kinds) > 0
}

// laneEntries returns the pool entries this key can reach AT ALL: not disabled and
// admitted by its policy — for a models-form key the entries it names, for a
// lane-form key every entry in one of its lanes. Cooling/paused stay in (transient —
// the pick surfaces the pool's own error).
//
// The lane fence lives here because the pool does not apply it on the pinned path
// (contract.ModelRequest: "the pinned path does not check Kind") — modelgate is the
// only place a key's lane admission can be enforced.
func laneEntries(info contract.APIKeyInfo, snap contract.PoolSnapshot) []contract.ModelInfo {
	var out []contract.ModelInfo
	for _, e := range snap.Entries {
		if e.State == "disabled" {
			continue
		}
		if keyAdmits(info, e) {
			out = append(out, e)
		}
	}
	return out
}

// entriesInLane is the not-disabled entries of one kind — the candidate set a
// lane-form request actually routes within.
func entriesInLane(kind string, snap contract.PoolSnapshot) []contract.ModelInfo {
	var out []contract.ModelInfo
	for _, e := range snap.Entries {
		if e.Kind == kind && e.State != "disabled" {
			out = append(out, e)
		}
	}
	return out
}

// keyAdmits reports whether the key's policy admits this entry.
func keyAdmits(info contract.APIKeyInfo, e contract.ModelInfo) bool {
	if len(info.Models) > 0 {
		return slices.Contains(info.Models, e.Name)
	}
	if len(info.Kinds) > 0 {
		return slices.Contains(info.Kinds, e.Kind)
	}
	return false // no policy → reaches nothing (fail-closed)
}

// allowsModelName reports whether the key may pin a specific entry by name — not
// disabled and named by its allowlist.
func (s *server) allowsModelName(info contract.APIKeyInfo, name string, snap contract.PoolSnapshot) bool {
	for _, e := range laneEntries(info, snap) {
		if e.Name == name {
			return true
		}
	}
	return false
}

// laneTags is the union of the tags carried by a lane's entries — the vocabulary a
// caller may put in `requires` / `prefer`. Sorted for a stable response body.
func laneTags(lane []contract.ModelInfo) []string {
	var out []string
	for _, e := range lane {
		for _, t := range e.Tags {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	slices.Sort(out)
	return out
}

// hasAllTags reports whether have ⊇ need — the same conjunction the picker applies
// to Requires, so the gateway's diagnosis counts the candidates the picker counted.
func hasAllTags(have, need []string) bool {
	for _, n := range need {
		if !slices.Contains(have, n) {
			return false
		}
	}
	return true
}
