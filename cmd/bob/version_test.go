package main

import (
	"os"
	"strings"
	"testing"
)

func TestRecordVersion(t *testing.T) {
	version = "vtest"
	d := t.TempDir()
	line := recordVersion(d)
	if !strings.HasPrefix(line, "vtest (booted ") {
		t.Fatalf("line=%q", line)
	}
	b, err := os.ReadFile(d + "/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "vtest (booted ") {
		t.Fatalf("file=%q", b)
	}
}
