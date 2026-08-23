// Package status reads and writes the sync status file with atomic
// rename-based writes, safe against concurrent readers.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNotExist indicates the status file does not exist (never synced).
var ErrNotExist = fmt.Errorf("status file does not exist: %w", os.ErrNotExist)

type SyncStatus struct {
	StartedAt  time.Time             `json:"startedAt"`
	FinishedAt time.Time             `json:"finishedAt"`
	Success    bool                  `json:"success"`
	Repos      map[string]RepoStatus `json:"repos"` // key: "owner/name"
}

type RepoStatus struct {
	Fetched       bool   `json:"fetched"`
	Indexed       bool   `json:"indexed"`
	IndexedCommit string `json:"indexedCommit,omitempty"` // full SHA
	Error         string `json:"error,omitempty"`
}

// Write atomically writes the status to path via a temp file and rename.
func Write(path string, s *SyncStatus) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling status: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating status directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing status: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming status into place: %w", err)
	}
	return nil
}

// Read loads the status from path. It returns an error wrapping ErrNotExist
// if the file does not exist, distinguishing "never synced" from corruption.
func Read(path string) (*SyncStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, path)
		}
		return nil, fmt.Errorf("reading status: %w", err)
	}
	var s SyncStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("corrupt status file %s: %w", path, err)
	}
	return &s, nil
}

// Age returns how long ago the sync finished.
func Age(s *SyncStatus) time.Duration {
	return time.Since(s.FinishedAt)
}
