package modelgate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"agentbob/contract"
)

// server holds the request-time dependencies: the model pool (routing target) and
// the API-key authority (resolved lazily — accounts may Start after modelgate, and
// is absent → every request 401s). There is no concurrency gate here: capacity is
// the pool's business (see handleChat).
type server struct {
	pool contract.ModelPool
	keys func() contract.APIKeys // lazy; nil result → fail-closed (401)
	// images is the shared style catalog (summaries + prompt manuals). Lazy and
	// optional for the same reason as keys: absent → the images endpoints report
	// that no style is available rather than 500ing.
	images func() contract.ImageCatalog
}

// keyAuth resolves the optional API-key authority. nil when accounts is absent /
// down — the caller then fails every request closed (401), never open.
func (s *server) keyAuth() contract.APIKeys {
	if s.keys == nil {
		return nil
	}
	return s.keys()
}

// routes builds the OpenAI-compatible mux. The two OpenAI v1 endpoints plus the
// bob-specific /v1/key runtime introspection; everything else 404s.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/models/", s.handleModel)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/key", s.handleKeyInfo)
	mux.HandleFunc("/v1/images/generations", s.handleImageGenerations)
	mux.HandleFunc("/v1/images/edits", s.handleImageEdits)
	return mux
}

// authenticate extracts the Bearer token and verifies it. On failure it writes the
// OpenAI 401 error and returns ok=false — the handler returns immediately.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (contract.APIKeyInfo, bool) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing bearer token", "")
		return contract.APIKeyInfo{}, false
	}
	auth := s.keyAuth()
	if auth == nil {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "key verification unavailable", "")
		return contract.APIKeyInfo{}, false
	}
	info, ok := auth.VerifyKey(r.Context(), token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid API key", "invalid_api_key")
		return contract.APIKeyInfo{}, false
	}
	return info, true
}

// bearerToken pulls the token out of "Authorization: Bearer <token>" (case-
// insensitive scheme). "" when absent or malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const pfx = "bearer "
	if len(h) < len(pfx) || !strings.EqualFold(h[:len(pfx)], pfx) {
		return ""
	}
	return strings.TrimSpace(h[len(pfx):])
}

// --- OpenAI error envelope ------------------------------------------------

type errEnvelope struct {
	Error errDetail `json:"error"`
}

type errDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeError renders the OpenAI error JSON at the given status. code may be "".
func writeError(w http.ResponseWriter, status int, typ, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errEnvelope{Error: errDetail{Message: message, Type: typ, Code: code}})
}

// writeJSON renders v as a 200 JSON body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// maxRequestBytes caps a chat request body — a coarse post-auth DoS guard so a
// (valid-key) caller can't force an unbounded buffer/parse. 16 MiB is far above any
// real conversation (~4M tokens of JSON) yet bounded.
const maxRequestBytes = 16 << 20

// decodeChatRequest reads + parses the request body under a size cap. A parse error
// (including an over-cap body via MaxBytesReader) writes the 400 and returns ok=false.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (chatRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "could not parse request body: "+err.Error(), "")
		return chatRequest{}, false
	}
	return req, true
}

// ctxWithConsumer stamps the key's billing handle on ctx so the pool's per-handle
// ledger books this request's tokens to the key's account.
func ctxWithConsumer(ctx context.Context, keyID string) context.Context {
	return contract.WithConsumer(ctx, apiKeyBillingSource, keyID)
}

// apiKeyBillingSource is the source-family the key's spend is billed under — the
// single contract truth accounts also binds its billing handle under, so the two
// meet in the usage ledger with no leaf-to-leaf import.
const apiKeyBillingSource = contract.APIKeyBillingSource
