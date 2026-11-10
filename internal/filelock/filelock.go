// Package filelock takes whole-file locks that never wait. A lock lasts until
// Unlock or until the file is closed. On Unix it is flock(2); on Windows it is
// LockFileEx over the whole file.
package filelock

import (
	"errors"
	"os"
	"syscall"
)

// ErrWouldBlock is what TryExclusive and TryShared return when another holder
// has the lock. Test for it with errors.Is.
var ErrWouldBlock error = syscall.EWOULDBLOCK

// TryExclusive takes an exclusive lock on f, or fails at once.
func TryExclusive(f *os.File) error { return lock(f, true) }

// TryShared takes a shared lock on f, or fails at once.
func TryShared(f *os.File) error { return lock(f, false) }

// Unlock releases a lock taken on f.
func Unlock(f *os.File) error { return unlock(f) }

// Busy reports whether err means another holder has the lock.
func Busy(err error) bool { return errors.Is(err, ErrWouldBlock) }
