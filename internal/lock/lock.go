// Package lock provides an advisory whole-file lock used to keep two syncs
// from running against the same mirrors and shards at once. Overlapping
// runs are not merely wasteful: both call CleanTmp, so each deletes the
// temp files the other is mid-write on, and both write the status file with
// last-writer-wins.
//
// The lock is held by the open file description, so the kernel releases it
// when the process exits for any reason, including SIGKILL. That rules out
// the stale-lock problem a PID file would have, and means the lock file
// itself is never removed: unlinking it would let a second process create
// and lock a fresh inode while the first still holds the old one.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked reports that another process holds the lock. It is not a
// failure: callers are expected to skip their work and exit successfully.
var ErrLocked = errors.New("lock is held by another process")

// ErrUnsupported reports that the filesystem or platform provides no
// locking, so the caller proceeded unprotected. Callers should warn rather
// than abort: an unlockable state directory is a reason to lose the
// concurrency guarantee, not a reason to stop syncing.
var ErrUnsupported = errors.New("advisory locking is not supported here")

// Lock is an acquired advisory lock. It is not safe for concurrent use.
type Lock struct {
	f *os.File
}

// TryAcquire takes an exclusive lock on path without blocking, creating the
// file and its parent directory if needed.
//
// It returns an error wrapping ErrLocked when the lock is already held, and
// one wrapping ErrUnsupported when the platform or filesystem provides no
// locking. In the unsupported case the returned *Lock is still usable (and
// must still be released) so the caller can carry on unprotected after
// warning; every other error yields a nil Lock.
func TryAcquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", path, err)
	}
	if err := tryLock(f); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return &Lock{f: f}, fmt.Errorf("locking %s: %w", path, err)
		}
		f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("acquiring lock %s: %w", path, err)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. Closing the descriptor releases it, so the
// explicit unlock is only there to surface an error the close would hide.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := unlock(l.f)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return fmt.Errorf("unlocking: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing lock file: %w", closeErr)
	}
	return nil
}
