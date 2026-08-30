package providers

import (
	"agentbob/contract"
)

// NewChatter builds the right contract.Chatter for a provider name (already
// lowercased+trimmed by buildEntry). Anthropic gets the native Messages-API
// client; rapidocr maps onto the OCR sidecar's /ocr endpoint (KindOCR); comfyui
// maps onto a ComfyUI server's async prompt/history/view trio (KindImage);
// everything else (openai, openrouter, ollama, llama-cpp, vllm, sglang,
// custom, unknown) goes through the OpenAI-compat /chat/completions client.
func NewChatter(provider, baseURL, apiKey string, maxTokens int, extraBody map[string]any) contract.Chatter {
	switch provider {
	case "anthropic":
		return NewAnthropicClient(baseURL, apiKey, maxTokens, extraBody)
	case "rapidocr":
		return NewRapidOCRClient(baseURL)
	case "comfyui":
		return NewComfyUIClient(baseURL, extraBody)
	default:
		return NewClient(baseURL, apiKey, maxTokens, extraBody)
	}
}
