package store

import (
	"reflect"
	"testing"
)

// joinTags/splitTags map the tag list to/from the comma-joined column; ” must
// round-trip to nil (no preference) and stray whitespace/empty segments drop.
func TestTagsRoundTrip(t *testing.T) {
	if got := joinTags(nil); got != "" {
		t.Fatalf("joinTags(nil) = %q, want empty", got)
	}
	if got := splitTags(""); got != nil {
		t.Fatalf("splitTags(\"\") = %v, want nil", got)
	}
	if got := joinTags([]string{"uncensored"}); got != "uncensored" {
		t.Fatalf("joinTags = %q", got)
	}
	if got := splitTags("uncensored"); !reflect.DeepEqual(got, []string{"uncensored"}) {
		t.Fatalf("splitTags = %v", got)
	}
	if got := splitTags(joinTags([]string{"a", "b"})); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("round trip = %v, want [a b]", got)
	}
	if got := splitTags("a, ,b,"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("splitTags dirty = %v, want [a b]", got)
	}
}
