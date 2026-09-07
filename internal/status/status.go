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
var ErrNotExist = errors.New("status file does not exist")

type SyncStatus struct {
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt is when the most recent run ended, successful or not. It
	// is an attempt clock, never a freshness clock: measuring staleness
	// with it makes a repeatedly failing sync look perpetually fresh.
	// Zero means the run recorded here is still in flight.
	FinishedAt time.Time `json:"finishedAt"`
	Success    bool      `json:"success"`
	// LastSuccessAt is when the most recent fully successful run finished.
	// It is carried forward across failed runs and is the only correct
	// basis for staleness. Zero means no run has ever fully succeeded.
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	// Error summarizes why the most recent run did not fully succeed
	// (either the total-failure cause or a per-repo failure count). It is
	// cleared on success.
	Error string `json:"error,omitempty"`
	// ConsecutiveFailures counts runs since the last full success, so the
	// difference between one flaky run and a week of silent breakage is
	// visible without reading logs.
	ConsecutiveFailures int                   `json:"consecutiveFailures,omitempty"`
	Repos               map[string]RepoStatus `json:"repos"` // key: "owner/name"
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
	f, err := os.CreateTemp(filepath.Dir(path), ".status-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp status file: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("setting status permissions: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing status: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("syncing status: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing status: %w", err)
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
			return nil, fmt.Errorf("%w: %w", ErrNotExist, err)
		}
		return nil, fmt.Errorf("reading status: %w", err)
	}
	var s SyncStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("corrupt status file %s: %w", path, err)
	}
	// Migration: status files written before LastSuccessAt existed record a
	// successful run only in FinishedAt. Backfilling here means an upgrade
	// does not report "never succeeded" for an index that is in fact fresh.
	if s.LastSuccessAt.IsZero() && s.Success && !s.FinishedAt.IsZero() {
		s.LastSuccessAt = s.FinishedAt
	}
	return &s, nil
}

// Age returns how long ago the last run finished, successful or not. Use
// SuccessAge or Stale to judge whether the index is usable; a failed run
// also sets FinishedAt.
func Age(s *SyncStatus) time.Duration {
	return time.Since(s.FinishedAt)
}

// SuccessAge returns how long ago the last fully successful sync finished
// and whether there has ever been one.
func SuccessAge(s *SyncStatus) (age time.Duration, ok bool) {
	if s == nil || s.LastSuccessAt.IsZero() {
		return 0, false
	}
	return time.Since(s.LastSuccessAt), true
}

// Stale reports whether the index has gone without a successful sync for
// longer than after. A status with no successful run is always stale.
func Stale(s *SyncStatus, after time.Duration) bool {
	age, ok := SuccessAge(s)
	return !ok || age > after
}

// Degraded reports whether the last completed run failed to fully succeed,
// meaning the index is no longer being updated even though runs are still
// firing. A run in flight (FinishedAt zero) is not degraded.
func Degraded(s *SyncStatus) bool {
	if s == nil || s.FinishedAt.IsZero() {
		return false
	}
	return s.FinishedAt.After(s.LastSuccessAt)
}

// Due reports whether a scheduled sync should start at now.
//
// A success within interval means the index is fresh and nothing runs. Once
// it is older than that an attempt is warranted, and how soon it may be
// reattempted depends on Degraded: after a failed run the cadence drops to
// retry so a transient cause (expired token, no network on wake) costs
// minutes of staleness instead of a full interval. Runs still in flight,
// and statuses with no run at all, are always due — overlap is prevented by
// the sync lock, not by this check.
func Due(s *SyncStatus, interval, retry time.Duration, now time.Time) bool {
	if s == nil {
		return true // never synced
	}
	if !s.LastSuccessAt.IsZero() && now.Sub(s.LastSuccessAt) < interval {
		return false
	}
	cooldown := interval
	if Degraded(s) {
		cooldown = retry
	}
	return s.FinishedAt.IsZero() || now.Sub(s.FinishedAt) >= cooldown
}
