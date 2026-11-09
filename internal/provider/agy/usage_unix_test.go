//go:build unix

package agy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pidFrom reads the pid a test CLI wrote, waiting for it to appear.
func pidFrom(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatal("the test CLI wrote no pid")
	return 0
}

// gone waits up to d for pid to end.
func gone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
	}
	return false
}

// A process the CLI left holding the output is ended once the poll has its answer, not
// left running after every poll.
func TestPollEndsWhatTheCLILeft(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := &Provider{Command: func(args, env []string) *exec.Cmd {
		cmd := exec.Command("sh", "-c", `sleep 30 & echo $! > "$AIQ_TEST_PID"; printf '%s' "$AIQ_TEST_OUTPUT"`)
		cmd.Env = append(env, "AIQ_TEST_OUTPUT="+usagePayload, "AIQ_TEST_PID="+pidFile)
		return cmd
	}}
	u, err := p.Poll(t.TempDir(), false, t.TempDir(), time.Now(), 20*time.Second)
	if err != nil || len(u.Windows) != 5 {
		t.Fatalf("poll: %+v %v", u, err)
	}
	if pid := pidFrom(t, pidFile); !gone(pid, 2*time.Second) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the process the CLI left (pid %d) still runs after the poll", pid)
	}
}

// One that ignores TERM is killed a few seconds later, whether the CLI exited or was
// stopped at the deadline.
func TestPollKillsWhatIgnoresTerm(t *testing.T) {
	for name, script := range map[string]string{
		"exited":  `(trap "" TERM; exec sleep 30) & echo $! > "$AIQ_TEST_PID"; printf '%s' "$AIQ_TEST_OUTPUT"`,
		"stopped": `(trap "" TERM; exec sleep 30) & echo $! > "$AIQ_TEST_PID"; sleep 100`,
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			p := &Provider{Command: func(args, env []string) *exec.Cmd {
				cmd := exec.Command("sh", "-c", script)
				cmd.Env = append(env, "AIQ_TEST_OUTPUT="+usagePayload, "AIQ_TEST_PID="+pidFile)
				return cmd
			}}
			timeout := 20 * time.Second
			if name == "stopped" {
				timeout = 300 * time.Millisecond
			}
			p.Poll(t.TempDir(), false, t.TempDir(), time.Now(), timeout)
			if pid := pidFrom(t, pidFile); !gone(pid, 6*time.Second) {
				syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("pid %d ignored TERM and was never killed", pid)
			}
		})
	}
}
