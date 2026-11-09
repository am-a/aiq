//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

// NewGroup starts cmd in a new process group. Windows has no signal that reaches a group.
func NewGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// TermGroup ends cmd. Windows has no polite stop a console program without a window would
// see, so this is the same forced stop as KillGroup.
func TermGroup(cmd *exec.Cmd) error { return KillGroup(cmd) }

// KillGroup ends cmd itself, through the handle Go holds for it, so it can never reach
// another process that reused the PID; after cmd has exited it returns an error and does
// nothing. Processes cmd started are not reached: on Windows nothing that calls this runs
// yet (agy's poll starts aiq __launch, which refuses before agy starts). A job object that
// holds the whole tree comes with running providers on Windows.
func KillGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }
