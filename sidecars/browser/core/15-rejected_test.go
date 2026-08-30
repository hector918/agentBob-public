package core

import "testing"

func TestRejectedSenders_RecordDedupAndCount(t *testing.T) {
	r := NewRejectedSenders(50)
	r.Record("feishu", "oc_1", "ou_a", "", ChatGroup)
	r.Record("feishu", "oc_1", "ou_a", "Alice", ChatGroup) // same key → bump + name
	r.Record("feishu", "oc_1", "ou_b", "", ChatDM)
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("want 2 distinct senders, got %d", len(list))
	}
	var a *RejectedSender
	for i := range list {
		if list[i].UserID == "ou_a" {
			a = &list[i]
		}
	}
	if a == nil || a.Count != 2 || a.UserName != "Alice" {
		t.Fatalf("ou_a = %+v, want count 2 + name Alice", a)
	}
	if a.ChatType != ChatGroup {
		t.Fatalf("ou_a ChatType = %q, want group", a.ChatType)
	}
}

// TestRejectedSenders_ChatTypeRefreshed: a later record with a known chat type
// fills in an entry first seen with an empty one (mirrors the UserName refresh).
func TestRejectedSenders_ChatTypeRefreshed(t *testing.T) {
	r := NewRejectedSenders(50)
	r.Record("telegram", "-100", "u1", "", "")
	r.Record("telegram", "-100", "u1", "", ChatGroup)
	list := r.List()
	if len(list) != 1 || list[0].ChatType != ChatGroup {
		t.Fatalf("ChatType not refreshed: %+v", list)
	}
}

func TestRejectedSenders_NilAndEmptyUserNoop(t *testing.T) {
	var r *RejectedSenders // nil receiver
	r.Record("feishu", "oc_1", "ou_a", "", ChatDM)
	if r.List() != nil {
		t.Fatal("nil receiver List should be nil")
	}
	r2 := NewRejectedSenders(50)
	r2.Record("feishu", "oc_1", "", "", ChatGroup) // empty user id ignored
	if len(r2.List()) != 0 {
		t.Fatal("empty user id should not be recorded")
	}
}

func TestRejectedSenders_ForgetUserAllChats(t *testing.T) {
	r := NewRejectedSenders(50)
	r.Record("feishu", "oc_1", "ou_a", "", ChatGroup)
	r.Record("feishu", "oc_2", "ou_a", "", ChatGroup) // same user, different chat
	r.Record("feishu", "oc_1", "ou_b", "", ChatGroup)
	r.ForgetUser("feishu", "ou_a")
	list := r.List()
	if len(list) != 1 || list[0].UserID != "ou_b" {
		t.Fatalf("after ForgetUser(ou_a): want only ou_b, got %+v", list)
	}
}

func TestRejectedSenders_EvictOldestWhenFull(t *testing.T) {
	r := NewRejectedSenders(2)
	r.Record("s", "c", "u1", "", ChatGroup)
	r.Record("s", "c", "u2", "", ChatGroup)
	r.Record("s", "c", "u3", "", ChatGroup) // evicts u1 (oldest)
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("cap 2: want 2 entries, got %d", len(list))
	}
	for _, e := range list {
		if e.UserID == "u1" {
			t.Fatal("u1 should have been evicted as oldest")
		}
	}
}
