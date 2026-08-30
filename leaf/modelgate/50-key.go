package modelgate

import (
	"net/http"
	"slices"

	"agentbob/contract"
)

// This file is the bob-specific runtime-introspection endpoint GET /v1/key. It lets
// a key holder read, from their own script, WHAT their key can do (its policy) and
// the live RUNTIME state of what it can reach (health, context window, the tags
// worth preferring) — so they can size requests and know what to ask for. It
// deliberately exposes NO billing/usage and NO internal identity (backend model id,
// provider, priority, error strings): only what a caller needs to make good requests.

// honoredParams / ignoredParams describe, once, which request fields this gateway
// acts on vs. silently drops. `kind` / `requires` / `prefer` are bob extensions (not
// OpenAI fields) carrying the routing address, spelled exactly as the picker spells
// it; everything else honored is OpenAI's. It is a property of the endpoint, not of
// any single model, so it rides the top level.
var honoredParams = []string{"kind", "requires", "prefer", "model", "messages", "stream", "tools"}
var ignoredParams = []string{"temperature", "top_p", "n", "stop", "max_tokens", "presence_penalty", "frequency_penalty", "tool_choice", "response_format", "logprobs", "seed"}

// keyInfoResponse is the GET /v1/key body: the key's own policy, the reachable
// models' runtime state, and the endpoint's honored/ignored parameter lists.
type keyInfoResponse struct {
	Key    keyPolicy      `json:"key"`
	Models []modelRuntime `json:"models"`
	Params paramSupport   `json:"params"`
}

type keyPolicy struct {
	ID      string   `json:"id"`
	Account string   `json:"account"`
	Kinds   []string `json:"kinds,omitempty"`  // lane form: the lanes `kind` may name
	Models  []string `json:"models,omitempty"` // pin form: the entry names `model` may pin
}

// modelRuntime is one SELECTABLE thing's runtime state — for a lane-form key that is
// a lane, for a pin-form key one entry. Name is therefore whatever the caller may
// address.
//
// A lane row does NOT carry a lane-wide context_window: a kind-level minimum is the
// smallest window ANY backend in the lane declares, which one utility model drags
// down for everybody (nv-nano's 10240 vs the chat models' 131072) and which no
// caller can act on. Capability is reported per TAG instead — that is the grain a
// caller actually requires on, and with `requires` being hard the number is a
// guarantee. A lane whose backends carry no tags at all has nothing to break down,
// so it reports its own window directly.
type modelRuntime struct {
	Name          string       `json:"name"`
	State         string       `json:"state"`                    // "live" | "tentative" | "cooling" | "paused" | "unavailable" (lane with no backend)
	ContextWindow int          `json:"context_window,omitempty"` // tag-less lanes / pinned entries only; see tags[] otherwise
	Tags          []tagRuntime `json:"tags,omitempty"`           // per-tag capability: what `requires` may name, and what each guarantees
}

// tagRuntime is one requirable tag's runtime state within a lane: the state of the
// best backend carrying it, and the SMALLEST window among them (any of them may take
// the request, so sizing to the smallest always fits).
type tagRuntime struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	ContextWindow int    `json:"context_window"`
}

type paramSupport struct {
	Honored []string `json:"honored"`
	Ignored []string `json:"ignored"`
}

// stateRank orders backend states so an aggregate can report the BEST one — that is
// what a request would land on.
func stateRank(state string) int {
	switch state {
	case "live":
		return 3
	case "tentative":
		return 2
	case "cooling":
		return 1
	default: // paused / anything unknown
		return 0
	}
}

// aggregate folds a set of backends into (best state, smallest window). Empty set →
// "unavailable" with window 0.
func aggregate(entries []contract.ModelInfo) (state string, window int) {
	state, best := "unavailable", -1
	for _, e := range entries {
		if r := stateRank(e.State); r > best {
			best, state = r, e.State
		}
		if window == 0 || (e.ContextWindow > 0 && e.ContextWindow < window) {
			window = e.ContextWindow
		}
	}
	return state, window
}

// laneRuntime renders one lane row: its own state, plus a per-TAG capability
// breakdown. The lane-wide window is deliberately absent when the lane has tags —
// see modelRuntime. A lane whose backends carry no tags reports its window directly,
// since there is nothing to break down.
func laneRuntime(kind string, lane []contract.ModelInfo) modelRuntime {
	out := modelRuntime{Name: kind}
	out.State, _ = aggregate(lane)

	for _, tag := range laneTags(lane) {
		var carrying []contract.ModelInfo
		for _, e := range lane {
			if slices.Contains(e.Tags, tag) {
				carrying = append(carrying, e)
			}
		}
		st, win := aggregate(carrying)
		out.Tags = append(out.Tags, tagRuntime{Name: tag, State: st, ContextWindow: win})
	}
	if len(out.Tags) == 0 {
		_, out.ContextWindow = aggregate(lane)
	}
	return out
}

// handleKeyInfo serves GET /v1/key.
func (s *server) handleKeyInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	info, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	snap := s.pool.Snapshot()

	models := make([]modelRuntime, 0)
	if keyIsLaneForm(info) {
		// One row per lane the key admits. This is the ROUTING-INTROSPECTION view and
		// it deliberately differs from GET /v1/models, which lists capabilities and no
		// lanes at all (docs/modelgate-tags.md §5): a lane is bob's internal
		// partitioning, and the only place it is still worth showing is here, next to
		// the per-tag windows a caller sizes its requests by.
		for _, kind := range selectableModels(info, snap) {
			models = append(models, laneRuntime(kind, entriesInLane(kind, snap)))
		}
	} else {
		for _, e := range laneEntries(info, snap) {
			// No tags[] here: pinning bypasses routing, so this caller can't require
			// anything — its window is the entry's own, full stop.
			models = append(models, modelRuntime{
				Name:          e.Name,
				State:         e.State,
				ContextWindow: e.ContextWindow,
			})
		}
	}
	writeJSON(w, keyInfoResponse{
		Key: keyPolicy{
			ID:      info.ID,
			Account: info.AccountID,
			Kinds:   info.Kinds,
			Models:  info.Models,
		},
		Models: models,
		Params: paramSupport{Honored: honoredParams, Ignored: ignoredParams},
	})
}
