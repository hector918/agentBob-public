package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/config"
)

type sample struct {
	Host  string `yaml:"host"`
	Port  int    `yaml:"port"`
	Quiet bool   `yaml:"quiet"`
}

func defaults() sample { return sample{Host: "localhost", Port: 5432, Quiet: false} }

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml"), defaults())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got != defaults() {
		t.Fatalf("got %+v, want defaults %+v", got, defaults())
	}
}

func TestLoad_OverridesMergeOverDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	// only host + quiet present → port keeps its default.
	if err := os.WriteFile(p, []byte("host: example.com\nquiet: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(p, defaults())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := sample{Host: "example.com", Port: 5432, Quiet: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoad_BadYamlErrorsAndReturnsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("host: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(p, defaults())
	if err == nil {
		t.Fatal("malformed yaml should error")
	}
	if got != defaults() {
		t.Fatalf("on error want defaults, got %+v", got)
	}
}
