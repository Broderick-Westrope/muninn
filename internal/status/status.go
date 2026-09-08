// Package status reads and writes the sync status file with atomic
// rename-based writes, safe against concurrent readers.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// StaleAfter is how long a repo may go without a successful sync before
// readers warn that its data is old. It lives here so the MCP server and
// the web API cannot drift to two definitions of a repo-level fact.
const StaleAfter = 24 * time.Hour

// ErrNotExist indicates the status file does not exist (never synced).
var ErrNotExist = errors.New("status file does not exist")

// schemaVersion is written into every status file. It exists so migration
// heuristics can be applied to old files without also firing on new ones:
// a zero per-repo clock in a v1 file is a fact (that repo has never
// succeeded), whereas in a versionless file it is merely absent.
const schemaVersion = 1

type SyncStatus struct {
	// Version is the schema version of this file; absent (0) means it was
	// written before versioning, so migrations apply.
	Version   int       `json:"version,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt is when the most recent run ended, successful or not. It
	// is an attempt clock, never a freshness clock: measuring staleness
	// with it makes a repeatedly failing sync look perpetually fresh.
	// Zero means the run recorded here is still in flight.
	FinishedAt time.Time `json:"finishedAt"`
	Success    bool      `json:"success"`
	// LastSuccessAt is when the whole fleet was last simultaneously clean.
	// It is a health signal for `muninn status`, not a freshness measure:
	// one permanently failing repo freezes it while every other repo keeps
	// updating, so per-repo staleness reads RepoStatus.LastSuccessAt.
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	// Error summarizes why the most recent run did not fully succeed
	// (either the total-failure cause or a per-repo failure count). It is
	// cleared on success.
	Error string `json:"error,omitempty"`
	// ConsecutiveFailures counts runs since the last full success, so the
	// difference between one flaky run and a week of silent breakage is
	// visible without reading logs.
	ConsecutiveFailures int `json:"consecutiveFailures,omitempty"`
	// TotalFailure records that the run failed as a whole rather than in
	// individual repos (bad credentials, missing ctags, a discovery that
	// returned nothing). It is what distinguishes a broken sync mechanism
	// from a sync that worked and found one bad repo.
	TotalFailure bool                  `json:"totalFailure,omitempty"`
	Repos        map[string]RepoStatus `json:"repos"` // key: "owner/name"
}

type RepoStatus struct {
	Fetched       bool   `json:"fetched"`
	Indexed       bool   `json:"indexed"`
	IndexedCommit string `json:"indexedCommit,omitempty"` // full SHA
	Error         string `json:"error,omitempty"`
	// GCError records a failure of post-index housekeeping (git gc). It is
	// kept apart from Error because the repo is fully fetched, indexed, and
	// pinned either way: counting it as a run failure would freeze the
	// freshness clock and flip the whole index to "stale" over a bloated
	// pack file.
	GCError string `json:"gcError,omitempty"`
	// LastSuccessAt is when this repo was last fetched and indexed. It is
	// carried forward across failures alongside IndexedCommit, and is the
	// basis for staleness: freshness is a property of a repo's data, not of
	// the fleet, so one broken repo must not age the other forty-nine.
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	// FetchRetries counts retries spent fetching this repo in the most
	// recent run. A retried repo that then succeeded is a full success and
	// deliberately does not fail the run — otherwise an hourly sync would
	// sit permanently degraded over a transient network flake — so this
	// counter is the only thing keeping that flake visible. A value that
	// used to be 0 or 1 and is now routinely nonzero means the network or
	// the fetch concurrency needs attention, not a bigger retry budget.
	FetchRetries int `json:"fetchRetries,omitempty"`
}

// Write atomically writes the status to path via a temp file and rename.
func Write(path string, s *SyncStatus) error {
	s.Version = schemaVersion
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
	if s.Version < schemaVersion {
		migrate(&s)
	}
	return &s, nil
}

// migrate fills in fields added after a status file was written. It runs
// only for files below the current schema version: the same absent value
// means "not recorded yet" in an old file and "never happened" in a new
// one, so guessing unconditionally would overwrite facts with defaults.
func migrate(s *SyncStatus) {
	// A successful run recorded its time only in FinishedAt, so without
	// this an upgrade reports "never succeeded" for a fresh index.
	if s.LastSuccessAt.IsZero() && s.Success && !s.FinishedAt.IsZero() {
		s.LastSuccessAt = s.FinishedAt
	}
	// Per-repo clocks are backfilled from each repo's own recorded
	// outcome, never from the fleet-wide Success flag. A repo that is
	// indexed with no error did succeed in the recorded run even when some
	// *other* repo failed the run overall — and that partial-failure state
	// is exactly what these clocks exist to describe, so keying off
	// Success would zero every clock in the case that matters most.
	if s.FinishedAt.IsZero() {
		return
	}
	for name, rs := range s.Repos {
		if rs.LastSuccessAt.IsZero() && rs.Indexed && rs.Error == "" {
			rs.LastSuccessAt = s.FinishedAt
			s.Repos[name] = rs
		}
	}
}

// maxErrorLen caps the recorded error text.
//
// SyncStatus.Error is the string that needs the cap: on the TotalFailure
// path it carries remote-derived text (a GitHub API response body) and it
// is the one error interpolated into MCP tool output, via the degraded
// warning, where an unbounded value could flood or steer an agent's
// context. Truncation keeps the head because that is where muninn's own
// framing of the failure sits; the remote payload trails it and is the
// part that should be cut.
//
// RepoStatus.Error only reaches the `muninn status` table, so the cap is
// a length backstop rather than a safety boundary there. It is not a
// licence to record sprawling errors: Write marshals the whole file after
// every repo completes, so per-entry size is multiplied by the repo count
// on every run and again on every MCP tool call that reads the file.
// Callers should record a concise cause (see mirror's netOpError) instead
// of relying on this to trim a verbose one.
const maxErrorLen = 200

// TruncateError shortens a recorded error to maxErrorLen runes, marking
// that it was cut so a truncated cause is never mistaken for the whole one.
func TruncateError(msg string) string {
	runes := []rune(msg)
	if len(runes) <= maxErrorLen {
		return msg
	}
	return string(runes[:maxErrorLen]) + "… (truncated)"
}

// Age returns how long ago the last run finished, successful or not. Use
// SuccessAge or Stale to judge whether the index is usable; a failed run
// also sets FinishedAt.
func Age(s *SyncStatus) time.Duration {
	return time.Since(s.FinishedAt)
}

// SuccessAge returns how long ago the last fully successful sync finished
// and whether there has ever been one.
//
// A LastSuccessAt in the future yields a negative age rather than being
// clamped, so callers can distinguish "impossibly fresh" from genuinely
// fresh; Stale and Due both treat it as unusable. Timestamps ahead of now
// mean the clock moved backwards (a VM snapshot restore, a dead RTC, an
// NTP correction after a bad boot clock), and trusting them would suppress
// both syncing and every staleness warning until real time caught up.
func SuccessAge(s *SyncStatus) (age time.Duration, ok bool) {
	if s == nil || s.LastSuccessAt.IsZero() {
		return 0, false
	}
	return time.Since(s.LastSuccessAt), true
}

// Stale reports whether the index has gone without a successful sync for
// longer than after. A status with no successful run, or one whose success
// is dated in the future, is always stale.
//
// This is the fleet-wide question and is only meaningful for overall
// health; use RepoStale for whether a given repo's data is usable.
func Stale(s *SyncStatus, after time.Duration) bool {
	age, ok := SuccessAge(s)
	return !ok || age < 0 || age > after
}

// RepoSuccessAge returns how long ago the named repo was last successfully
// synced, and whether it ever was. A repo absent from the status has never
// been synced.
func RepoSuccessAge(s *SyncStatus, repo string) (age time.Duration, ok bool) {
	if s == nil {
		return 0, false
	}
	rs, found := s.Repos[repo]
	if !found || rs.LastSuccessAt.IsZero() {
		return 0, false
	}
	return time.Since(rs.LastSuccessAt), true
}

// RepoStale reports whether the named repo's data is older than after.
// A repo that has never synced successfully is stale, as is one whose
// success is dated in the future (see SuccessAge on backwards clocks).
func RepoStale(s *SyncStatus, repo string, after time.Duration) bool {
	age, ok := RepoSuccessAge(s, repo)
	return !ok || age < 0 || age > after
}

// StaleRepos returns the names of indexed repos whose data is older than
// after, sorted. Repos that have never been indexed are excluded: they hold
// no searchable data, so calling them "stale" would describe results that
// cannot be returned in the first place.
func StaleRepos(s *SyncStatus, after time.Duration) []string {
	if s == nil {
		return nil
	}
	var stale []string
	for name, rs := range s.Repos {
		if !rs.Indexed && rs.IndexedCommit == "" {
			continue
		}
		if RepoStale(s, name, after) {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	return stale
}

// Interrupted reports whether the recorded run started but never finished.
// Under the sync lock no live run can be observed this way from another
// process, so outside a run in progress it means the previous one died
// before writing its outcome (killed, OOM, or the machine slept).
func Interrupted(s *SyncStatus) bool {
	return s != nil && !s.StartedAt.IsZero() && s.FinishedAt.IsZero()
}

// Degraded reports whether the sync *mechanism* is broken: the last run
// failed as a whole, or never completed at all. It is deliberately not
// "some repo is behind".
//
// A run that fetched and indexed forty-nine repos and hit one bad one did
// its job; the machinery works and one repo is unreachable. Treating that
// as degraded would put the scheduler on the retry cadence forever,
// re-fetching the whole fleet every few minutes over a repo that will
// never succeed. Per-repo staleness is reported through RepoStale instead,
// which is where that failure actually shows up.
func Degraded(s *SyncStatus) bool {
	if s == nil {
		return false
	}
	if Interrupted(s) {
		return true
	}
	if s.FinishedAt.IsZero() {
		return false
	}
	return s.TotalFailure
}

// Due reports whether a scheduled sync should start at now.
//
// The freshness gate measures from the end of the last run that actually
// did its work, not from the last run in which *every* repo succeeded.
// Those differ whenever one repo is permanently unreachable, and keying off
// the latter would leave the scheduler convinced it was never fresh: it
// would refetch and reindex the entire fleet at the retry cadence forever
// over a single bad repo.
//
// When the mechanism itself is broken — a total failure, or a run that died
// mid-flight — the cadence drops to retry so a transient cause (expired
// token, no network on wake) costs minutes of staleness instead of a full
// interval. The cooldown applies to interrupted runs too, measured from
// when they started, since exempting them would retry at the scheduler's
// tick rate for as long as the crash persists.
//
// Overlap is prevented by the sync lock, not by this check.
func Due(s *SyncStatus, interval, retry time.Duration, now time.Time) bool {
	if s == nil {
		return true // never synced
	}
	cooldown := interval
	if Degraded(s) {
		cooldown = retry
	}
	// An interrupted run has no FinishedAt, so its cooldown runs from
	// StartedAt; a status with neither is a fresh install and always due.
	last := s.FinishedAt
	if last.IsZero() {
		last = s.StartedAt
	}
	if last.IsZero() {
		return true
	}
	// A last-run timestamp in the future is treated as elapsed rather than
	// blocking the sync until the clock catches up.
	return now.Before(last) || now.Sub(last) >= cooldown
}
