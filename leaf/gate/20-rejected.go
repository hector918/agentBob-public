package gate

import (
	"sort"
	"sync"
	"time"

	"agentbob/contract"
)

// bounceReplyTTL caps how often a rejected sender is re-sent the bounce notice:
// once, then silence for this long, then one more on the first message past it.
// Short enough that someone who missed the first notice isn't stuck waiting,
// long enough that a chatty stranger can't make the bot spam the room.
const bounceReplyTTL = 2 * time.Minute

// RejectedSender is one inbound sender turned away by a source's closed-default
// authorization (the allowlist gate). It is surfaced in the admin webui so an
// operator bootstrapping a fresh deploy can discover the id they must allowlist
// without grepping logs — the classic chicken-and-egg of "I need my id to allow
// myself, but the message that carries it gets dropped".
type RejectedSender struct {
	Source    string
	ChatID    string
	UserID    string
	UserName  string
	ChatType  contract.ChatType // lets the webui offer "allow group" only for real rooms
	Count     int
	FirstSeen time.Time // when first rejected; the eviction key (S-23)
	LastSeen  time.Time
	// LastReplied is when the gate last sent this sender the bounce notice. Record
	// re-arms a reply once now-LastReplied >= bounceReplyTTL, so a sender who returns
	// after the cooldown is re-greeted (with their token) instead of being ignored
	// forever — without re-replying to every message in the window.
	LastReplied time.Time
	// Token is the admit code carried in the bounce notice — a claim token whose batch
	// admits this sender (contract.BatchPayload, /gate allow … frozen to the applicant +
	// scope). A real admin redeems it by pasting the code (the batch's /gate allow is
	// AdminOnly). It is safe to show in a public group: the grant target is frozen at mint,
	// and an ineligible redeemer (non-admin) commits nothing and falls through to normal
	// gating with the token intact. It is load-bearing for the retire-previous dedupe:
	// SetToken returns the prior value so the caller can Consume the stale token, keeping
	// at most one live admit token per (source,chat,user) (D7). Lives as long as the entry
	// (cap / eviction / restart).
	Token string
}

// RejectedSenders is a bounded, deduplicated in-memory log of recently rejected
// inbound senders across all sources. Safe for concurrent use from each
// source's inbound goroutine and the webui snapshot reader. A nil
// *RejectedSenders is a valid no-op receiver.
type RejectedSenders struct {
	mu  sync.Mutex
	max int
	m   map[string]*RejectedSender // key: source|chat|user
	now func() time.Time           // injectable clock (tests); defaults to time.Now
}

// NewRejectedSenders returns a log holding at most max distinct
// (source,chat,user) entries (the oldest is evicted when full). max<=0 → 50.
func NewRejectedSenders(max int) *RejectedSenders {
	if max <= 0 {
		max = 50
	}
	return &RejectedSenders{max: max, m: make(map[string]*RejectedSender, max), now: time.Now}
}

func rejKey(source, chatID, userID string) string {
	return source + "|" + chatID + "|" + userID
}

// Record upserts a rejection and reports whether the caller should send the bounce
// notice NOW (the caller mints/embeds the admit token via the claimtoken facility and
// SetTokens it so the retire-previous dedupe can Consume any prior token). A new
// (source,chat,user) → shouldReply=true. A known
// sender bumps Count + LastSeen, and re-arms shouldReply only once now-LastReplied >=
// bounceReplyTTL — so repeats inside the window are silent (no reply-amplification) but
// a sender who returns later is re-greeted. Empty userID is ignored (the id is the
// whole point — nothing to surface without it).
func (r *RejectedSenders) Record(source, chatID, userID, userName string, chatType contract.ChatType) (shouldReply bool) {
	if r == nil || userID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if e, ok := r.m[rejKey(source, chatID, userID)]; ok {
		e.Count++
		e.LastSeen = now
		if userName != "" {
			e.UserName = userName
		}
		if chatType != "" {
			e.ChatType = chatType
		}
		if now.Sub(e.LastReplied) >= bounceReplyTTL {
			e.LastReplied = now
			return true
		}
		return false
	}
	if len(r.m) >= r.max {
		r.evictOldestLocked()
	}
	r.m[rejKey(source, chatID, userID)] = &RejectedSender{
		Source: source, ChatID: chatID, UserID: userID, UserName: userName,
		ChatType: chatType, Count: 1, FirstSeen: now, LastSeen: now,
		LastReplied: now,
	}
	return true
}

// SetToken stamps the entry's current admit token (a claimtoken-facility token the
// screen minted) and returns the token it replaces ("" if none) so the caller can
// retire the old one in the facility (prev → tokens.Consume) — at most one live admit
// token per (source,chat,user) (D7). found=false if the entry is gone.
// The swap happens under r.mu, so concurrent bounces can't both read the same prev.
func (r *RejectedSenders) SetToken(source, chatID, userID, token string) (prev string, found bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.m[rejKey(source, chatID, userID)]; ok {
		prev = e.Token
		e.Token = token
		return prev, true
	}
	return "", false
}

// evictOldestLocked drops the entry with the oldest FirstSeen — the longest-
// standing rejection, which is the stale noise (a sender rejected since long ago);
// recently-DISCOVERED ids (the ones an operator is here to allowlist) are kept.
// Evicting by LastSeen instead let a noisy repeat-rejector (bumping LastSeen) pin
// itself while the one-time bootstrap id aged out — the opposite of the feed's
// purpose (S-23).
func (r *RejectedSenders) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range r.m {
		if oldestKey == "" || e.FirstSeen.Before(oldest) {
			oldestKey, oldest = k, e.FirstSeen
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
// after the user is allowlisted (the allowlist is per-user, so all their
// per-chat rejection rows become stale at once). No-op if absent. The user id
// compares per idEqual (10-policy.go): exact for platform ids, case-insensitive
// for @-carrying email addresses — an admin who allowlists a mixed-case address
// must still clear the lowercase feed row the email source recorded (F116).
func (r *RejectedSenders) ForgetUser(source, userID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.m {
		if e.Source == source && idEqual(e.UserID, userID) {
			delete(r.m, k)
		}
	}
}

// ForgetUserInChat drops the rejected record for exactly one (source, chatID, user)
// — the scope of a per-chat admit grant. A sender rejected in several chats keeps
// their OTHER chats' feed rows, so an admin who admitted them in ONE chat still sees
// them queued in the chats where they remain unauthorized. (L-gate-D1)
// The user id compares per idEqual (case-insensitive for email addresses, same as
// ForgetUser); the map key is a concatenated string, so this scans instead of a
// rejKey lookup — the map is small (max entries, default 50).
func (r *RejectedSenders) ForgetUserInChat(source, chatID, userID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.m {
		if e.Source == source && e.ChatID == chatID && idEqual(e.UserID, userID) {
			delete(r.m, k)
		}
	}
}
