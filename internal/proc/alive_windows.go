//go:build windows

package proc

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited.
const stillActive = 259

// alive opens the process for a status query. Only "no such process"
// (ERROR_INVALID_PARAMETER) means dead: a process this user may not open still
// exists, as EPERM does on Unix, and any other failure, such as a lack of
// resources, leaves the answer unknown, which counts as alive so a live lease is
// never dropped. A process that exited with code 259 reads as alive; Windows
// offers no way to tell the two apart through the exit code.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
