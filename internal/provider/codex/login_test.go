package codex

import "testing"

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
