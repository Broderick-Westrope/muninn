package xdg

import (
	"fmt"
	"os"
	"path/filepath"
)

const appName = "muninn"

func ConfigPath() string {
	return filepath.Join(configDir(), "config.json")
}

func DataDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, appName)
	}
	return filepath.Join(homeDir(), ".local", "share", appName)
}

func StateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, appName)
	}
	return filepath.Join(homeDir(), ".local", "state", appName)
}

func MirrorsDir() string {
	return filepath.Join(DataDir(), "mirrors")
}

func IndexDir() string {
	return filepath.Join(DataDir(), "index")
}

func StatusPath() string {
	return filepath.Join(StateDir(), "status.json")
}

func LogPath() string {
	return filepath.Join(StateDir(), "sync.log")
}

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

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
