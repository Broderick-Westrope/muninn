//go:build unix

package lock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock, translating the
// would-block errno into ErrLocked.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
