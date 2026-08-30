package modelgate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbob/contract"
)

// --- fakes ----------------------------------------------------------------

type fakePool struct {
	entries        []contract.ModelInfo
	chatResp       contract.ChatResponse
	chatErr        error
	lastReq        contract.ModelRequest
	lastConsumer   string // "<source>:<uid>" seen on ctx at Chat time
	lastMsgContent string // the last message body handed to Chat
	streamEvents   []contract.StreamEvent
}

func (f *fakePool) Chat(ctx context.Context, req contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	f.lastReq = req
	if len(msgs) > 0 {
		f.lastMsgContent = msgs[len(msgs)-1].Content
	}
	if s, u, ok := contract.ConsumerFrom(ctx); ok {
		f.lastConsumer = s + ":" + u
	}
	return f.chatResp, f.chatErr
}

func (f *fakePool) ChatStreamWatch(ctx context.Context, req contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	f.lastReq = req
	if s, u, ok := contract.ConsumerFrom(ctx); ok {
		f.lastConsumer = s + ":" + u
	}
	if w != nil {
		for _, ev := range f.streamEvents {
			if err := w(ev, &contract.StreamAccumulator{}); err != nil {
				return contract.ChatResponse{}, err
			}
		}
	}
	return f.chatResp, f.chatErr
}

func (f *fakePool) Snapshot() contract.PoolSnapshot { return contract.PoolSnapshot{Entries: f.entries} }
func (f *fakePool) FlushUsage(context.Context)      {}
func (f *fakePool) FlushUsageFinal(context.Context) {}
func (f *fakePool) Close()                          {}

type fakeKeys struct {
	byToken map[string]contract.APIKeyInfo
}

func (f fakeKeys) VerifyKey(_ context.Context, token string) (contract.APIKeyInfo, bool) {
	info, ok := f.byToken[token]
	return info, ok
}

func newTestServer(pool contract.ModelPool, keys contract.APIKeys) *server {
	return &server{pool: pool, keys: func() contract.APIKeys { return keys }}
}

func do(s *server, method, path, token, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

var poolEntries = []contract.ModelInfo{
	{Name: "big", Kind: contract.KindLLM, Tags: []string{"smart"}, State: "live", ContextWindow: 131072},
	{Name: "small", Kind: contract.KindLLM, Tags: []string{"fast"}, State: "live", ContextWindow: 32768},
	{Name: "dead404", Kind: contract.KindLLM, Tags: []string{"smart"}, State: "disabled"},
	{Name: "hymt", Kind: contract.KindTranslate, State: "live", ContextWindow: 4096},
	{Name: "whisper", Kind: contract.KindASR, State: "live"},
}

// --- request bodies -------------------------------------------------------

// laneBody is the lane-form request: routing rides on `kind` + `requires` +
// `prefer`, the picker's own vocabulary, all bob extensions an OpenAI SDK sends via
// extra_body. `model` is absent entirely.
func laneBody(kind string, requires, prefer []string, stream bool) string {
	b := `{"messages":[{"role":"user","content":"hi"}],"stream":` + boolJSON(stream)
	if kind != "" {
		b += `,"kind":"` + kind + `"`
	}
	if len(requires) > 0 {
		q, _ := json.Marshal(requires)
		b += `,"requires":` + string(q)
	}
	if len(prefer) > 0 {
		q, _ := json.Marshal(prefer)
		b += `,"prefer":` + string(q)
	}
	return b + "}"
}

// pinBody is the pin-form request: the entry name goes in the standard `model`.
func pinBody(model string, stream bool) string {
	return `{"model":"` + model + `","stream":` + boolJSON(stream) + `,"messages":[{"role":"user","content":"hi"}]}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// --- auth -----------------------------------------------------------------

func TestAuthMissingAndBad(t *testing.T) {
	s := newTestServer(&fakePool{entries: poolEntries}, fakeKeys{byToken: map[string]contract.APIKeyInfo{}})
	if w := do(s, http.MethodGet, "/v1/models", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", w.Code)
	}
	if w := do(s, http.MethodGet, "/v1/models", "nope", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token: got %d, want 401", w.Code)
	}
}

func TestAuthAbsentAuthority(t *testing.T) {
	s := &server{pool: &fakePool{}, keys: func() contract.APIKeys { return nil }}
	if w := do(s, http.MethodGet, "/v1/models", "any", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("absent APIKeys: got %d, want 401 (fail-closed)", w.Code)
	}
}

// --- /v1/models -----------------------------------------------------------

func modelIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad models body: %v (%s)", err, w.Body.String())
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

// TestModelsLaneForm: a lane-form key addresses CAPABILITIES — it sees the tags its
// lanes carry. Not entry names (bob's internal inventory), and no longer lane names
// either: a lane partitions payload shape / admission / queueing / accounting, all
// of it inside bob (docs/modelgate-tags.md §1).
func TestModelsLaneForm(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	s := newTestServer(&fakePool{entries: poolEntries}, keys)
	w := do(s, http.MethodGet, "/v1/models", "k", "")
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	ids := modelIDs(t, w)
	// "smart" survives its disabled carrier (dead404) because `big` also has it;
	// the translate lane contributes nothing because `hymt` carries no tags at all —
	// an untagged entry declares no capability and is not on offer. (`kind:
	// translate` still routes to it: lane names stay accepted, just unlisted.)
	if len(ids) != 2 || !contains(ids, "smart") || !contains(ids, "fast") {
		t.Errorf("lane-form models = %v, want exactly [fast smart]", ids)
	}
	for _, name := range []string{"big", "small", "dead404", "hymt", "whisper", "auto",
		contract.KindLLM, contract.KindTranslate} {
		if contains(ids, name) {
			t.Errorf("lane-form leaked %q: %v", name, ids)
		}
	}
}

// The catalog's one non-standard field, and the only thing a caller cannot work out
// for itself: whether a capability is usable right now. Pool vocabulary verbatim.
func TestModelsCarryState(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM}},
	}}
	entries := []contract.ModelInfo{
		{Name: "big", Kind: contract.KindLLM, Tags: []string{"smart"}, State: "cooling"},
		{Name: "big2", Kind: contract.KindLLM, Tags: []string{"smart"}, State: "tentative"},
		{Name: "small", Kind: contract.KindLLM, Tags: []string{"fast"}, State: "paused"},
	}
	w := do(newTestServer(&fakePool{entries: entries}, keys), http.MethodGet, "/v1/models", "k", "")
	var got struct {
		Data []struct{ ID, State string } `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v (%s)", err, w.Body.String())
	}
	states := map[string]string{}
	for _, d := range got.Data {
		states[d.ID] = d.State
	}
	// best-of, not worst-of: any carrier may take the request, so "smart" reports the
	// tentative one rather than the cooling one.
	if states["smart"] != "tentative" {
		t.Errorf("smart state = %q, want tentative (best of cooling+tentative)", states["smart"])
	}
	if states["fast"] != "paused" {
		t.Errorf("fast state = %q, want paused", states["fast"])
	}
}

// `model` naming a capability is THE external address: the caller has a tag off
// /v1/models and nothing else, so the gateway resolves which lane carries it and
// turns the name into the same hard requirement `requires` carries.
func TestChatModelNamesACapability(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	// Two lanes and no `kind` — this used to be a 400 asking for one.
	w := do(newTestServer(pool, keys), http.MethodPost, "/v1/chat/completions", "k", pinBody("smart", false))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != contract.KindLLM {
		t.Errorf("lane not resolved from the tag: %+v", pool.lastReq)
	}
	if !contains(pool.lastReq.Requires, "smart") {
		t.Errorf("the named capability must become a hard requirement: %+v", pool.lastReq)
	}
	if pool.lastReq.PinnedEntry != "" {
		t.Errorf("a capability is not a pin: %+v", pool.lastReq)
	}
}

// `model` + `requires` together: the shorthand joins the list, it does not replace
// it, and it is not duplicated when the caller already spelled it out.
func TestChatModelCapabilityJoinsRequires(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM}},
	}}
	s := newTestServer(pool, keys)
	body := `{"model":"smart","requires":["fast"],"messages":[{"role":"user","content":"hi"}],"stream":false}`
	if w := do(s, http.MethodPost, "/v1/chat/completions", "k", body); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(pool.lastReq.Requires) != 2 ||
		!contains(pool.lastReq.Requires, "smart") || !contains(pool.lastReq.Requires, "fast") {
		t.Errorf("requires = %v, want both fast and smart", pool.lastReq.Requires)
	}
}

// The reply echoes the capability the caller named. A dropdown client matches the
// reply against the id it picked off /v1/models, and a lane name is an id that
// listing never showed it.
func TestChatEchoesTheCapabilityAsked(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM}},
	}}
	w := do(newTestServer(pool, keys), http.MethodPost, "/v1/chat/completions", "k", pinBody("smart", false))
	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v (%s)", err, w.Body.String())
	}
	if got.Model != "smart" {
		t.Errorf("echoed %q, want the capability the caller sent (\"smart\")", got.Model)
	}
	if got.Model == "big" {
		t.Errorf("echoed the backend entry name — internal inventory must not travel out")
	}
}

// An explicit `kind` that disagrees with where the capability actually lives is a
// contradiction, not a preference. Honouring it would route into a lane where
// nothing carries the tag and surface as a 503 blaming a requirement the caller
// never wrote.
func TestChatRefusesKindThatDoesNotCarryTheCapability(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	body := `{"kind":"translate","model":"smart","messages":[{"role":"user","content":"hi"}],"stream":false}`
	w := do(newTestServer(pool, keys), http.MethodPost, "/v1/chat/completions", "k", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != "" {
		t.Errorf("a contradictory request must not reach the pool: %+v", pool.lastReq)
	}
}

// The catalog and the endpoints have to agree on what a key can address. A pin-form
// key lists entry NAMES, so a tag detail must 404 for it — answering 200 while its
// own chat requests reject the same string is the contradiction this endpoint set
// out to remove.
func TestModelDetailTagsAreLaneFormOnly(t *testing.T) {
	pool := &fakePool{entries: poolEntries}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"pin": {ID: "ak2", Models: []string{"big"}},
	}}
	s := newTestServer(pool, keys)
	if w := do(s, http.MethodGet, "/v1/models/smart", "pin", ""); w.Code != http.StatusNotFound {
		t.Errorf("pin-form key got %d for a tag detail, want 404: %s", w.Code, w.Body.String())
	}
	// its own vocabulary still resolves
	if w := do(s, http.MethodGet, "/v1/models/big", "pin", ""); w.Code != 200 {
		t.Errorf("pin-form key got %d for its own entry name, want 200", w.Code)
	}
}

// A lane name in `model` still routes (unlisted, but accepted) — the clients that
// were told to send one must not break.
func TestChatModelStillAcceptsALaneName(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "hymt"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	if w := do(newTestServer(pool, keys), http.MethodPost, "/v1/chat/completions", "k",
		pinBody(contract.KindTranslate, false)); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != contract.KindTranslate || len(pool.lastReq.Requires) != 0 {
		t.Errorf("a lane name must route as a lane, not as a tag: %+v", pool.lastReq)
	}
}

func TestModelsPinForm(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Models: []string{"big", "small"}},
	}}
	s := newTestServer(&fakePool{entries: poolEntries}, keys)
	ids := modelIDs(t, do(s, http.MethodGet, "/v1/models", "k", ""))
	if len(ids) != 2 || !contains(ids, "big") || !contains(ids, "small") {
		t.Errorf("pin-form models = %v, want big + small", ids)
	}
}

// --- chat: lane form ------------------------------------------------------

// TestChatLaneRoutingPassthrough: kind/requires/prefer reach the picker UNCHANGED —
// one mechanism for internal and external callers alike. requires stays HARD, so the
// pool's admin-declared tag-fallback chain and its "a queue beats a quiet downgrade"
// guard (which keys on len(Requires)>0) both stay armed for external traffic.
func TestChatLaneRoutingPassthrough(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "small"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", laneBody(contract.KindLLM, []string{"fast"}, []string{"smart"}, false))
	if w.Code != 200 {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != contract.KindLLM || pool.lastReq.PinnedEntry != "" {
		t.Errorf("req = %+v, want Kind=llm with no pin", pool.lastReq)
	}
	if len(pool.lastReq.Requires) != 1 || pool.lastReq.Requires[0] != "fast" {
		t.Errorf("Requires = %v, want the request's requires [fast] verbatim", pool.lastReq.Requires)
	}
	if len(pool.lastReq.Prefer) != 1 || pool.lastReq.Prefer[0] != "smart" {
		t.Errorf("Prefer = %v, want the request's prefer [smart] verbatim", pool.lastReq.Prefer)
	}
	if pool.lastConsumer != "apikey:ak1" {
		t.Errorf("billing consumer = %q, want apikey:ak1", pool.lastConsumer)
	}
	// The served entry name must not travel back to a lane-addressing caller.
	if strings.Contains(w.Body.String(), "small") {
		t.Errorf("response leaked the served entry name: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model":"llm"`) {
		t.Errorf("response should echo the selected lane: %s", w.Body.String())
	}
}

// TestChatLaneSelectsTranslate: the same key reaches a second lane by changing one
// field — the whole point of moving the lane off the key.
func TestChatLaneSelectsTranslate(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "猫坐在垫子上", Model: "hymt"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", laneBody(contract.KindTranslate, nil, nil, false))
	if w.Code != 200 {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != contract.KindTranslate {
		t.Errorf("Kind = %q, want translate", pool.lastReq.Kind)
	}
}

// TestChatLaneOmittedResolution: `kind` may be omitted when the key leaves no
// ambiguity — a sole lane, or `model` carrying a lane name (the only address a
// dropdown-only client like open-webui can send).
func TestChatLaneOmittedResolution(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"one":  {ID: "ak1", Kinds: []string{contract.KindTranslate}},
		"many": {ID: "ak2", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	s := newTestServer(pool, keys)

	if w := do(s, http.MethodPost, "/v1/chat/completions", "one", laneBody("", nil, nil, false)); w.Code != 200 {
		t.Fatalf("sole-lane key omitting kind: got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != contract.KindTranslate {
		t.Errorf("Kind = %q, want the key's sole lane", pool.lastReq.Kind)
	}

	// `model` as the lane shorthand.
	if w := do(s, http.MethodPost, "/v1/chat/completions", "many", pinBody(contract.KindTranslate, false)); w.Code != 200 {
		t.Fatalf("model-as-lane shorthand: got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != contract.KindTranslate {
		t.Errorf("Kind = %q, want translate from the model shorthand", pool.lastReq.Kind)
	}

	// Multi-lane key with neither → 400 that names the lanes.
	w := do(s, http.MethodPost, "/v1/chat/completions", "many", laneBody("", nil, nil, false))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multi-lane key omitting kind: got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "llm") || !strings.Contains(w.Body.String(), "translate") {
		t.Errorf("400 should list the lanes: %s", w.Body.String())
	}
}

// TestChatLaneNotAdmitted: a lane outside the key's list is a 404 naming what IS
// selectable, and the request never reaches the pool.
func TestChatLaneNotAdmitted(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "whisper"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM}},
	}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", laneBody(contract.KindASR, nil, nil, false))
	if w.Code != http.StatusNotFound {
		t.Fatalf("un-admitted lane: got %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "llm") {
		t.Errorf("404 should name the selectable lanes: %s", w.Body.String())
	}
	if pool.lastReq.Kind != "" {
		t.Errorf("rejected request must not reach the pool: %+v", pool.lastReq)
	}
}

// TestChatEntryNameRejectedOnLaneForm: entry names are not part of a lane-form key's
// vocabulary — naming one is a 404, never a silent pin.
func TestChatEntryNameRejectedOnLaneForm(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", false))
	if w.Code != http.StatusNotFound {
		t.Fatalf("entry name on a lane-form key: got %d, want 404", w.Code)
	}
	if pool.lastReq.PinnedEntry != "" {
		t.Errorf("must not pin: %+v", pool.lastReq)
	}
}

// --- chat: pin form -------------------------------------------------------

func TestChatPinnedAllowed(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "hello", Model: "big", Usage: contract.Usage{InputTokens: 3, OutputTokens: 2}}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"big"}}}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", false))
	if w.Code != 200 {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.PinnedEntry != "big" {
		t.Errorf("PinnedEntry = %q, want big", pool.lastReq.PinnedEntry)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("body missing content: %s", w.Body.String())
	}
}

func TestChatPinnedNotAllowed(t *testing.T) {
	pool := &fakePool{entries: poolEntries}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"small"}}}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", false))
	if w.Code != http.StatusNotFound {
		t.Errorf("pin outside allowlist: got %d, want 404", w.Code)
	}
}

// TestChatPinIgnoresRouting: pinning bypasses routing, so a pin-form request's
// kind/tags must NOT reach the pool — setting them would only mislead (the pool
// ignores both on the pinned path).
func TestChatPinIgnoresRouting(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"big"}}}}
	s := newTestServer(pool, keys)
	body := `{"model":"big","kind":"translate","requires":["fast"],"prefer":["x"],"messages":[{"role":"user","content":"hi"}]}`
	if w := do(s, http.MethodPost, "/v1/chat/completions", "k", body); w.Code != 200 {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.Kind != "" || len(pool.lastReq.Requires) != 0 || len(pool.lastReq.Prefer) != 0 {
		t.Errorf("pin req = %+v, want Kind/Requires/Prefer unset", pool.lastReq)
	}
	if pool.lastReq.PinnedEntry != "big" {
		t.Errorf("PinnedEntry = %q, want big", pool.lastReq.PinnedEntry)
	}
}

// TestChatPinCrossKind: the pin form is lane-blind by design (the pool doesn't check
// Kind on the pinned path either) — the entry-name allowlist IS the whole fence.
func TestChatPinCrossKind(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "whisper"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"whisper"}}}}
	s := newTestServer(pool, keys)
	if w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("whisper", false)); w.Code != 200 {
		t.Fatalf("pinning a named ASR entry: got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.PinnedEntry != "whisper" {
		t.Errorf("PinnedEntry = %q, want whisper", pool.lastReq.PinnedEntry)
	}
}

// TestChatSolePinDefault: a pin-form key with exactly one allowed entry may omit
// `model`; with several, the honest answer is "say which one".
func TestChatSolePinDefault(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"one":  {ID: "ak1", Models: []string{"big"}},
		"many": {ID: "ak2", Models: []string{"big", "small"}},
	}}
	s := newTestServer(pool, keys)
	if w := do(s, http.MethodPost, "/v1/chat/completions", "one", body); w.Code != 200 {
		t.Fatalf("sole-entry key omitting model: got %d (%s)", w.Code, w.Body.String())
	}
	if pool.lastReq.PinnedEntry != "big" {
		t.Errorf("PinnedEntry = %q, want the sole allowed entry", pool.lastReq.PinnedEntry)
	}
	w := do(s, http.MethodPost, "/v1/chat/completions", "many", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multi-entry key omitting model: got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "big") || !strings.Contains(w.Body.String(), "small") {
		t.Errorf("400 should list the choices: %s", w.Body.String())
	}
}

// TestChatEmptyPolicyFailsClosed: a key carrying neither form (a pre-v8 row, whose
// lane/tag columns v8 stopped reading) reaches NOTHING, and the message says so
// rather than trailing off after "selectable: ".
func TestChatEmptyPolicyFailsClosed(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatResp: contract.ChatResponse{Content: "ok", Model: "big"}}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1"}}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", false))
	if w.Code != http.StatusNotFound {
		t.Fatalf("policy-less key: got %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "reaches no lane or entry") {
		t.Errorf("empty selectable list should be spelled out: %s", w.Body.String())
	}
	if pool.lastReq.PinnedEntry != "" {
		t.Errorf("must not reach the pool: %+v", pool.lastReq)
	}
	if ids := modelIDs(t, do(s, http.MethodGet, "/v1/models", "k", "")); len(ids) != 0 {
		t.Errorf("policy-less key should list nothing, got %v", ids)
	}
}

// --- errors ---------------------------------------------------------------

// TestLaneDiagnosis: the 503 says WHY — the lane is empty, its entries are all down
// (transient), or one is live so the REQUEST was refused — and never names entries.
func TestLaneDiagnosis(t *testing.T) {
	snap := contract.PoolSnapshot{Entries: []contract.ModelInfo{
		{Name: "big", Kind: contract.KindLLM, Tags: []string{"smart"}, State: "cooling"},
		{Name: "hymt", Kind: contract.KindTranslate, State: "cooling"},
	}}
	got := laneDiagnosis(contract.ModelRequest{Kind: contract.KindTranslate}, snap)
	if !strings.Contains(got, "cooling×1") {
		t.Errorf("all-down diagnosis = %q, want the state histogram", got)
	}
	if strings.Contains(got, "hymt") {
		t.Errorf("diagnosis leaked an entry name: %q", got)
	}
	// A LIVE candidate means the pool reached a backend and the REQUEST was refused
	// (oversized prompt is the usual cause) — claiming "none could serve" would lie.
	live := contract.PoolSnapshot{Entries: []contract.ModelInfo{{Name: "big", Kind: contract.KindLLM, State: "live"}}}
	got = laneDiagnosis(contract.ModelRequest{Kind: contract.KindLLM}, live)
	if !strings.Contains(got, "the request itself was refused") || !strings.Contains(got, "context_window") {
		t.Errorf("live-candidate diagnosis = %q, want the request-refused shape", got)
	}
	got = laneDiagnosis(contract.ModelRequest{Kind: contract.KindOCR}, snap)
	if !strings.Contains(got, "no ocr backend is configured") {
		t.Errorf("empty-lane diagnosis = %q", got)
	}
	// requires is HARD, so "nothing carries this tag" is a real outcome again — and
	// the caller must be told which tag, and that no fallback was declared for it.
	got = laneDiagnosis(contract.ModelRequest{Kind: contract.KindLLM, Requires: []string{"vision"}}, live)
	if !strings.Contains(got, "carries the required tag(s) vision") || !strings.Contains(got, "tag-fallback") {
		t.Errorf("unmet-requirement diagnosis = %q, want the tag named + the fallback hint", got)
	}
}

// TestChatErrorStatus: a full wait queue is BUSY (retryable 429), an outage is 503.
// modelgate keeps no gate of its own, so this mapping is the whole saturation story.
func TestChatErrorStatus(t *testing.T) {
	snap := contract.PoolSnapshot{Entries: poolEntries}
	mr := contract.ModelRequest{Kind: contract.KindLLM}
	if code, _, msg := chatErrorStatus(fmt.Errorf("wrapped: %w", contract.ErrModelQueueFull), mr, snap); code != http.StatusTooManyRequests {
		t.Errorf("queue-full: got %d (%s), want 429", code, msg)
	}
	if code, _, _ := chatErrorStatus(context.DeadlineExceeded, mr, snap); code != http.StatusServiceUnavailable {
		t.Errorf("outage: got %d, want 503", code)
	}
}

func TestChatPoolErrorIs503(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatErr: context.DeadlineExceeded}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"big"}}}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", false))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("pool error: got %d, want 503", w.Code)
	}
}

func TestChatQueueFullIs429(t *testing.T) {
	pool := &fakePool{entries: poolEntries, chatErr: contract.ErrModelQueueFull}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM}},
	}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", laneBody(contract.KindLLM, nil, nil, false))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("queue full: got %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "saturated") {
		t.Errorf("429 should say the backends are up but saturated: %s", w.Body.String())
	}
}

// --- streaming ------------------------------------------------------------

func TestChatStreamSSE(t *testing.T) {
	pool := &fakePool{
		entries:      poolEntries,
		chatResp:     contract.ChatResponse{Content: "Hello world", Model: "big", Usage: contract.Usage{InputTokens: 1, OutputTokens: 2}},
		streamEvents: []contract.StreamEvent{{Text: "Hello "}, {Text: "world"}, {Done: true}},
	}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"big"}}}}
	s := newTestServer(pool, keys)
	w := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", true))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	for _, want := range []string{`"role":"assistant"`, "Hello ", "world", `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q:\n%s", want, body)
		}
	}
}

// TestChatStreamLaneEcho: a streaming lane-form response names the LANE in every
// chunk — chunks must carry a model before any entry is picked, and the entry name
// is exactly what this form keeps internal.
func TestChatStreamLaneEcho(t *testing.T) {
	pool := &fakePool{
		entries:      poolEntries,
		chatResp:     contract.ChatResponse{Content: "hi", Model: "big"},
		streamEvents: []contract.StreamEvent{{Text: "hi"}, {Done: true}},
	}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindLLM}},
	}}
	s := newTestServer(pool, keys)
	body := do(s, http.MethodPost, "/v1/chat/completions", "k", laneBody(contract.KindLLM, nil, nil, true)).Body.String()
	if !strings.Contains(body, `"model":"llm"`) {
		t.Errorf("chunks should name the lane: %s", body)
	}
	if strings.Contains(body, `"big"`) {
		t.Errorf("stream leaked the served entry name: %s", body)
	}
}

// TestChatStreamToolCalls: a streaming tool-call response emits the settled
// tool_calls ONCE (from the returned response, not live per-delta) so a mid-stream
// failover can't splice two partial argument sets into corrupt JSON.
func TestChatStreamToolCalls(t *testing.T) {
	pool := &fakePool{
		entries: poolEntries,
		chatResp: contract.ChatResponse{
			Model:     "big",
			ToolCalls: []contract.ToolCall{{ID: "call_1", Name: "get_time", Arguments: `{"city":"NYC"}`}},
		},
		// The pool may stream partial tool deltas internally, but modelgate must NOT
		// forward them live — so even if events carry ToolDelta, none should reach the wire.
		streamEvents: []contract.StreamEvent{{ToolDelta: &contract.ToolCallDelta{Index: 0, Name: "get_time", ArgumentsDelta: `{"ci`}}, {Done: true}},
	}
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"big"}}}}
	s := newTestServer(pool, keys)
	body := do(s, http.MethodPost, "/v1/chat/completions", "k", pinBody("big", true)).Body.String()
	if !strings.Contains(body, `"get_time"`) || !strings.Contains(body, `{\"city\":\"NYC\"}`) {
		t.Errorf("stream missing settled tool_calls:\n%s", body)
	}
	// Exactly ONE tool_calls delta (the settled one) — the partial `{\"ci` fragment
	// must never have been forwarded live.
	if n := strings.Count(body, `"tool_calls":[`); n != 1 {
		t.Errorf("want exactly 1 tool_calls delta (settled), got %d:\n%s", n, body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream missing tool_calls finish / DONE:\n%s", body)
	}
}

// --- /v1/key --------------------------------------------------------------

func TestKeyInfoLaneForm(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", AccountID: "ac9", Kinds: []string{contract.KindLLM, contract.KindTranslate}},
	}}
	s := newTestServer(&fakePool{entries: poolEntries}, keys)
	w := do(s, http.MethodGet, "/v1/key", "k", "")
	if w.Code != 200 {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	var resp keyInfoResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad body: %v (%s)", err, w.Body.String())
	}
	if resp.Key.ID != "ak1" || resp.Key.Account != "ac9" || len(resp.Key.Kinds) != 2 {
		t.Errorf("key policy wrong: %+v", resp.Key)
	}
	if len(resp.Key.Models) != 0 {
		t.Errorf("lane-form key must not report a pin list: %+v", resp.Key)
	}
	// One row per LANE, and capability broken down per TAG — NOT a lane-wide minimum.
	// poolEntries has big(smart,131072) and small(fast,32768) in the llm lane: a
	// kind-level min would report 32768 for everything, hiding that a `smart`
	// requirement actually guarantees 131072.
	if len(resp.Models) != 2 || resp.Models[0].Name != contract.KindLLM {
		t.Fatalf("models = %+v, want one row per lane", resp.Models)
	}
	llm := resp.Models[0]
	if llm.State != "live" {
		t.Errorf("llm lane state = %q, want live", llm.State)
	}
	if llm.ContextWindow != 0 {
		t.Errorf("a tagged lane must NOT report a kind-level window, got %d", llm.ContextWindow)
	}
	byTag := map[string]tagRuntime{}
	for _, tr := range llm.Tags {
		byTag[tr.Name] = tr
	}
	if got := byTag["smart"]; got.ContextWindow != 131072 || got.State != "live" {
		t.Errorf("tag smart = %+v, want the smart backend's own 131072 (not the lane min)", got)
	}
	if got := byTag["fast"]; got.ContextWindow != 32768 {
		t.Errorf("tag fast = %+v, want 32768", got)
	}
	// A lane whose backends carry no tags has nothing to break down → its own window.
	tr := resp.Models[1]
	if tr.Name != contract.KindTranslate || tr.ContextWindow != 4096 || len(tr.Tags) != 0 {
		t.Errorf("tag-less lane runtime wrong: %+v", tr)
	}
	for _, p := range []string{"kind", "requires", "prefer"} {
		if !contains(resp.Params.Honored, p) {
			t.Errorf("honored params must advertise %q: %+v", p, resp.Params)
		}
	}
	if !contains(resp.Params.Ignored, "temperature") {
		t.Errorf("ignored params wrong: %+v", resp.Params)
	}
}

// TestKeyInfoEmptyLane: a lane on the key's list with no backend in the pool reports
// "unavailable" rather than silently looking healthy.
func TestKeyInfoEmptyLane(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"k": {ID: "ak1", Kinds: []string{contract.KindOCR}},
	}}
	s := newTestServer(&fakePool{entries: poolEntries}, keys)
	var resp keyInfoResponse
	if err := json.Unmarshal(do(s, http.MethodGet, "/v1/key", "k", "").Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if len(resp.Models) != 1 || resp.Models[0].State != "unavailable" {
		t.Errorf("empty lane = %+v, want one 'unavailable' row", resp.Models)
	}
}

func TestKeyInfoPinForm(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1", Models: []string{"big"}}}}
	s := newTestServer(&fakePool{entries: poolEntries}, keys)
	var resp keyInfoResponse
	if err := json.Unmarshal(do(s, http.MethodGet, "/v1/key", "k", "").Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if len(resp.Key.Kinds) != 0 || len(resp.Key.Models) != 1 {
		t.Errorf("pin-form policy wrong: %+v", resp.Key)
	}
	if len(resp.Models) != 1 || resp.Models[0].Name != "big" || resp.Models[0].ContextWindow != 131072 {
		t.Errorf("pin-form runtime should be per ENTRY: %+v", resp.Models)
	}
}

func TestKeyInfoAuth(t *testing.T) {
	s := newTestServer(&fakePool{entries: poolEntries}, fakeKeys{})
	if w := do(s, http.MethodGet, "/v1/key", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", w.Code)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

var _ contract.ModelPool = (*fakePool)(nil)
var _ contract.APIKeys = fakeKeys{}

// TestModelsEmptyListIsArray: a key that reaches nothing must render "data": [] —
// "data": null blows up an SDK that iterates the field, and every pre-v8 key lands
// on exactly this path.
func TestModelsEmptyListIsArray(t *testing.T) {
	keys := fakeKeys{byToken: map[string]contract.APIKeyInfo{"k": {ID: "ak1"}}}
	s := newTestServer(&fakePool{entries: poolEntries}, keys)
	if body := do(s, http.MethodGet, "/v1/models", "k", "").Body.String(); !strings.Contains(body, `"data":[]`) {
		t.Errorf("empty models body = %s, want an empty ARRAY", body)
	}
}
