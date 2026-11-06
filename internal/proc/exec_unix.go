//go:build unix

package proc

import "syscall"

// Exec replaces this process with path, keeping its pid. It returns only
// on failure.
func Exec(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }
