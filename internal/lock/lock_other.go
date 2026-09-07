//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package lock

import "os"

// Platforms without flock get no locking. muninn is released for darwin
// only (the scheduled sync is a launchd agent), so this exists to keep
// `go build` working elsewhere. It reports ErrUnsupported rather than
// silently claiming success, so callers warn that concurrent syncs are
// unprotected instead of assuming they cannot happen.
func tryLock(*os.File) error { return ErrUnsupported }

func unlock(*os.File) error { return nil }
