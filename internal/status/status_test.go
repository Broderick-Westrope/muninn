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
		{"last run failed", &SyncStatus{FinishedAt: now, LastSuccessAt: now.Add(-time.Hour)}, true},
		{"never succeeded", &SyncStatus{FinishedAt: now}, true},
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
			// The degraded case: stale and the last attempt failed, so
			// the retry cadence applies instead of the full interval.
			"degraded, retry elapsed",
			&SyncStatus{FinishedAt: ago(11 * time.Minute), LastSuccessAt: ago(3 * time.Hour)},
			true,
		},
		{
			// Same state, but too soon — reattempts are faster, not
			// unbounded.
			"degraded, retry not elapsed",
			&SyncStatus{FinishedAt: ago(2 * time.Minute), LastSuccessAt: ago(3 * time.Hour)},
			false,
		},
		{
			// A failing run must not buy freshness: were staleness
			// measured from FinishedAt this would be "fresh" forever.
			"degraded but success still within interval",
			&SyncStatus{FinishedAt: ago(time.Minute), LastSuccessAt: ago(30 * time.Minute)},
			false,
		},
		{
			"never succeeded, retry elapsed",
			&SyncStatus{FinishedAt: ago(30 * time.Minute)},
			true,
		},
		{
			"never succeeded, retry not elapsed",
			&SyncStatus{FinishedAt: ago(time.Minute)},
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
