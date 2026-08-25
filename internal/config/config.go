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
	"sort"
	"strings"
)

type Config struct {
	Schema      string                `json:"$schema,omitempty"`
	Connections map[string]Connection `json:"connections"`
	// Muninn-specific, written by `sync --install` / first run:
	Auth   AuthConfig   `json:"auth,omitempty"`
	Ctags  CtagsConfig  `json:"ctags,omitempty"`
	Sync   SyncConfig   `json:"sync,omitempty"`
	Editor EditorConfig `json:"editor,omitempty"`
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

type EditorConfig struct {
	// Scheme is the URL scheme for open-in-editor links ("cursor" or
	// "vscode"); default "cursor".
	Scheme string `json:"scheme,omitempty"`
	// Roots are directories scanned (2 levels deep) for local git
	// checkouts of indexed repos, e.g. "~/dev".
	Roots []string `json:"roots,omitempty"`
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
	if s := c.Editor.Scheme; s != "" && s != "cursor" && s != "vscode" {
		return fmt.Errorf("editor.scheme %q: must be \"cursor\" or \"vscode\"", s)
	}
	return nil
}

func applyDefaults(c *Config) {
	if c.Sync.IntervalMinutes <= 0 {
		c.Sync.IntervalMinutes = defaultSyncIntervalMinutes
	}
	if c.Editor.Scheme == "" {
		c.Editor.Scheme = "cursor"
	}
	for i, root := range c.Editor.Roots {
		c.Editor.Roots[i] = expandTilde(root)
	}
}

// expandTilde expands a leading "~/" (or a bare "~") to the current
// user's home directory. Paths without the prefix are returned unchanged,
// as are paths when the home directory cannot be determined.
func expandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// ResolveToken returns the GitHub token using the following precedence:
// auth.githubToken from the config, env vars named by connections' token.env
// (in sorted connection-name order for determinism), then `gh auth token`.
func ResolveToken(c *Config) (string, error) {
	if c.Auth.GitHubToken != "" {
		return c.Auth.GitHubToken, nil
	}

	names := make([]string, 0, len(c.Connections))
	for name := range c.Connections {
		names = append(names, name)
	}
	sort.Strings(names)

	var envName string
	for _, name := range names {
		conn := c.Connections[name]
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

	var ghDetail string
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			ghDetail = fmt.Sprintf(" (`gh auth token` failed: %s)", stderr)
		}
	}

	hint := "set a token env var referenced by a connection's token.env, or install the GitHub CLI (gh) and run `gh auth login`"
	if envName != "" {
		hint = fmt.Sprintf("set the %s environment variable, or install the GitHub CLI (gh) and run `gh auth login`", envName)
	}
	return "", fmt.Errorf("no GitHub token found: %s%s", hint, ghDetail)
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
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp config file: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("setting config permissions: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("syncing config: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming config into place: %w", err)
	}
	return nil
}
