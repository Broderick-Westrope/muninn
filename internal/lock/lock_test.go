package lock

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTryAcquireExcludesSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "sync.lock")

	first, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}

	// flock is held by the open file description, so a second descriptor
	// conflicts even within one process.
	if _, err := TryAcquire(path); !errors.Is(err, ErrLocked) {
		t.Errorf("second TryAcquire err = %v, want ErrLocked", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")
	lk, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	if err := (*Lock)(nil).Release(); err != nil {
		t.Errorf("nil Release: %v", err)
	}
}

func TestLockFileSurvivesRelease(t *testing.T) {
	// Releasing must not unlink the file: a fresh inode would let two
	// processes each hold a lock on their own copy.
	path := filepath.Join(t.TempDir(), "sync.lock")
	lk, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file removed on release: %v", err)
	}
}

// TestLockReleasedOnProcessDeath is the property a PID file could not give
// us: a holder killed without cleanup must not wedge the lock forever.
func TestLockReleasedOnProcessDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")

	cmd := exec.Command(os.Args[0], "-test.run=TestLockHolderHelper")
	cmd.Env = append(os.Environ(), "MUNINN_LOCK_HELPER_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The helper blocks forever by design, so it is killed unconditionally
	// rather than only on the success path: a t.Fatal below would
	// otherwise leak it for the lifetime of the test binary.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	type ready struct {
		n   int
		err error
	}
	held := make(chan ready, 1)
	go func() {
		buf := make([]byte, len("locked\n"))
		n, err := io.ReadFull(stdout, buf)
		held <- ready{n, err}
	}()
	select {
	case r := <-held:
		// Without checking the read, a helper that dies before writing
		// would fall through and fail on the ErrLocked assertion instead,
		// reporting the wrong cause.
		if r.err != nil {
			t.Fatalf("helper did not announce the lock: %v", r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the helper to take the lock")
	}

	if _, err := TryAcquire(path); !errors.Is(err, ErrLocked) {
		t.Errorf("TryAcquire while helper holds the lock: err = %v, want ErrLocked", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	lk, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after holder was killed: %v (lock leaked)", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestLockHolderHelper is not a test: it is the subprocess body for
// TestLockReleasedOnProcessDeath, which takes the lock, announces it, and
// blocks until killed.
func TestLockHolderHelper(t *testing.T) {
	path := os.Getenv("MUNINN_LOCK_HELPER_PATH")
	if path == "" {
		t.Skip("not running as the lock holder helper")
	}
	if _, err := TryAcquire(path); err != nil {
		t.Fatalf("helper TryAcquire: %v", err)
	}
	os.Stdout.WriteString("locked\n")
	select {} // killed by the parent
}
