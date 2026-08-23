package xdg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	t.Setenv("XDG_DATA_HOME", "/tmp/data")
	t.Setenv("XDG_STATE_HOME", "/tmp/state")

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ConfigPath", ConfigPath(), "/tmp/cfg/muninn/config.json"},
		{"DataDir", DataDir(), "/tmp/data/muninn"},
		{"StateDir", StateDir(), "/tmp/state/muninn"},
		{"MirrorsDir", MirrorsDir(), "/tmp/data/muninn/mirrors"},
		{"IndexDir", IndexDir(), "/tmp/data/muninn/index"},
		{"StatusPath", StatusPath(), "/tmp/state/muninn/status.json"},
		{"LogPath", LogPath(), "/tmp/state/muninn/sync.log"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestDefaultsExpandHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ConfigPath", ConfigPath(), filepath.Join(home, ".config", "muninn", "config.json")},
		{"DataDir", DataDir(), filepath.Join(home, ".local", "share", "muninn")},
		{"StateDir", StateDir(), filepath.Join(home, ".local", "state", "muninn")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestEnsureDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	if err := EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	for _, dir := range []string{
		filepath.Dir(ConfigPath()),
		MirrorsDir(),
		IndexDir(),
		StateDir(),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s perm = %o, want 0700", dir, perm)
		}
	}
}
