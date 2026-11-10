package overlay

import (
	"runtime"
	"testing"
)

func TestSyncLockName(t *testing.T) {
	cases := map[string]string{
		"/Users/x/.local/share/aiq/claude/a": "Users_x_.local_share_aiq_claude_a.sync.lock",
		"/a:b/c/":                            "a:b_c.sync.lock", // a colon on Unix stays, as before
	}
	if runtime.GOOS == "windows" {
		cases = map[string]string{
			`C:\Users\x\.local\share\aiq\claude\a`: "C_Users_x_.local_share_aiq_claude_a.sync.lock",
			`D:\aiq\a`:                             "D_aiq_a.sync.lock",
			`\\server\share\aiq\a`:                 "server_share_aiq_a.sync.lock",
			`\\?\C:\aiq\a`:                         "C_aiq_a.sync.lock",
		}
	}
	for in, want := range cases {
		if got := syncLockName(in); got != want {
			t.Errorf("syncLockName(%q) = %q, want %q", in, got, want)
		}
	}
}
