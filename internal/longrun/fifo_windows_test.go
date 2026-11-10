//go:build windows

package longrun

import "testing"

func mkfifo(t *testing.T, path string) {
	t.Helper()
	t.Skip("Windows has no FIFOs in the file system")
}
