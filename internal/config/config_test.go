package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSourcebotConfig(t *testing.T) {
	c, err := Load(filepath.Join("testdata", "sourcebot.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	conn, ok := c.Connections["github-connection"]
	if !ok {
		t.Fatal("missing github-connection")
	}
	if conn.Type != "github" {
		t.Errorf("Type = %q, want github", conn.Type)
	}
	if len(conn.Orgs) != 1 || conn.Orgs[0] != "eucalyptusvc" {
		t.Errorf("Orgs = %v, want [eucalyptusvc]", conn.Orgs)
	}
	if conn.Token == nil || conn.Token.Env != "MUNINN_TEST_GITHUB_TOKEN" {
		t.Errorf("Token = %+v, want env MUNINN_TEST_GITHUB_TOKEN", conn.Token)
	}
	if conn.Exclude == nil {
		t.Fatal("Exclude is nil")
	}
	if !conn.Exclude.Archived {
		t.Error("Exclude.Archived = false, want true")
	}
	if len(conn.Exclude.Repos) != 3 {
		t.Errorf("len(Exclude.Repos) = %d, want 3", len(conn.Exclude.Repos))
	}
	if c.Sync.IntervalMinutes != 60 {
		t.Errorf("Sync.IntervalMinutes = %d, want default 60", c.Sync.IntervalMinutes)
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"no connections", `{"connections": {}}`},
		{"wrong type", `{"connections": {"c": {"type": "gitlab", "orgs": ["x"]}}}`},
		{"no orgs or repos", `{"connections": {"c": {"type": "github"}}}`},
		{"corrupt json", `{not json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeConfigFile(t, tt.content)); err == nil {
				t.Error("Load succeeded, want error")
			}
		})
	}
}

func TestLoadReposOnlyIsValid(t *testing.T) {
	c, err := Load(writeConfigFile(t, `{"connections": {"c": {"type": "github", "repos": ["owner/name"]}}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Connections["c"].Repos; len(got) != 1 || got[0] != "owner/name" {
		t.Errorf("Repos = %v, want [owner/name]", got)
	}
}

func TestLoadKeepsExplicitInterval(t *testing.T) {
	c, err := Load(writeConfigFile(t, `{"connections": {"c": {"type": "github", "orgs": ["x"]}}, "sync": {"intervalMinutes": 15}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Sync.IntervalMinutes != 15 {
		t.Errorf("Sync.IntervalMinutes = %d, want 15", c.Sync.IntervalMinutes)
	}
}

func TestResolveTokenPrecedence(t *testing.T) {
	base := func() *Config {
		return &Config{
			Connections: map[string]Connection{
				"c": {Type: "github", Orgs: []string{"x"}, Token: &TokenRef{Env: "MUNINN_TEST_GITHUB_TOKEN"}},
			},
		}
	}

	t.Run("auth wins over env", func(t *testing.T) {
		t.Setenv("MUNINN_TEST_GITHUB_TOKEN", "from-env")
		c := base()
		c.Auth.GitHubToken = "from-auth"
		got, err := ResolveToken(c)
		if err != nil {
			t.Fatalf("ResolveToken: %v", err)
		}
		if got != "from-auth" {
			t.Errorf("token = %q, want from-auth", got)
		}
	})

	t.Run("env wins over gh", func(t *testing.T) {
		t.Setenv("MUNINN_TEST_GITHUB_TOKEN", "from-env")
		got, err := ResolveToken(base())
		if err != nil {
			t.Fatalf("ResolveToken: %v", err)
		}
		if got != "from-env" {
			t.Errorf("token = %q, want from-env", got)
		}
	})

	t.Run("gh fallback", func(t *testing.T) {
		t.Setenv("MUNINN_TEST_GITHUB_TOKEN", "")
		t.Setenv("PATH", fakeGH(t, "gh-token", 0))
		got, err := ResolveToken(base())
		if err != nil {
			t.Fatalf("ResolveToken: %v", err)
		}
		if got != "gh-token" {
			t.Errorf("token = %q, want gh-token", got)
		}
	})

	t.Run("all fail", func(t *testing.T) {
		t.Setenv("MUNINN_TEST_GITHUB_TOKEN", "")
		t.Setenv("PATH", fakeGH(t, "", 1))
		_, err := ResolveToken(base())
		if err == nil {
			t.Fatal("ResolveToken succeeded, want error")
		}
		for _, want := range []string{"MUNINN_TEST_GITHUB_TOKEN", "gh"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// fakeGH writes a fake gh executable that prints output and exits with code,
// returning a PATH containing only its directory.
func fakeGH(t *testing.T, output string, code int) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho %s\nexit %d\n", output, code)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSaveRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	c := &Config{
		Connections: map[string]Connection{
			"c": {
				Type:    "github",
				Orgs:    []string{"eucalyptusvc"},
				Token:   &TokenRef{Env: "GH_TOKEN"},
				Exclude: &Exclude{Archived: true, Repos: []string{"owner/name"}},
			},
		},
		Sync: SyncConfig{IntervalMinutes: 30},
	}
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	conn := got.Connections["c"]
	if conn.Exclude == nil || !conn.Exclude.Archived || len(conn.Exclude.Repos) != 1 {
		t.Errorf("roundtrip Exclude = %+v", conn.Exclude)
	}
	if got.Sync.IntervalMinutes != 30 {
		t.Errorf("roundtrip IntervalMinutes = %d, want 30", got.Sync.IntervalMinutes)
	}
}
