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
	"time"
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
	// GitHubToken is a token supplied by the user. It is never written
	// here by muninn: a frozen token expires and takes every scheduled
	// run down with it, so scheduled runs resolve a token per run.
	GitHubToken string `json:"githubToken,omitempty"`
	// GHPath is the absolute path to the GitHub CLI, recorded at install
	// time for the same reason as Ctags.Path: launchd's PATH excludes
	// Homebrew. Storing the path rather than the token is what keeps
	// scheduled runs working across token rotation and expiry.
	GHPath string `json:"ghPath,omitempty"`
}

type CtagsConfig struct {
	Path string `json:"path,omitempty"`
}

type SyncConfig struct {
	IntervalMinutes int `json:"intervalMinutes,omitempty"`
	// RetryIntervalMinutes is the cadence used while the index is stale and
	// the last run failed, so a transient failure is reattempted in minutes
	// instead of costing a full interval. Clamped to IntervalMinutes.
	RetryIntervalMinutes int `json:"retryIntervalMinutes,omitempty"`
}

// Interval is how old a successful sync may be before another is due.
func (s SyncConfig) Interval() time.Duration {
	return time.Duration(s.IntervalMinutes) * time.Minute
}

// RetryInterval is the reattempt cadence while the index is stale and the
// last run failed.
func (s SyncConfig) RetryInterval() time.Duration {
	return time.Duration(s.RetryIntervalMinutes) * time.Minute
}

// TickMinutes is how often the scheduler must wake to be able to honor both
// cadences. The scheduled job runs with --if-due, so most ticks are a
// status read and an immediate exit; the due check, not the tick, decides
// whether work happens.
func (s SyncConfig) TickMinutes() int {
	return min(s.IntervalMinutes, s.RetryIntervalMinutes)
}

type EditorConfig struct {
	// Scheme is the URL scheme for open-in-editor links ("cursor" or
	// "vscode"); default "cursor".
	Scheme string `json:"scheme,omitempty"`
	// Roots are directories scanned (2 levels deep) for local git
	// checkouts of indexed repos, e.g. "~/dev".
	Roots []string `json:"roots,omitempty"`
}

const (
	defaultSyncIntervalMinutes   = 60
	defaultSyncRetryIntervalMins = 10
)

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
	if c.Sync.RetryIntervalMinutes <= 0 {
		c.Sync.RetryIntervalMinutes = defaultSyncRetryIntervalMins
	}
	// A retry slower than the normal cadence would make failures back off
	// rather than reattempt, inverting the point of the setting.
	if c.Sync.RetryIntervalMinutes > c.Sync.IntervalMinutes {
		c.Sync.RetryIntervalMinutes = c.Sync.IntervalMinutes
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

// Token source names reported by ResolveTokenSource.
const (
	TokenSourceConfig = "config"
	TokenSourceEnv    = "env"
	TokenSourceGH     = "gh"
)

// Token is a resolved GitHub token and its provenance. The source matters
// at install time: only a token resolved per run via the GitHub CLI keeps
// working after rotation, whereas one read from the shell environment is
// invisible to a launchd job.
type Token struct {
	Value string
	// Source is one of TokenSourceConfig, TokenSourceEnv, TokenSourceGH.
	Source string
	// EnvName is the environment variable consulted, set when Source is
	// TokenSourceEnv or when no token was found but a connection named
	// one.
	EnvName string
	// GHPath is the resolved GitHub CLI path, set when Source is
	// TokenSourceGH.
	GHPath string
}

// ghProbeCandidates are tried in order when auth.ghPath is not configured.
// Absolute Homebrew paths are included because launchd's default PATH
// excludes them, mirroring the ctags probe.
var ghProbeCandidates = []string{
	"gh",
	"/opt/homebrew/bin/gh",
	"/usr/local/bin/gh",
}

// ResolveToken returns the GitHub token using the following precedence:
// auth.githubToken from the config, env vars named by connections' token.env
// (in sorted connection-name order for determinism), then `gh auth token`.
func ResolveToken(c *Config) (string, error) {
	tok, err := ResolveTokenSource(c)
	return tok.Value, err
}

// ResolveTokenSource resolves a token and reports where it came from.
//
// GHPath is filled in whenever the GitHub CLI can be located, even when a
// higher-precedence source won the token. Install persists it, so a stored
// token that later expires can be recovered from by deleting that one
// field rather than reinstalling the agent.
func ResolveTokenSource(c *Config) (Token, error) {
	ghPath, ghErr := resolveGH(c.Auth.GHPath)

	if c.Auth.GitHubToken != "" {
		return Token{Value: c.Auth.GitHubToken, Source: TokenSourceConfig, GHPath: ghPath}, nil
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
				return Token{Value: v, Source: TokenSourceEnv, EnvName: envName, GHPath: ghPath}, nil
			}
		}
	}

	err := ghErr
	if err == nil {
		var out []byte
		out, err = exec.Command(ghPath, "auth", "token").Output()
		if err == nil {
			if token := strings.TrimSpace(string(out)); token != "" {
				return Token{Value: token, Source: TokenSourceGH, GHPath: ghPath}, nil
			}
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
	return Token{EnvName: envName}, fmt.Errorf("no GitHub token found: %s%s", hint, ghDetail)
}

// resolveGH returns a path to the GitHub CLI, validating a configured path
// directly or probing well-known names and Homebrew locations. Failures are
// collected per candidate, as ctags.Resolve does, so a wrong PATH is
// diagnosable from the error alone.
func resolveGH(configured string) (string, error) {
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("gh path %q from config is invalid: %w", configured, err)
		}
		return path, nil
	}
	var failures []string
	for _, candidate := range ghProbeCandidates {
		path, err := exec.LookPath(candidate)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: not found", candidate))
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("no gh binary found (tried: %s)", strings.Join(failures, "; "))
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
