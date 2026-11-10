//go:build windows

package proc

import "fmt"

// Exec would replace this process with path. Windows cannot replace a
// running process's image, and aiq does not yet run the program as a child
// in its place, so this always fails.
func Exec(path string, argv, env []string) error {
	return fmt.Errorf("starting %s in place of aiq is not supported on Windows yet", path)
}
