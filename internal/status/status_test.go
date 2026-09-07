package status

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestWriteReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "status.json")
	want := &SyncStatus{
		StartedAt:  time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 24, 10, 5, 0, 0, time.UTC),
		Success:    true,
		Repos: map[string]RepoStatus{
			"owner/ok":     {Fetched: true, Indexed: true, IndexedCommit: "abc123"},
			"owner/broken": {Fetched: true, Error: "index failed"},
		},
	}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("temp file left behind")
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.StartedAt.Equal(want.StartedAt) || !got.FinishedAt.Equal(want.FinishedAt) || got.Success != want.Success {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(got.Repos) != 2 {
		t.Fatalf("len(Repos) = %d, want 2", len(got.Repos))
	}
	if r := got.Repos["owner/ok"]; !r.Indexed || r.IndexedCommit != "abc123" {
		t.Errorf("owner/ok = %+v", r)
	}
	if r := got.Repos["owner/broken"]; r.Error != "index failed" {
		t.Errorf("owner/broken = %+v", r)
	}
}

func TestReadNotExist(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want to wrap os.ErrNotExist", err)
	}
}

func TestReadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path)
	if err == nil {
		t.Fatal("Read succeeded, want error")
	}
	if errors.Is(err, ErrNotExist) {
		t.Errorf("corrupt file reported as ErrNotExist: %v", err)
	}
}

func TestAge(t *testing.T) {
	s := &SyncStatus{FinishedAt: time.Now().Add(-time.Hour)}
	if age := Age(s); age < 59*time.Minute || age > 61*time.Minute {
		t.Errorf("Age = %v, want ~1h", age)
	}
}

func TestSuccessAge(t *testing.T) {
	// The regression this guards: a run that keeps failing refreshes
	// FinishedAt every hour, so measuring freshness with Age reports a
	// week-old index as minutes old.
	s := &SyncStatus{
		FinishedAt:    time.Now().Add(-time.Minute),
		LastSuccessAt: time.Now().Add(-7 * 24 * time.Hour),
	}
	age, ok := SuccessAge(s)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if age < 6*24*time.Hour {
		t.Errorf("SuccessAge = %v, want ~7 days (not the attempt age)", age)
	}
	if !Stale(s, 24*time.Hour) {
		t.Error("Stale = false, want true for a week without a success")
	}

	if _, ok := SuccessAge(&SyncStatus{FinishedAt: time.Now()}); ok {
		t.Error("ok = true, want false when no run has ever succeeded")
	}
	if !Stale(&SyncStatus{FinishedAt: time.Now()}, time.Hour) {
		t.Error("Stale = false, want true when no run has ever succeeded")
	}
	if _, ok := SuccessAge(nil); ok {
		t.Error("ok = true, want false for a nil status")
	}
}

func TestReadBackfillsLastSuccessAt(t *testing.T) {
	// Status files written before LastSuccessAt existed must not be read
	// as "never succeeded" after an upgrade.
	path := filepath.Join(t.TempDir(), "status.json")
	finished := time.Date(2026, 8, 24, 10, 5, 0, 0, time.UTC)
	legacy := fmt.Sprintf(`{"startedAt":%q,"finishedAt":%q,"success":true,"repos":{}}`,
		finished.Add(-time.Minute).Format(time.RFC3339), finished.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.LastSuccessAt.Equal(finished) {
		t.Errorf("LastSuccessAt = %v, want %v backfilled from FinishedAt", got.LastSuccessAt, finished)
	}

	// A legacy *failed* run must not be backfilled: it never succeeded.
	failed := fmt.Sprintf(`{"finishedAt":%q,"success":false,"repos":{}}`, finished.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(failed), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.LastSuccessAt.IsZero() {
		t.Errorf("LastSuccessAt = %v, want zero for a legacy failed run", got.LastSuccessAt)
	}
}

func TestDegraded(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		st   *SyncStatus
		want bool
	}{
		{"nil", nil, false},
		{"healthy", &SyncStatus{FinishedAt: now, LastSuccessAt: now}, false},
		// A run that completed with some repos failing is NOT degraded: the
		// machinery worked. Treating it as degraded is what pinned the
		// scheduler to the retry cadence over one unreachable repo.
		{"partial repo failure", &SyncStatus{FinishedAt: now, LastSuccessAt: now.Add(-time.Hour)}, false},
		{"total failure", &SyncStatus{FinishedAt: now, TotalFailure: true, LastSuccessAt: now.Add(-time.Hour)}, true},
		// An interrupted run is degraded: it records no error and no
		// failure count, so treating it as healthy is what let a
		// crash-looping sync stay invisible until the index went stale.
		{"interrupted", &SyncStatus{StartedAt: now, LastSuccessAt: now.Add(-time.Hour)}, true},
		{"empty", &SyncStatus{}, false},
	}
	for _, tt := range tests {
		if got := Degraded(tt.st); got != tt.want {
			t.Errorf("%s: Degraded = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestInterrupted(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		st   *SyncStatus
		want bool
	}{
		{"nil", nil, false},
		{"started, not finished", &SyncStatus{StartedAt: now}, true},
		{"finished", &SyncStatus{StartedAt: now.Add(-time.Minute), FinishedAt: now}, false},
		{"never ran", &SyncStatus{}, false},
	}
	for _, tt := range tests {
		if got := Interrupted(tt.st); got != tt.want {
			t.Errorf("%s: Interrupted = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTruncateError(t *testing.T) {
	if got := TruncateError("short"); got != "short" {
		t.Errorf("TruncateError(short) = %q, want it unchanged", got)
	}
	long := strings.Repeat("x", 500)
	got := TruncateError(long)
	if len([]rune(got)) >= len([]rune(long)) {
		t.Errorf("len(TruncateError) = %d, want it shortened", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("TruncateError = %q, want a truncation marker", got)
	}
	// Multi-byte input must not be cut mid-rune.
	if got := TruncateError(strings.Repeat("é", 500)); !utf8.ValidString(got) {
		t.Error("TruncateError produced invalid UTF-8")
	}
}

func TestDue(t *testing.T) {
	const (
		interval = time.Hour
		retry    = 10 * time.Minute
	)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name string
		st   *SyncStatus
		want bool
	}{
		{"never synced", nil, true},
		{
			"fresh success",
			&SyncStatus{FinishedAt: ago(5 * time.Minute), LastSuccessAt: ago(5 * time.Minute)},
			false,
		},
		{
			"success older than interval",
			&SyncStatus{FinishedAt: ago(90 * time.Minute), LastSuccessAt: ago(90 * time.Minute)},
			true,
		},
		{
			// A total failure gets the retry cadence: the mechanism is
			// broken and the cause is plausibly transient.
			"total failure, retry elapsed",
			&SyncStatus{FinishedAt: ago(11 * time.Minute), TotalFailure: true, LastSuccessAt: ago(3 * time.Hour)},
			true,
		},
		{
			// Same state, but too soon — reattempts are faster, not
			// unbounded.
			"total failure, retry not elapsed",
			&SyncStatus{FinishedAt: ago(2 * time.Minute), TotalFailure: true, LastSuccessAt: ago(3 * time.Hour)},
			false,
		},
		{
			// The case this model exists for: one repo has been failing for
			// hours, so the fleet clock is frozen, but the runs themselves
			// are working. The normal interval applies, not the retry
			// cadence — otherwise the whole fleet is refetched every few
			// minutes forever over a single bad repo.
			"partial failure uses the normal interval",
			&SyncStatus{FinishedAt: ago(11 * time.Minute), LastSuccessAt: ago(3 * time.Hour)},
			false,
		},
		{
			"partial failure, interval elapsed",
			&SyncStatus{FinishedAt: ago(61 * time.Minute), LastSuccessAt: ago(9 * time.Hour)},
			true,
		},
		{
			"recent successful run is not due",
			&SyncStatus{FinishedAt: ago(time.Minute), LastSuccessAt: ago(time.Minute), Success: true},
			false,
		},
		{
			"never succeeded, retry elapsed",
			&SyncStatus{FinishedAt: ago(30 * time.Minute), TotalFailure: true},
			true,
		},
		{
			"never succeeded, retry not elapsed",
			&SyncStatus{FinishedAt: ago(time.Minute), TotalFailure: true},
			false,
		},
		{
			// An interrupted run gets the retry cooldown measured from
			// StartedAt. Exempting it would make a sync that crashes every
			// time retry at the scheduler's tick rate instead of at retry.
			"interrupted, cooldown not elapsed",
			&SyncStatus{StartedAt: ago(2 * time.Minute), LastSuccessAt: ago(3 * time.Hour)},
			false,
		},
		{
			"interrupted, cooldown elapsed",
			&SyncStatus{StartedAt: ago(11 * time.Minute), LastSuccessAt: ago(3 * time.Hour)},
			true,
		},
		{
			// A clock that moved backwards must not make the index look
			// permanently fresh; that silence is the bug this all fixes.
			"success dated in the future",
			&SyncStatus{FinishedAt: now.Add(24 * time.Hour), LastSuccessAt: now.Add(24 * time.Hour)},
			true,
		},
		{
			"no run recorded at all",
			&SyncStatus{},
			true,
		},
	}
	for _, tt := range tests {
		if got := Due(tt.st, interval, retry, now); got != tt.want {
			t.Errorf("%s: Due = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestConcurrentReadDuringWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")

	const iterations = 100
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			s, err := Read(path)
			if err != nil {
				if errors.Is(err, ErrNotExist) {
					continue
				}
				t.Errorf("reader saw partial/corrupt status: %v", err)
				return
			}
			if len(s.Repos) != 1 {
				t.Errorf("reader saw partial status: %+v", s)
				return
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		s := &SyncStatus{
			StartedAt:  time.Now(),
			FinishedAt: time.Now(),
			Success:    true,
			Repos: map[string]RepoStatus{
				"owner/name": {Fetched: true, Indexed: true, IndexedCommit: fmt.Sprintf("%040d", i)},
			},
		}
		if err := Write(path, s); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
}

// TestReadBackfillsRepoClocksFromRepoOutcome is the regression test for a
// migration that would have failed in exactly the state it was written
// for. Keying the per-repo backfill off the fleet-wide Success flag zeroes
// every clock whenever one repo is failing — and a permanently failing repo
// is precisely why per-repo clocks exist, so the fix would have reported
// "every repo never synced" on upgrade.
func TestReadBackfillsRepoClocksFromRepoOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	finished := time.Date(2026, 8, 24, 10, 5, 0, 0, time.UTC)
	// A pre-versioning file: the run failed overall because one repo
	// failed, but the other two are indexed and error-free.
	legacy := fmt.Sprintf(`{
	  "startedAt": %q,
	  "finishedAt": %q,
	  "success": false,
	  "error": "1 of 3 repos failed",
	  "repos": {
	    "acme/ok":     {"fetched": true, "indexed": true, "indexedCommit": "aaa"},
	    "acme/also":   {"fetched": true, "indexed": true, "indexedCommit": "bbb"},
	    "acme/broken": {"fetched": true, "error": "auth failed"}
	  }
	}`, finished.Add(-time.Minute).Format(time.RFC3339), finished.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, name := range []string{"acme/ok", "acme/also"} {
		if !got.Repos[name].LastSuccessAt.Equal(finished) {
			t.Errorf("%s LastSuccessAt = %v, want %v backfilled from its own outcome",
				name, got.Repos[name].LastSuccessAt, finished)
		}
	}
	if !got.Repos["acme/broken"].LastSuccessAt.IsZero() {
		t.Errorf("acme/broken LastSuccessAt = %v, want zero: it failed in the recorded run",
			got.Repos["acme/broken"].LastSuccessAt)
	}
	// The fixture's timestamps are historical, so the two indexed repos
	// are legitimately stale now. What matters is that acme/broken is
	// excluded entirely: it holds no shards, so naming it as stale data
	// would describe results that cannot be returned.
	for _, name := range StaleRepos(got, time.Minute) {
		if name == "acme/broken" {
			t.Error("StaleRepos included a repo that was never indexed")
		}
	}
	if len(StaleRepos(got, time.Minute)) != 2 {
		t.Errorf("StaleRepos = %v, want the two indexed repos", StaleRepos(got, time.Minute))
	}
}

// TestReadDoesNotMigrateCurrentFiles guards the ambiguity that makes the
// backfill safe: in a versioned file a zero clock is a fact ("never
// succeeded"), so guessing a value would overwrite it.
func TestReadDoesNotMigrateCurrentFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	now := time.Now().Truncate(time.Second)
	// Written by the current code, so Version is stamped in.
	if err := Write(path, &SyncStatus{
		StartedAt:  now.Add(-time.Minute),
		FinishedAt: now,
		Success:    false,
		Repos: map[string]RepoStatus{
			// Indexed and error-free, but genuinely never succeeded: only
			// reachable in a hand-built status, which is the point — the
			// migration must not invent a clock for it.
			"acme/odd": {Fetched: true, Indexed: true, IndexedCommit: "aaa"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Version != schemaVersion {
		t.Errorf("Version = %d, want %d written by Write", got.Version, schemaVersion)
	}
	if !got.Repos["acme/odd"].LastSuccessAt.IsZero() {
		t.Errorf("LastSuccessAt = %v, want it left zero in a current-version file",
			got.Repos["acme/odd"].LastSuccessAt)
	}
}

func TestRepoStaleAndStaleRepos(t *testing.T) {
	now := time.Now()
	st := &SyncStatus{
		Version:    schemaVersion,
		FinishedAt: now,
		Repos: map[string]RepoStatus{
			"acme/fresh":  {Indexed: true, IndexedCommit: "a", LastSuccessAt: now.Add(-time.Minute)},
			"acme/old":    {Indexed: true, IndexedCommit: "b", LastSuccessAt: now.Add(-72 * time.Hour)},
			"acme/never":  {Indexed: true, IndexedCommit: "c"},
			"acme/absent": {Error: "auth failed"},
			"acme/future": {Indexed: true, IndexedCommit: "d", LastSuccessAt: now.Add(48 * time.Hour)},
		},
	}

	if RepoStale(st, "acme/fresh", StaleAfter) {
		t.Error("acme/fresh reported stale")
	}
	if !RepoStale(st, "acme/old", StaleAfter) {
		t.Error("acme/old not reported stale")
	}
	if !RepoStale(st, "acme/never", StaleAfter) {
		t.Error("a repo that never synced should be stale")
	}
	if !RepoStale(st, "acme/future", StaleAfter) {
		t.Error("a future clock should be stale, not impossibly fresh")
	}
	if !RepoStale(st, "acme/missing", StaleAfter) {
		t.Error("a repo absent from the status should be stale")
	}

	// Repos that were never indexed hold no searchable data, so calling
	// them stale would describe results that cannot be returned at all.
	got := StaleRepos(st, StaleAfter)
	want := []string{"acme/future", "acme/never", "acme/old"}
	if len(got) != len(want) {
		t.Fatalf("StaleRepos = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("StaleRepos = %v, want %v (sorted)", got, want)
		}
	}
	if _, ok := RepoSuccessAge(nil, "acme/fresh"); ok {
		t.Error("RepoSuccessAge(nil) = ok, want false")
	}
}
