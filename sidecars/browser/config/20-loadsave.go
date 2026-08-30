package config

import (
	"bufio"
	"os"
	"strings"

	"agentbob/sidecars/browser/bobhome"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// LoadDotEnv reads $BOB_HOME/.env (KEY=VALUE lines, "#" comments, optional "export ")
// into the process environment. Existing environment variables are NOT overwritten.
// A missing file is not an error.
func LoadDotEnv() error {
	f, err := os.Open(bobhome.Path(".env"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k == "" {
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// Load reads $BOB_HOME/config.yaml merged over Default(). A missing file → defaults.
func Load() (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(bobhome.Path("config.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Update runs Load → cb(cfg) → Save under an exclusive flock on
// $BOB_HOME/config.yaml.lock. This serializes concurrent writers (e.g.
// `bob config set` racing a startup migration) so updates aren't lost.
//
// The lockfile is a 0-byte sentinel created on demand and never deleted.
// Save() itself does not take the lock — Update is the only path that does.
func Update(cb func(*Config) error) error {
	if err := os.MkdirAll(bobhome.Home(), 0o755); err != nil {
		return err
	}
	lockPath := bobhome.Path("config.yaml.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)

	cfg, err := Load()
	if err != nil {
		return err
	}
	if err := cb(cfg); err != nil {
		return err
	}
	return Save(cfg)
}

// Save writes the config to $BOB_HOME/config.yaml (atomic: temp file + rename).
func Save(cfg *Config) error {
	if err := os.MkdirAll(bobhome.Home(), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := bobhome.Path("config.yaml.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, bobhome.Path("config.yaml"))
}
