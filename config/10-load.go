package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads the yaml file at path into a copy of defaults and returns the
// merged result. A missing file is NOT an error — it returns defaults unchanged
// (config files carry overrides only). Fields absent from a present file keep
// their default values. On a read or parse error, the defaults are returned
// alongside the error.
//
// Merge granularity follows yaml.v3: scalar and struct fields override
// field-by-field, and maps merge key-by-key — but a SLICE present in the file
// REPLACES the default slice wholesale (it is not appended/merged). So a module
// with a non-empty default slice must list the full set in its file, not just
// additions.
//
// CAVEAT: `cfg := defaults` is a SHALLOW copy, and yaml.v3 decodes a mapping into an
// existing non-nil map IN PLACE. So defaults must NOT contain pre-populated map fields —
// yaml.v3 would merge the file's keys into the shared default map, mutating it across
// calls. Current callers are safe (nil / struct-only map fields).
func Load[T any](path string, defaults T) (T, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaults, nil
	}
	if err != nil {
		return defaults, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := defaults
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return defaults, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}
