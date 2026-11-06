//go:build windows

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
)

// NewGroup starts cmd in a new process group. Windows has no signal that
// reaches a group, so TermGroup and KillGroup walk the process tree instead.
func NewGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// TermGroup ends cmd and every process it started. Windows has no polite
// stop a console program without a window would see, so this is the same
// forced stop as KillGroup.
func TermGroup(cmd *exec.Cmd) error { return KillGroup(cmd) }

// KillGroup ends cmd and every process it started, with taskkill /T /F.
func KillGroup(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
