//go:build !unix

package lock

import "os"

// Platforms without flock get no advisory locking. muninn is released for
// darwin only (the scheduled sync is a launchd agent), so this exists to
// keep cross-compilation building rather than to provide protection.
func tryLock(*os.File) error { return nil }

func unlock(*os.File) error { return nil }
