//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// NewGroup makes cmd, once started, the leader of a process group of its
// own, so TermGroup and KillGroup reach whatever it starts in turn.
func NewGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// TermGroup sends SIGTERM to the group cmd leads.
func TermGroup(cmd *exec.Cmd) error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }

// KillGroup sends SIGKILL to the group cmd leads.
func KillGroup(cmd *exec.Cmd) error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

// GroupAlive reports whether the group cmd led still has a member. Signal 0 checks
// without sending anything; EPERM means a member exists that this user may not signal.
func GroupAlive(cmd *exec.Cmd) bool {
	err := syscall.Kill(-cmd.Process.Pid, 0)
	return err == nil || err == syscall.EPERM
}
