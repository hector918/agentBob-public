package providers

// Profile is a provider preset: a default OpenAI-compatible base URL and the
// name of the env var that holds its API key (empty if the backend needs none).
//
// DefaultContextWindow is the fallback context window used when YAML doesn't
// set it AND the per-provider probe yields nothing. It exists for backends
// whose API does not expose context_window (Anthropic /v1/models returns the
// model list but not capability metadata, so we hardcode the family-wide 200K).
// Zero means "no opinion" — the MultiPool falls through to the global default.
type Profile struct {
	BaseURL              string // default ".../v1" root; "" → caller must set model.base_url
	APIKeyEnv            string // env var holding the key; "" → no key needed
	DefaultContextWindow int    // 0 → fall through to global fallback
}

// Profiles maps a provider name to its preset. Unknown names are treated as
// "custom" (no defaults — base_url must be configured), so users can name their
// own endpoints freely.
var Profiles = map[string]Profile{
	"ollama":     {BaseURL: "http://localhost:11434/v1"},
	"llama-cpp":  {BaseURL: "http://localhost:8080/v1"},
	"vllm":       {BaseURL: "http://localhost:8000/v1"},
	"sglang":     {BaseURL: "http://localhost:30000/v1"},
	"openai":     {BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	"anthropic":  {BaseURL: "https://api.anthropic.com/v1", APIKeyEnv: "ANTHROPIC_API_KEY", DefaultContextWindow: 200000},
	"rapidocr":   {}, // no defaults — set base_url to your OCR sidecar (e.g. http://ocr:11500)
	"comfyui":    {}, // no defaults — set base_url to the ComfyUI root (e.g. http://host:8080/klein)
	"custom":     {}, // no defaults — set model.base_url in config
}
