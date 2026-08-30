package modelgate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"agentbob/contract"
)

// handleChat serves POST /v1/chat/completions. It authenticates the key, resolves
// the request's routing address (kind + tags, or a pinned name) against the key's
// policy into a ModelRequest, stamps billing on ctx, and drives the pool — blocking
// (JSON) or streaming (SSE) per the request's stream flag.
//
// There is NO gate here: capacity is the pool's business (per-entry concurrency + a
// per-kind wait queue + busy retry). A door gate could only GUESS the ceiling, while
// the pool knows real occupancy — so saturation surfaces as the pool's own
// contract.ErrModelQueueFull, mapped to 429 by chatErrorStatus.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	info, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	req, ok := decodeChatRequest(w, r)
	if !ok {
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required", "")
		return
	}
	// `kind` / `model` are optional when the key leaves no ambiguity (see
	// buildModelRequest); only `messages` is mandatory.
	snap := s.pool.Snapshot()
	mr, ok := s.buildModelRequest(w, info, req, snap)
	if !ok {
		return
	}

	// Bill the key's account: the pool's per-handle ledger reads the consumer off ctx.
	ctx := ctxWithConsumer(r.Context(), info.ID)
	messages := toPoolMessages(req.Messages)

	// What the reply echoes in `model`. The caller's own spelling, verbatim: a
	// dropdown client matches the reply against the id it picked, and after the
	// catalog switched to capabilities a lane name is an id it has never seen.
	asked := strings.TrimSpace(req.Model)

	if req.Stream {
		s.streamChat(ctx, w, info, mr, asked, messages)
		return
	}
	s.blockChat(ctx, w, info, mr, asked, messages)
}

// buildModelRequest resolves the request's routing address against the key's policy.
// Which fields carry the address depends on the key's FORM:
//
//	Lane-form key: `kind` names the lane (must be one the key admits); `requires` and
//	               `prefer` pass through UNCHANGED to the picker. `kind` omitted →
//	               `model` read as a lane-name shorthand (for clients that only have a
//	               model dropdown), else the key's sole lane; ambiguous → 400.
//	Models-form key: `model` = "<entry name>" → pinned; "" → the sole allowed entry,
//	               if there is exactly one. The routing fields are ignored — pinning
//	               bypasses routing entirely.
//
// `requires` is passed through as a HARD requirement, exactly as an internal caller
// spells it. One mechanism, not two: the admin-declared tag-fallback chain
// (models.yaml `defaults.fallback`) handles graceful degradation, and a chain that
// runs dry fails loudly. Softening it here would route external traffic around the
// pool's own "a queue beats a quiet downgrade" guard, which keys on len(Requires)>0
// (leaf/model/40-chat.go).
//
// snap is the caller's pool snapshot (reused, not re-read). Writes the error +
// returns ok=false when the address can't be resolved.
func (s *server) buildModelRequest(w http.ResponseWriter, info contract.APIKeyInfo, req chatRequest, snap contract.PoolSnapshot) (contract.ModelRequest, bool) {
	want := strings.TrimSpace(req.Model)
	mr := contract.ModelRequest{
		Tools:       toPoolTools(req.Tools),
		AffinityKey: info.ID, // one key ≈ one caller → soft prompt-cache affinity for free
	}

	if keyIsLaneForm(info) {
		lanes := selectableModels(info, snap)
		kind := strings.ToLower(strings.TrimSpace(req.Kind))
		asked := strings.ToLower(want)

		// `model` names a CAPABILITY — a tag off GET /v1/models, which is the only
		// address an external caller has. Resolving which lane carries it is what lets
		// `kind` stay out of every outside vocabulary: the tag name already says what
		// the thing does, and which lane it lives in is bob's business.
		//
		// A lane name in `model` still wins, for the clients that were told to send one
		// (and because `translate` / `asr` / `ocr` name both a lane and its sole tag —
		// same routing either way, so the older reading stays authoritative).
		var askedTag string
		if asked != "" && !slices.Contains(lanes, asked) {
			// Membership is decided by the CATALOG, not by the raw tag union: what
			// /v1/models lists and what this endpoint accepts have to be the same set, or
			// the two contradict each other over some name (an undeclared operational tag
			// on an image entry is exactly such a name).
			if _, listed := findTagState(s.addressableTags(info, snap), asked); !listed {
				writeError(w, http.StatusNotFound, "invalid_request_error",
					fmt.Sprintf("unknown model %q; GET /v1/models lists what this key can address", want),
					"model_not_found")
				return contract.ModelRequest{}, false
			}
			carriers := tagLanes(info, snap, asked)
			switch {
			case len(carriers) == 0: // unreachable via the catalog check above; fail closed anyway
				writeError(w, http.StatusNotFound, "invalid_request_error",
					fmt.Sprintf("unknown model %q; GET /v1/models lists what this key can address", want),
					"model_not_found")
				return contract.ModelRequest{}, false

			// An explicitly-supplied `kind` has to be one the capability actually lives
			// in — whether there is one carrier or several. Honouring a disagreeing one
			// would route into a lane where nothing carries the tag, and the failure
			// arrives as a 503 blaming a requirement the caller never wrote.
			case kind != "" && !slices.Contains(carriers, kind):
				writeError(w, http.StatusBadRequest, "invalid_request_error",
					fmt.Sprintf("%q is not part of kind %q (it is carried by: %s)",
						asked, kind, strings.Join(carriers, ", ")), "")
				return contract.ModelRequest{}, false

			case kind != "":
				askedTag = asked // caller already picked a lane that carries it

			case len(carriers) == 1:
				askedTag, kind = asked, carriers[0]

			default:
				writeError(w, http.StatusBadRequest, "invalid_request_error",
					fmt.Sprintf("%q is carried by more than one lane (%s) — send `kind` to say which",
						asked, strings.Join(carriers, ", ")), "")
				return contract.ModelRequest{}, false
			}
		}

		if kind == "" {
			switch {
			// A model-dropdown-only client (open-webui and friends) picks an id off
			// /v1/models and sends it as `model`; a lane name there is the older form.
			case slices.Contains(lanes, asked):
				kind = asked
			case len(lanes) == 1:
				kind = lanes[0]
			default:
				writeError(w, http.StatusBadRequest, "invalid_request_error",
					fmt.Sprintf("kind is required for this key — it admits more than one lane; selectable: %s",
						selectableText(lanes)), "")
				return contract.ModelRequest{}, false
			}
		}
		if !slices.Contains(lanes, kind) {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("kind %q is not available to this key; selectable: %s",
					kind, selectableText(lanes)), "model_not_found")
			return contract.ModelRequest{}, false
		}
		// The catalog is flat — image capabilities sit next to chat ones — and a client
		// that only speaks chat (a model dropdown) can pick one and send it here. Say
		// where it belongs instead of routing a chat payload at a renderer, which dies
		// downstream as a 503 that names the wrong cause.
		if kind == contract.KindImage {
			msg := "this key's only capabilities draw images — POST /v1/images/generations is the endpoint that takes them"
			if want != "" {
				msg = fmt.Sprintf("%q draws images — send it to POST /v1/images/generations, not this endpoint", want)
			}
			writeError(w, http.StatusBadRequest, "invalid_request_error", msg, "")
			return contract.ModelRequest{}, false
		}
		mr.Kind = kind
		mr.Requires = req.Requires
		// The named capability becomes a HARD requirement, the same one `requires`
		// carries — `model` is a shorthand for it, not a second mechanism.
		if askedTag != "" && !slices.Contains(mr.Requires, askedTag) {
			mr.Requires = append(slices.Clone(mr.Requires), askedTag)
		}
		mr.Prefer = req.Prefer
		return mr, true
	}

	// Models-form: pinning one exact entry IS the point of this form, so Kind/Prefer
	// stay unset (the pool ignores both on the pinned path — setting them would only
	// mislead).
	names := selectableModels(info, snap)
	if want == "" {
		if len(names) == 1 {
			mr.PinnedEntry = names[0]
			return mr, true
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model is required for this key — it pins one entry by name; selectable: %s",
				selectableText(names)), "")
		return contract.ModelRequest{}, false
	}
	if s.allowsModelName(info, want, snap) {
		mr.PinnedEntry = want
		return mr, true
	}
	writeError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("model %q is not available to this key; selectable: %s",
			want, selectableText(names)), "model_not_found")
	return contract.ModelRequest{}, false
}

// laneDiagnosis says WHY this request got no answer, using only what modelgate can
// actually observe: the lane and the health of its entries. It deliberately does NOT
// claim a cause it can't see — a live candidate means the pool reached a backend and
// the REQUEST was refused (an oversized prompt is the common one), so saying "none
// could serve" there would be a confident lie. It counts entries instead of naming
// them: a lane-form caller addresses capabilities, and entry names are bob's
// internal inventory.
//
// It also sanitizes: the pool's own errors name entry names, $BOB_HOME paths and
// admin CLI commands, none of which may travel outward.
func laneDiagnosis(mr contract.ModelRequest, snap contract.PoolSnapshot) string {
	kind := mr.Kind
	if kind == "" {
		kind = contract.KindLLM
	}
	if mr.PinnedEntry != "" {
		return fmt.Sprintf("the pinned entry %q did not answer — it may be down, or it rejected the request itself (an oversized prompt does that; see context_window in GET /v1/key)", mr.PinnedEntry)
	}
	tags := strings.Join(mr.Requires, "+")
	total, matched, serving := 0, 0, 0
	states := map[string]int{}
	for _, e := range entriesInLane(kind, snap) {
		total++
		if hasAllTags(e.Tags, mr.Requires) {
			matched++
			states[e.State]++
			if e.State == "live" || e.State == "tentative" {
				serving++
			}
		}
	}
	switch {
	case total == 0:
		return fmt.Sprintf("no %s backend is configured — ask the admin", kind)
	case matched == 0:
		// The pool consults the admin-declared tag-fallback chain before giving up,
		// so this states the shortfall it can see without claiming nothing was tried.
		return fmt.Sprintf("no %s backend carries the required tag(s) %s (of %d in this lane), and no tag-fallback was configured for them — drop the requirement or ask the admin",
			kind, tags, total)
	case serving == 0:
		return fmt.Sprintf("%d %s backend(s) carry %s but none is serving right now (states: %s) — transient, retry with backoff",
			matched, kind, tags, statesText(states))
	default:
		return fmt.Sprintf("%d %s backend(s) carry %s and %d of them are up (states: %s), so the request itself was refused — most often the prompt exceeds the backend's context_window (see GET /v1/key)",
			matched, kind, tags, serving, statesText(states))
	}
}

// chatErrorStatus maps a pool failure to its HTTP status + message. A full wait
// queue is BUSY, not broken — the backends are alive, there is simply nowhere left
// to stand in line — so it earns a retryable 429 rather than the 503 an outage gets.
// contract.ErrModelQueueFull is cross-layer precisely so a user-facing surface can
// tell the two apart.
func chatErrorStatus(err error, mr contract.ModelRequest, snap contract.PoolSnapshot) (int, string, string) {
	if errors.Is(err, contract.ErrModelQueueFull) {
		return http.StatusTooManyRequests, "rate_limit_exceeded",
			"the model queue is full — the backends are up but saturated; retry with backoff"
	}
	return http.StatusServiceUnavailable, "server_error",
		"no model could serve the request: " + laneDiagnosis(mr, snap)
}

// selectableText renders a "selectable: …" list, or an explicit sentence when the
// list is empty — a caller told to choose from nothing learns nothing. An empty list
// means the key carries no policy at all (a pre-v8 row) or every entry it names is
// gone, and neither is something the caller can fix.
func selectableText(names []string) string {
	if len(names) == 0 {
		return "(nothing — this key currently reaches no lane or entry; ask the admin)"
	}
	return strings.Join(names, ", ")
}

// statesText renders a state histogram deterministically ("cooling×2, paused×1").
func statesText(states map[string]int) string {
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, states[k]))
	}
	return strings.Join(parts, ", ")
}

// echoModel is what the response's `model` field reports. A models-form caller
// pinned a name, so it gets the entry that actually served (its own vocabulary). A
// lane-form caller gets the LANE it selected: telling it which entry ran would hand
// it the inventory names the whole form exists to keep internal.
func echoModel(info contract.APIKeyInfo, mr contract.ModelRequest, asked, served string) string {
	if keyIsLaneForm(info) {
		// Echo the capability the caller named, in their spelling. Falling back to the
		// lane is only for a request that named nothing at all — there is no better
		// answer then, and the backend's own name is internal inventory that never
		// travels outward on this form.
		if asked != "" {
			return asked
		}
		return mr.Kind
	}
	return servedModel(served, mr.PinnedEntry)
}

// blockChat runs a non-streaming completion and renders the OpenAI JSON body.
func (s *server) blockChat(ctx context.Context, w http.ResponseWriter, info contract.APIKeyInfo, mr contract.ModelRequest, asked string, messages []contract.Message) {
	resp, err := s.pool.Chat(ctx, mr, messages)
	if err != nil {
		slog.Warn("modelgate: chat failed", "key", info.ID, "kind", mr.Kind, "pinned", mr.PinnedEntry, "err", err)
		status, typ, msg := chatErrorStatus(err, mr, s.pool.Snapshot())
		writeError(w, status, typ, msg, "")
		return
	}
	writeJSON(w, chatCompletion{
		ID:      newCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   echoModel(info, mr, asked, resp.Model),
		Choices: []completionChoice{{
			Index: 0,
			Message: completionMessage{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: toOpenAIToolCalls(resp.ToolCalls),
			},
			FinishReason: finishReasonFor(resp),
		}},
		Usage: usageOf(resp.Usage),
	})
}

// streamChat runs a streaming completion, translating pool StreamEvents into
// OpenAI SSE chunks. Headers are written LAZILY on the first content event so a
// pre-stream pool error (no entry available) can still surface as a proper JSON
// error status; once streaming has started, a mid-stream failure can only be
// reported in-band (an error chunk + [DONE]).
//
// TEXT deltas stream live. TOOL-CALL deltas do NOT: a pure tool-call round carries
// no visible text, so the pool keeps it retriable and may fail over to another
// backend mid-stream (leaf/model/45-chat-stream-watch.go) — re-running this watcher
// and re-emitting a fresh set of index-0 tool_call deltas. A client reassembling
// per-index by concatenation would then splice two partial argument strings into
// invalid JSON. So tool_calls are emitted ONCE, complete, from the SETTLED response
// after ChatStreamWatch returns — after any failover has resolved.
func (s *server) streamChat(ctx context.Context, w http.ResponseWriter, info contract.APIKeyInfo, mr contract.ModelRequest, asked string, messages []contract.Message) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported", "")
		return
	}
	st := &sseStream{
		w:       w,
		flusher: flusher,
		id:      newCompletionID(),
		created: time.Now().Unix(),
		// Chunks must name a model before any entry has been picked, so echo the
		// caller's own selection (the lane / the pinned name) throughout.
		model: echoModel(info, mr, asked, ""),
	}

	resp, err := s.pool.ChatStreamWatch(ctx, mr, messages, func(ev contract.StreamEvent, _ *contract.StreamAccumulator) error {
		if ev.Text != "" {
			st.sendDelta(chunkDelta{Content: ev.Text})
		}
		// Tool deltas deliberately NOT streamed live — see the doc comment above.
		return nil
	})

	if err != nil {
		if !st.started {
			// Nothing streamed yet → a clean JSON error with the right status.
			slog.Warn("modelgate: stream failed pre-content", "key", info.ID, "kind", mr.Kind, "pinned", mr.PinnedEntry, "err", err)
			status, typ, msg := chatErrorStatus(err, mr, s.pool.Snapshot())
			writeError(w, status, typ, msg, "")
			return
		}
		// Already committed to a 200 SSE stream → report in-band, best effort.
		slog.Warn("modelgate: stream failed mid-content", "key", info.ID, "kind", mr.Kind, "pinned", mr.PinnedEntry, "err", err)
		st.sendRaw(errEnvelope{Error: errDetail{Message: "stream failed", Type: "server_error"}})
		st.done()
		return
	}
	// Ensure the stream is open even for an empty (no-content, no-tool) response.
	st.ensureStarted()
	// Tool calls from the settled response — complete, emitted once (never during a
	// possibly-refailed-over stream).
	if len(resp.ToolCalls) > 0 {
		st.sendToolCalls(resp.ToolCalls)
	}
	// Terminal chunk: finish reason + usage, then [DONE].
	reason := finishReasonFor(resp)
	st.sendFinal(reason, usageOf(resp.Usage))
	st.done()
}

// sseStream carries the streaming write state. All methods run on the handler's
// goroutine (the pool invokes the watcher synchronously within ChatStreamWatch),
// so no locking is needed.
type sseStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	created int64
	model   string
	started bool
}

// ensureStarted writes the SSE headers + the leading role delta exactly once.
func (st *sseStream) ensureStarted() {
	if st.started {
		return
	}
	st.started = true
	h := st.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	st.w.WriteHeader(http.StatusOK)
	st.sendChunk(chatChunk{
		ID: st.id, Object: "chat.completion.chunk", Created: st.created, Model: st.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Role: "assistant"}}},
	})
}

// sendDelta emits one content/tool delta chunk (opening the stream on first use).
func (st *sseStream) sendDelta(d chunkDelta) {
	st.ensureStarted()
	st.sendChunk(chatChunk{
		ID: st.id, Object: "chat.completion.chunk", Created: st.created, Model: st.model,
		Choices: []chunkChoice{{Index: 0, Delta: d}},
	})
}

// sendToolCalls emits the settled tool calls as ONE delta chunk (each with its
// stable index), opening the stream if needed. Complete arguments in a single
// chunk → a client concatenating per index reassembles them intact.
func (st *sseStream) sendToolCalls(tcs []contract.ToolCall) {
	deltas := make([]oaiToolCallDelta, 0, len(tcs))
	for i, tc := range tcs {
		deltas = append(deltas, oaiToolCallDelta{
			Index:    i,
			ID:       tc.ID,
			Type:     "function",
			Function: oaiToolCallFunc{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	st.sendDelta(chunkDelta{ToolCalls: deltas})
}

// sendFinal emits the closing chunk (finish_reason + usage).
func (st *sseStream) sendFinal(reason string, u completionUsage) {
	st.sendChunk(chatChunk{
		ID: st.id, Object: "chat.completion.chunk", Created: st.created, Model: st.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: &reason}},
		Usage:   &u,
	})
}

// sendChunk marshals + writes one SSE data frame and flushes.
func (st *sseStream) sendChunk(c chatChunk) { st.sendRaw(c) }

// sendRaw writes any value as one `data: <json>\n\n` SSE frame and flushes.
func (st *sseStream) sendRaw(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = st.w.Write([]byte("data: "))
	_, _ = st.w.Write(b)
	_, _ = st.w.Write([]byte("\n\n"))
	st.flusher.Flush()
}

// done writes the terminal [DONE] sentinel.
func (st *sseStream) done() {
	_, _ = st.w.Write([]byte("data: [DONE]\n\n"))
	st.flusher.Flush()
}

// servedModel prefers the entry name the pool reported; falls back to the
// requested model string when the pool didn't set one.
func servedModel(served, requested string) string {
	if served != "" {
		return served
	}
	return requested
}

// newCompletionID mints an OpenAI-style completion id.
func newCompletionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}
