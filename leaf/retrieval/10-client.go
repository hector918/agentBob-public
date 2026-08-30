package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"agentbob/contract"
)

// Client is the HTTP client for the external cold-memory leaf (sidecars/chat-retrieval).
// It implements contract.RetrievalClient (Retrieve) and carries the write path
// (Ingest) for the in-package feeder. Safe for concurrent use.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var _ contract.RetrievalClient = (*Client)(nil)

// NewClient returns a Client posting to baseURL with the given per-call timeout.
// apiKey, when non-empty, is sent as X-API-Key on every request.
func NewClient(baseURL string, timeout time.Duration, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

// Retrieve POSTs a query to /retrieve and returns the raw JSON response for the
// caller (recall_memory tool) to shape. Fail-open: any error means skip.
func (c *Client) Retrieve(ctx context.Context, q contract.RetrievalQuery) (json.RawMessage, error) {
	params := q.Params
	if params == nil {
		params = map[string]any{}
	}
	req := map[string]any{
		"query_type": q.QueryType,
		"params":     params,
		"limit":      q.Limit,
	}
	if q.Filters != nil {
		req["filters"] = q.Filters
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("retrieval: marshal query: %w", err)
	}
	out, status, err := c.do(ctx, http.MethodPost, "/retrieve", body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("retrieval: POST /retrieve → %d: %s", status, snippet(out))
	}
	return out, nil
}

// ingestMsg is the leaf's MessageIn JSON shape (sidecars/chat-retrieval
// app/ingestion/handler.py). omitempty on optional fields so NULLs aren't sent as
// empty strings.
type ingestMsg struct {
	MessageID string `json:"message_id"`
	CompanyID string `json:"company_id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"` // = source_handle
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Lang      string `json:"lang,omitempty"`
	ChannelID string `json:"channel_id,omitempty"` // = room (group scope / agora inbox)
}

// Ingest pushes outbox rows to POST /messages/batch. The leaf is idempotent
// (ON CONFLICT DO NOTHING) so a retried batch is safe. Returns ErrPermanent for a
// content-fault rejection (400/422 — what the leaf actually emits for true content
// faults) so the feeder dead-letters instead of blocking the FIFO outbox forever;
// other non-2xx are transient (left for retry).
func (c *Client) Ingest(ctx context.Context, rows []outboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	msgs := make([]ingestMsg, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Content) == "" {
			continue // empty content → the embedder rejects it (would fail the whole batch)
		}
		msgs = append(msgs, ingestMsg{
			MessageID: r.MessageID,
			CompanyID: r.CompanyID,
			SessionID: r.SessionID,
			UserID:    r.UserID,
			Timestamp: unixToRFC3339(r.Ts),
			Role:      r.Role,
			Content:   r.Content,
			Lang:      r.Lang,
			ChannelID: r.ChannelID,
		})
	}
	if len(msgs) == 0 {
		return nil // whole batch empty-content → report success so the feeder drops them
	}
	body, err := json.Marshal(map[string]any{"messages": msgs})
	if err != nil {
		return fmt.Errorf("retrieval: marshal ingest: %w", err)
	}
	out, status, err := c.do(ctx, http.MethodPost, "/messages/batch", body)
	if err != nil {
		return err // transport failure — transient, leave rows for retry
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if isPermanentStatus(status) {
		return fmt.Errorf("%w: POST /messages/batch → %d: %s", ErrPermanent, status, snippet(out))
	}
	return fmt.Errorf("retrieval: POST /messages/batch → %d: %s", status, snippet(out))
}

// isPermanentStatus reports whether an HTTP status means retrying the SAME batch
// will never succeed — a CONTENT fault the LEAF ITSELF emits (400 malformed / 422
// unprocessable). 413 is deliberately NOT here: the leaf never emits it (it wraps
// its own oversize/embedder faults as 500=transient), so a 413 at the bob↔leaf
// boundary comes from an intermediary proxy (e.g. nginx client_max_body_size) below
// bob's batch size — a deployment-fixable misconfig. Like other config faults
// (401/403/404/413/…) it stays in the queue (back off), so a proxy misconfig can't
// silently discard un-backfillable append-only corpus.
func isPermanentStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// do issues one request and returns (body, statusCode, err). It reads the FULL
// body — a /retrieve response can exceed any small cap and truncation would hand
// the recall tool malformed JSON; the leaf is a trusted internal LAN service. err
// is non-nil only for transport/build failures, not HTTP status.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("retrieval: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("retrieval: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("retrieval: read %s response: %w", path, rerr)
	}
	return out, resp.StatusCode, nil
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if utf8.RuneCountInString(s) > max {
		return truncRunes(s, max) + "…"
	}
	return s
}

// unixToRFC3339 converts fractional unix seconds to the RFC3339 the leaf expects.
func unixToRFC3339(ts float64) string {
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339Nano)
}
