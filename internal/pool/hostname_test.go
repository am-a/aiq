package pool

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// On Linux and macOS this host's identity is what it was before Windows had its own
// branch: /etc/machine-id, else the platform UUID from ioreg, else os.Hostname. Leases
// record it, so a change would orphan every lease this host holds.
func TestHostnameUnchangedOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pins the Unix identity")
	}
	want := func() string {
		if data, err := os.ReadFile("/etc/machine-id"); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
			if m := platformUUIDRe.FindSubmatch(out); m != nil {
				return string(m[1])
			}
		}
		h, _ := os.Hostname()
		return h
	}()
	saved := machineID
	machineID = ""
	t.Cleanup(func() { machineID = saved })
	if got := Hostname(); got != want {
		t.Fatalf("Hostname() = %q, want %q", got, want)
	}
}
