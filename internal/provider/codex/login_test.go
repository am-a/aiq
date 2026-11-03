package codex

import (
	"strings"
	"testing"
)

func TestHeadless(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		goos string
		want bool
	}{
		{"linux desktop", map[string]string{"DISPLAY": ":0"}, "linux", false},
		{"linux wayland", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, "linux", false},
		{"linux no display", nil, "linux", true},
		{"linux ssh with forwarded display", map[string]string{"DISPLAY": "localhost:10.0", "SSH_CONNECTION": "1.2.3.4 22 5.6.7.8 22"}, "linux", true},
		{"mac local", nil, "darwin", false},
		{"mac over ssh", map[string]string{"SSH_TTY": "/dev/ttys001"}, "darwin", true},
	}
	for _, c := range cases {
		getenv := func(k string) string { return c.env[k] }
		if got := Headless(getenv, c.goos); got != c.want {
			t.Errorf("%s: Headless = %v, want %v", c.name, got, c.want)
		}
	}
}

// The hint must name a command that works after `account add` already saved the
// account, which rerunning add does not.
func TestLoginHintNamesACommandThatWorks(t *testing.T) {
	for deviceAuth, want := range map[bool]string{
		true:  "aiq account login codex/work --browser",
		false: "aiq account login codex/work --device-auth",
	} {
		h := loginHint(deviceAuth, "codex/work")
		if !strings.Contains(h, want) || strings.Contains(h, "rerun with") || strings.ContainsAny(h, "<>") {
			t.Errorf("deviceAuth=%v: %s", deviceAuth, h)
		}
	}
}
