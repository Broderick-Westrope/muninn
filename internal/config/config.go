// Package config loads and saves the muninn configuration, a
// Sourcebot-compatible subset of the v3 schema.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Config struct {
	Schema      string                `json:"$schema,omitempty"`
	Connections map[string]Connection `json:"connections"`
	// Muninn-specific, written by `sync --install` / first run:
	Auth  AuthConfig  `json:"auth,omitempty"`
	Ctags CtagsConfig `json:"ctags,omitempty"`
	Sync  SyncConfig  `json:"sync,omitempty"`
}

type Connection struct {
	Type    string    `json:"type"` // "github" only in v1
	Orgs    []string  `json:"orgs,omitempty"`
	Repos   []string  `json:"repos,omitempty"` // ad-hoc "owner/name" additions
	Token   *TokenRef `json:"token,omitempty"`
	Exclude *Exclude  `json:"exclude,omitempty"`
}

type TokenRef struct {
	Env string `json:"env,omitempty"`
}

type Exclude struct {
	Archived bool     `json:"archived,omitempty"`
	Repos    []string `json:"repos,omitempty"`
}

type AuthConfig struct {
	GitHubToken string `json:"githubToken,omitempty"`
}

type CtagsConfig struct {
	Path string `json:"path,omitempty"`
}

type SyncConfig struct {
	IntervalMinutes int `json:"intervalMinutes,omitempty"`
}

const defaultSyncIntervalMinutes = 60

// Load reads, parses, validates, and applies defaults to the config at path.
// Unknown top-level fields (e.g. Sourcebot's "models") are ignored.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := validate(&c); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	applyDefaults(&c)
	return &c, nil
}

func validate(c *Config) error {
	if len(c.Connections) == 0 {
		return errors.New("no connections defined")
	}
	hasTarget := false
	for name, conn := range c.Connections {
		if conn.Type != "github" {
			return fmt.Errorf("connection %q: unsupported type %q (only \"github\" is supported)", name, conn.Type)
		}
		if len(conn.Orgs) > 0 || len(conn.Repos) > 0 {
			hasTarget = true
		}
	}
	if !hasTarget {
		return errors.New("no orgs or repos configured across connections")
	}
	return nil
}

func applyDefaults(c *Config) {
	if c.Sync.IntervalMinutes <= 0 {
		c.Sync.IntervalMinutes = defaultSyncIntervalMinutes
	}
}

// ResolveToken returns the GitHub token using the following precedence:
// auth.githubToken from the config, the env var named by the first
// connection's token.env, then `gh auth token`.
func ResolveToken(c *Config) (string, error) {
	if c.Auth.GitHubToken != "" {
		return c.Auth.GitHubToken, nil
	}

	var envName string
	for _, conn := range c.Connections {
		if conn.Token != nil && conn.Token.Env != "" {
			envName = conn.Token.Env
			if v := os.Getenv(envName); v != "" {
				return v, nil
			}
		}
	}

	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil {
		if token := strings.TrimSpace(string(out)); token != "" {
			return token, nil
		}
	}

	hint := "set a token env var referenced by a connection's token.env, or install the GitHub CLI (gh) and run `gh auth login`"
	if envName != "" {
		hint = fmt.Sprintf("set the %s environment variable, or install the GitHub CLI (gh) and run `gh auth login`", envName)
	}
	return "", fmt.Errorf("no GitHub token found: %s", hint)
}

// Save writes the config to path atomically with 0600 permissions.
func Save(path string, c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming config into place: %w", err)
	}
	return nil
}
