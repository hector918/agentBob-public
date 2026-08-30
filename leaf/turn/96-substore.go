package turn

import (
	"context"
	"sync"

	"agentbob/contract"
)

// subStore is the in-memory contract.MessageStore a delegated sub-turn runs against
// (spec §7 / docs/wip-message-ownership.md §5). A sub-turn's transcript is its
// PRIVATE multi-round working memory: needed during its life for replay, but the
// parent only takes the final product, and a sub-turn is bounded + non-resumable (a
// crash fails the whole turn → the user re-sends). So it lives in process memory, not
// the session's persistent store. Consequence: a hard crash drops it with ZERO orphan
// rows — the old approach wrote child-sid rows into bob_messages and deleted them on
// exit, which leaked when the cleanup defer didn't run on a kill (B-19).
//
// It mirrors PG's replay/compaction semantics exactly so the shared round loop
// behaves identically on a sub-turn: GetReplay anchors on the LAST summary marker
// (§6.9), else the last cap rows; AppendCompactionBatch writes a marker then the
// recent tail re-appended by shape. sessionID is ignored (a subStore serves exactly
// one sub-turn). The mutex guards against the parallel tool-dispatch path even though
// results are appended on the main goroutine.
//
// LOAD-BEARING: every Append* method is pure in-memory and ALWAYS returns nil. runSubTurn
// relies on this — a sub-turn can't hit the round loop's persist-abort exit (which only
// fires on an AppendAssistantCalls failure), so it never reaches OutcomeProcess, which is
// why SubResult.Partial needs no result-side backstop (see runSubTurn). Keep appends
// infallible, or revisit that derivation.
type subStore struct {
	mu   sync.Mutex
	rows []contract.Message
}

func newSubStore() *subStore { return &subStore{} }

func (s *subStore) AppendMessage(_ context.Context, _, role, content string, author contract.MsgAuthor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, contract.Message{Role: role, Content: content, RowKind: contract.RowKindMsg, Author: author})
	return nil
}

func (s *subStore) AppendUserMsgs(_ context.Context, _ string, msgs []contract.UserMsg) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		s.rows = append(s.rows, contract.Message{Role: "user", Content: m.Text, RowKind: contract.RowKindMsg, Author: m.Author})
	}
	return nil
}

func (s *subStore) AppendAssistantCalls(_ context.Context, _, content string, calls []contract.ToolCall, author contract.MsgAuthor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, contract.Message{Role: "assistant", Content: content, ToolCalls: calls, RowKind: contract.RowKindMsg, Author: author})
	return nil
}

func (s *subStore) AppendToolResult(_ context.Context, _, toolCallID, toolName, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, contract.Message{Role: "tool", Content: content, ToolCallID: toolCallID, ToolName: toolName, RowKind: contract.RowKindMsg, Author: contract.MsgAuthor{Kind: "tool", Name: toolName}})
	return nil
}

// AppendCompactionBatch mirrors PG: a summary marker (role=user, RowKind=summary)
// then the recent tail re-appended by shape with RowKind normalised to msg.
func (s *subStore) AppendCompactionBatch(_ context.Context, _, summary string, recent []contract.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, contract.Message{Role: "user", Content: summary, RowKind: contract.RowKindSummary})
	for _, m := range recent {
		m.RowKind = contract.RowKindMsg
		s.rows = append(s.rows, m)
	}
	return nil
}

// RecentAttachments is empty for a sub-turn: its transcript is in-memory scratch and
// carries no persisted attachments (a sub-turn never compacts to re-surface files).
func (s *subStore) RecentAttachments(_ context.Context, _ string, _ int) ([]contract.Attachment, error) {
	return nil, nil
}

func (s *subStore) DeleteSessionMessages(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = nil
	return nil
}

func (s *subStore) GetMessages(_ context.Context, _ string, limit int) ([]contract.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tail(limit), nil
}

// GetReplay anchors on the last summary marker (§6.9); with none, the last cap rows.
func (s *subStore) GetReplay(_ context.Context, _ string, cap int) ([]contract.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.rows) - 1; i >= 0; i-- {
		if s.rows[i].RowKind == contract.RowKindSummary {
			return cloneMsgs(s.rows[i:]), nil
		}
	}
	return s.tail(cap), nil
}

// tail returns a copy of the last n rows (all if n<=0), chronological order.
func (s *subStore) tail(n int) []contract.Message {
	if n <= 0 || n >= len(s.rows) {
		return cloneMsgs(s.rows)
	}
	return cloneMsgs(s.rows[len(s.rows)-n:])
}

func cloneMsgs(in []contract.Message) []contract.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]contract.Message, len(in))
	copy(out, in)
	return out
}

var _ contract.MessageStore = (*subStore)(nil)
