//go:build unix

package proc

import (
	"errors"
	"syscall"
)

// alive sends signal 0: a process that exists but belongs to someone else
// still counts.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
