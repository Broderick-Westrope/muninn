// Package xdg resolves muninn's config, data, and state paths following
// the XDG Base Directory specification, with home-relative fallbacks.
package xdg

import (
	"fmt"
	"os"
	"path/filepath"
)

const appName = "muninn"

// ConfigPath returns the path to the config file.
func ConfigPath() string {
	return filepath.Join(configDir(), "config.json")
}

// DataDir returns the data directory, honoring XDG_DATA_HOME.
func DataDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, appName)
	}
	return filepath.Join(homeDir(), ".local", "share", appName)
}

// StateDir returns the state directory, honoring XDG_STATE_HOME.
func StateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, appName)
	}
	return filepath.Join(homeDir(), ".local", "state", appName)
}

// MirrorsDir returns the directory holding bare git mirrors.
func MirrorsDir() string {
	return filepath.Join(DataDir(), "mirrors")
}

// IndexDir returns the directory holding the Zoekt index shards.
func IndexDir() string {
	return filepath.Join(DataDir(), "index")
}

// StatusPath returns the path to the sync status file.
func StatusPath() string {
	return filepath.Join(StateDir(), "status.json")
}

// LogPath returns the path to the sync log file.
func LogPath() string {
	return filepath.Join(StateDir(), "sync.log")
}

// EnsureDirs creates the config, mirrors, index, and state directories
// with 0700 permissions if they do not already exist.
func EnsureDirs() error {
	dirs := []string{configDir(), MirrorsDir(), IndexDir(), StateDir()}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}
	return nil
}

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, appName)
	}
	return filepath.Join(homeDir(), ".config", appName)
}

// homeDir returns the user's home directory. It is only called when the
// relevant XDG override is unset, so if the home directory cannot be
// determined (e.g. launchd strips HOME from the environment) there is no
// safe fallback and homeDir panics with an actionable message rather than
// silently using a wrong path.
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Sprintf("muninn: cannot determine home directory (HOME unset?): %v; set XDG_CONFIG_HOME/XDG_DATA_HOME/XDG_STATE_HOME explicitly", err))
	}
	return home
}
