package warrant

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// spaceBackend declares where a space physically lives. backend "" / "local" =
// a local dir under $BOB_HOME/spaces; "ssh" = a remote host reached via the
// named credential. (sftp file ops on a remote space are a followup — needs the
// sftp dep; remote exec works over ssh now.)
type spaceBackend struct {
	Backend    string `yaml:"backend"`
	Credential string `yaml:"credential"`
}

// spaceConfig maps space name → backend. Unlisted spaces are local. Loaded from
// $BOB_HOME/spaces.yaml.
type spaceConfig struct {
	spaces map[string]spaceBackend
}

func loadSpaces(home string) (*spaceConfig, error) {
	c := &spaceConfig{spaces: map[string]spaceBackend{}}
	raw, err := os.ReadFile(filepath.Join(home, "spaces.yaml"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Spaces map[string]spaceBackend `yaml:"spaces"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	// Validate every declared backend: an unknown / typo backend (or a "credential:"
	// set with a local/empty backend) MUST fail closed, never fall through to the
	// local branch — a remote space that silently runs in a local empty dir and
	// skips the credential gate is the worst outcome (see 00-module.go fail-closed
	// posture; the caller sets spacesBroken on this error).
	for name, be := range f.Spaces {
		switch be.Backend {
		case "", "local":
			if be.Credential != "" {
				return nil, fmt.Errorf("warrant: space %q has credential set but backend %q is local — a credential requires a remote backend", name, be.Backend)
			}
		case "ssh":
			// remote — credential is gated at exec time (empty is caught there).
		default:
			return nil, fmt.Errorf("warrant: space %q has unknown backend %q (want one of: local, ssh)", name, be.Backend)
		}
	}
	if f.Spaces != nil {
		c.spaces = f.Spaces
	}
	return c, nil
}

// backend returns the space's backend (defaulting to local).
func (c *spaceConfig) backend(space string) spaceBackend {
	if c != nil {
		if b, ok := c.spaces[space]; ok && b.Backend != "" {
			return b
		}
	}
	return spaceBackend{Backend: "local"}
}
