package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// tokenizeTimeout bounds one tokenize round trip. Roomy on purpose: the
// payload can be tens of KB and a routing proxy (llama-swap) may add seconds
// — 72KB measured 2.9s warm on the reference deployment — while the CALLER's
// breaker (leaf/turn weigh) already bounds a dead endpoint to one timeout per
// cooldown window, so a tight cap here buys nothing and trips the breaker on
// merely-slow backends.
const tokenizeTimeout = 8 * time.Second

// tokenizers is the single source of provider tokenize capability: CanTokenize
// is a key lookup and CountTokens dispatches through the same map, so the two
// can never drift (a provider added to one but not the other was the failure
// mode this table replaces — the skipped provider would silently weigh on the
// rune estimator forever).
var tokenizers = map[string]func(ctx context.Context, baseURL, apiKey, model, text string) (int, error){
	"llama-cpp": tokenizeLlamaCpp,
	"vllm":      tokenizeVLLM,
}

// CanTokenize reports whether provider has a tokenize endpoint at all — a
// STRUCTURAL fact, not a health check. Callers use it to skip incapable
// entries (openrouter/openai/anthropic) instead of treating them as outages.
func CanTokenize(provider string) bool {
	_, ok := tokenizers[provider]
	return ok
}

// CountTokens asks the backend's own tokenizer for the exact token count of
// text — the ground-truth ruler for prompt sizing (称重制). A non-nil error
// means no tokenize path for this provider or a transport / decode failure —
// the caller falls back to its estimator (and can log WHICH backend failed).
func CountTokens(ctx context.Context, provider, baseURL, apiKey, model, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	fn, ok := tokenizers[provider]
	if !ok {
		return 0, fmt.Errorf("provider %q has no tokenize path", provider)
	}
	tctx, cancel := context.WithTimeout(ctx, tokenizeTimeout)
	defer cancel()
	return fn(tctx, baseURL, apiKey, model, text)
}

// tokenizeLlamaCpp: POST /tokenize {"content": ...} → {"tokens": [...]}.
func tokenizeLlamaCpp(ctx context.Context, baseURL, apiKey, _, text string) (int, error) {
	body, err := httpPostJSON(ctx, stripV1(baseURL)+"/tokenize", apiKey, map[string]string{"content": text})
	if err != nil {
		return 0, fmt.Errorf("llama-cpp /tokenize: %w", err)
	}
	var r struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("llama-cpp /tokenize decode: %w", err)
	}
	if len(r.Tokens) == 0 {
		return 0, fmt.Errorf("llama-cpp /tokenize: empty tokens for non-empty text (wrong response shape?)")
	}
	return len(r.Tokens), nil
}

// tokenizeVLLM: POST /tokenize {"model": ..., "prompt": ...} → {"count": n, "tokens": [...]}.
func tokenizeVLLM(ctx context.Context, baseURL, apiKey, model, text string) (int, error) {
	body, err := httpPostJSON(ctx, stripV1(baseURL)+"/tokenize", apiKey, map[string]any{"model": model, "prompt": text})
	if err != nil {
		return 0, fmt.Errorf("vllm /tokenize: %w", err)
	}
	var r struct {
		Count  int   `json:"count"`
		Tokens []int `json:"tokens"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("vllm /tokenize decode: %w", err)
	}
	if r.Count > 0 {
		return r.Count, nil
	}
	if len(r.Tokens) > 0 {
		return len(r.Tokens), nil
	}
	return 0, fmt.Errorf("vllm /tokenize: empty count and tokens")
}
