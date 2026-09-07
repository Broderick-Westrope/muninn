//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package lock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock, translating the
// would-block errno into ErrLocked and an unsupported filesystem into
// ErrUnsupported.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return ErrLocked
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.EINVAL):
		// Some network and FUSE filesystems do not implement flock.
		return ErrUnsupported
	}
	return err
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
