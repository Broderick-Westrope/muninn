package status

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
