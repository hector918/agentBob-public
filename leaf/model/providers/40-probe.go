package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ProbeResult is what an inference server tells us about itself. Zero values
// mean "didn't get an answer" — caller falls back to YAML or built-in default.
// Source labels the origin so doctor can show "probed:llama-cpp" vs "yaml".
type ProbeResult struct {
	ContextWindow int    // 0 = unknown
	Concurrency   int    // 0 = unknown
	Source        string // e.g. "llama-cpp /props", "ollama /api/show", ""
}

// probeTimeout is the per-probe HTTP timeout — startup must not block on
// a slow / dead model server.
const probeTimeout = 2 * time.Second

// Probe asks the model backend what it can tell us about context window and
// concurrency. baseURL is the OpenAI-compat root (".../v1") — provider-specific
// probes strip /v1 if they need a different path. apiKey is sent as Bearer
// when non-empty. Errors are swallowed (returned ProbeResult is just zero) —
// startup must not fail because a server happened to be down at boot.
func Probe(ctx context.Context, provider, baseURL, apiKey, model string) ProbeResult {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	switch provider {
	case "llama-cpp":
		return probeLlamaCpp(pctx, baseURL, apiKey)
	case "ollama":
		return probeOllama(pctx, baseURL, apiKey, model)
	case "vllm":
		return probeVLLM(pctx, baseURL, apiKey, model)
	case "sglang":
		return probeSGLang(pctx, baseURL, apiKey)
	case "anthropic":
		return probeAnthropic(pctx, baseURL, apiKey, model)
	}
	// openai / openrouter / custom / unknown: no probe path.
	return ProbeResult{}
}

// stripV1 turns ".../v1" into "..." so probes can hit non-OpenAI-compat
// endpoints on the same host.
func stripV1(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// probeLlamaCpp uses GET /props which returns:
//
//	{
//	  "default_generation_settings": { "n_ctx": 32768 },
//	  "total_slots": 4
//	}
func probeLlamaCpp(ctx context.Context, baseURL, apiKey string) ProbeResult {
	body, err := httpGet(ctx, stripV1(baseURL)+"/props", apiKey)
	if err != nil {
		return ProbeResult{}
	}
	var r struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		TotalSlots int `json:"total_slots"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ProbeResult{}
	}
	return ProbeResult{
		ContextWindow: r.DefaultGenerationSettings.NCtx,
		Concurrency:   r.TotalSlots,
		Source:        "llama-cpp /props",
	}
}

// probeOllama uses POST /api/show with {"name": "MODEL"}; the response has
// a "model_info" map keyed by "<arch>.context_length" (we walk for any
// suffix match because the arch prefix varies — "qwen2.context_length",
// "llama.context_length", etc.). Concurrency is server-wide
// (OLLAMA_NUM_PARALLEL); not exposed per-model — return 0 there.
func probeOllama(ctx context.Context, baseURL, apiKey, model string) ProbeResult {
	if model == "" {
		return ProbeResult{}
	}
	body, err := httpPostJSON(ctx, stripV1(baseURL)+"/api/show", apiKey, map[string]string{"name": model})
	if err != nil {
		return ProbeResult{}
	}
	var r struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ProbeResult{}
	}
	for k, v := range r.ModelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		// encoding/json decodes every JSON number to float64 — the only real case.
		if x, ok := v.(float64); ok {
			return ProbeResult{ContextWindow: int(x), Source: "ollama /api/show"}
		}
	}
	return ProbeResult{}
}

// probeVLLM uses GET /v1/models — each entry has "max_model_len". Concurrency
// is server-side batching (no client-side limit needed); return 0.
func probeVLLM(ctx context.Context, baseURL, apiKey, model string) ProbeResult {
	body, err := httpGet(ctx, strings.TrimRight(baseURL, "/")+"/models", apiKey)
	if err != nil {
		return ProbeResult{}
	}
	var r struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ProbeResult{}
	}
	for _, m := range r.Data {
		if (model == "" || m.ID == model) && m.MaxModelLen > 0 {
			return ProbeResult{ContextWindow: m.MaxModelLen, Source: "vllm /v1/models"}
		}
	}
	return ProbeResult{}
}

// probeAnthropic hits GET /v1/models — Anthropic's API doesn't expose
// context_window or concurrency through any endpoint, so this probe is solely
// a model-id sanity check. If the configured model isn't in the returned list,
// we log a warning (with the closest match if there is one) but never block
// startup — the user can still call a model the catalog doesn't list. Source is
// set when the catalog call succeeded so doctor can show "probed:anthropic".
func probeAnthropic(ctx context.Context, baseURL, apiKey, model string) ProbeResult {
	c := NewAnthropicClient(baseURL, apiKey, 0, nil)
	ids, err := c.ListModels(ctx)
	if err != nil {
		return ProbeResult{}
	}
	if model != "" {
		found := false
		for _, id := range ids {
			if id == model {
				found = true
				break
			}
		}
		if !found {
			closest := closestModelID(model, ids)
			if closest != "" {
				slog.Warn("anthropic: model id not in /v1/models catalog",
					"configured", model, "closest_match", closest)
			} else {
				slog.Warn("anthropic: model id not in /v1/models catalog",
					"configured", model, "catalog_size", len(ids))
			}
		}
	}
	return ProbeResult{Source: "anthropic /v1/models"}
}

// closestModelID returns the id in `ids` whose lowercase form shares the
// longest common prefix with `want` (lowercased). Empty if `ids` is empty.
// Cheap heuristic — enough to point at the obvious typo (claude-sonet-4-5 →
// claude-sonnet-4-5) without dragging in a Levenshtein implementation.
func closestModelID(want string, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	w := strings.ToLower(want)
	best := ""
	bestLen := 0
	for _, id := range ids {
		l := commonPrefixLen(w, strings.ToLower(id))
		if l > bestLen {
			best = id
			bestLen = l
		}
	}
	if bestLen < 4 {
		return ""
	}
	return best
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// probeSGLang uses GET /get_model_info which returns:
//
//	{ "model_path": "...", "context_length": 8192 }
func probeSGLang(ctx context.Context, baseURL, apiKey string) ProbeResult {
	body, err := httpGet(ctx, stripV1(baseURL)+"/get_model_info", apiKey)
	if err != nil {
		return ProbeResult{}
	}
	var r struct {
		ContextLength int `json:"context_length"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ProbeResult{}
	}
	return ProbeResult{ContextWindow: r.ContextLength, Source: "sglang /get_model_info"}
}

// httpGet — short-lived JSON GET helper for the probe path.
func httpGet(ctx context.Context, url, apiKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // probe JSON is tiny; bound the read like the error path
}

// httpPostJSON — short-lived JSON POST helper for the probe path.
func httpPostJSON(ctx context.Context, url, apiKey string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // probe JSON is tiny; bound the read like the error path
}
