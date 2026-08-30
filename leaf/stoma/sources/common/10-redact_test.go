package common

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactErr(t *testing.T) {
	if RedactErr(nil, "tok") != nil {
		t.Fatal("nil error must stay nil")
	}
	// secret present → replaced.
	err := errors.New("post https://api/bot123:SECRET/send failed")
	got := RedactErr(err, "123:SECRET").Error()
	if strings.Contains(got, "123:SECRET") || !strings.Contains(got, "<redacted>") {
		t.Fatalf("redacted = %q, want the secret gone", got)
	}
	// no secret → original error returned unchanged (type identity kept).
	plain := errors.New("connection refused")
	if RedactErr(plain, "tok") != plain {
		t.Fatal("an error with no secret must be returned unchanged")
	}
}
