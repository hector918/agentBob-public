package core

import (
	"sort"
	"sync"
	"time"
)

// RejectedSender is one inbound sender turned away by a source's closed-default
// authorization (the allowlist gate). It is surfaced in the admin webui so an
// operator bootstrapping a fresh deploy can discover the id they must allowlist
// without grepping logs — the classic chicken-and-egg of "I need my id to allow
// myself, but my message that carries it gets dropped".
type RejectedSender struct {
	Source   string    `json:"source"`
	ChatID   string    `json:"chat_id"`
	UserID   string    `json:"user_id"`
	UserName string    `json:"user_name,omitempty"`
	ChatType ChatType  `json:"chat_type,omitempty"` // dm | group | channel | topic — lets the webui offer "allow group" only for real rooms
	Count    int       `json:"count"`
	LastSeen time.Time `json:"last_seen"`
}

// RejectedSenders is a bounded, deduplicated in-memory log of recently rejected
// inbound senders across all sources. Safe for concurrent use from each
// source's inbound goroutine and the webui snapshot reader.
//
// A nil *RejectedSenders is a valid no-op receiver: Record does nothing, List
// returns nil, ForgetUser does nothing — so a source that wasn't wired with a
// log needn't nil-check.
type RejectedSenders struct {
	mu  sync.Mutex
	max int
	m   map[string]*RejectedSender // key: source|chat|user
}

// NewRejectedSenders returns a log holding at most max distinct
// (source,chat,user) entries (the oldest is evicted when full). max<=0 → 50.
func NewRejectedSenders(max int) *RejectedSenders {
	if max <= 0 {
		max = 50
	}
	return &RejectedSenders{max: max, m: make(map[string]*RejectedSender, max)}
}

func rejKey(source, chatID, userID string) string {
	return source + "|" + chatID + "|" + userID
}

// Record upserts a rejection: a known (source,chat,user) has its Count + LastSeen
// bumped (and UserName refreshed when newly known); a new one is inserted,
// evicting the oldest entry when the log is full. Empty userID is ignored (the
// id is the whole point — nothing to surface without it).
func (r *RejectedSenders) Record(source, chatID, userID, userName string, chatType ChatType) {
	if r == nil || userID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.m[rejKey(source, chatID, userID)]; ok {
		e.Count++
		e.LastSeen = time.Now()
		if userName != "" {
			e.UserName = userName
		}
		if chatType != "" {
			e.ChatType = chatType
		}
		return
	}
	if len(r.m) >= r.max {
		r.evictOldestLocked()
	}
	r.m[rejKey(source, chatID, userID)] = &RejectedSender{
		Source: source, ChatID: chatID, UserID: userID, UserName: userName,
		ChatType: chatType, Count: 1, LastSeen: time.Now(),
	}
}

func (r *RejectedSenders) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range r.m {
		if oldestKey == "" || e.LastSeen.Before(oldest) {
			oldestKey, oldest = k, e.LastSeen
		}
	}
	if oldestKey != "" {
		delete(r.m, oldestKey)
	}
}

// List returns the current rejections, most-recent first.
func (r *RejectedSenders) List() []RejectedSender {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RejectedSender, 0, len(r.m))
	for _, e := range r.m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// ForgetUser drops every entry for (source,userID) across all chats — called
// after the user is allowlisted (the allowlist is per-user/global, so all their
// per-chat rejection rows become stale at once). No-op if absent.
func (r *RejectedSenders) ForgetUser(source, userID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.m {
		if e.Source == source && e.UserID == userID {
			delete(r.m, k)
		}
	}
}

// ForgetChat drops every entry for (source,chatID) across all users — called
// after a group is opened via AllowGroup (the per-chat allow_all override means
// every sender previously rejected in that chat now passes, so all their
// rejection rows are stale). Mirrors ForgetUser for the group-allow path.
func (r *RejectedSenders) ForgetChat(source, chatID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.m {
		if e.Source == source && e.ChatID == chatID {
			delete(r.m, k)
		}
	}
}
