package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The daemon's definitions are exactly what aiq wrote before services were shared, so an
// update rewrites them unchanged.
func TestDaemonDefinitionsAreUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(home, "data"))
	bin, path := "/home/u/.local/bin/aiq", "/home/u/.local/share/aiq/shims:/usr/bin"
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.aiq.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
    <key>HOME</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, bin, path, home, LogPath(), LogPath())
	if got := Daemon.plist(bin, path); got != plist {
		t.Errorf("plist:\n%s\nwant:\n%s", got, plist)
	}
	unit := fmt.Sprintf(`[Unit]
Description=aiq quota-aware account router
After=network-online.target

[Service]
ExecStart=%s daemon run
Environment=PATH=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, bin, path)
	if got := Daemon.unit(bin, path); got != unit {
		t.Errorf("unit:\n%s\nwant:\n%s", got, unit)
	}
	if LogPath() != filepath.Join(home, "data", "log", "daemon.log") {
		t.Errorf("log path %s", LogPath())
	}
}

// A service carries the variables it names, when set, so it reads the same data as the
// shell that installed it.
func TestServiceCarriesItsVariables(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIQ_DATA_DIR", `/data/a&b 50% "x"`)
	os.Unsetenv("AIQ_CONFIG")
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service", Description: "test", Args: []string{"asks", "watch"},
		LogName: "test.log", Env: []string{"AIQ_DATA_DIR", "AIQ_CONFIG"}}
	plist := s.plist("/bin/aiq", "/usr/bin")
	if !strings.Contains(plist, "<key>AIQ_DATA_DIR</key><string>/data/a&amp;b 50% &quot;x&quot;</string>") || strings.Contains(plist, "AIQ_CONFIG") {
		t.Errorf("plist env:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>asks</string>\n    <string>watch</string>") {
		t.Errorf("plist args:\n%s", plist)
	}
	unit := s.unit("/bin/aiq", "/usr/bin")
	if !strings.Contains(unit, "ExecStart=/bin/aiq asks watch\n") || !strings.Contains(unit, `Environment="AIQ_DATA_DIR=/data/a&b 50%% \"x\""`+"\n") ||
		strings.Contains(unit, "AIQ_CONFIG") {
		t.Errorf("unit:\n%s", unit)
	}
}

// Runs tells whether the installed definition starts a given binary, and not one whose
// path only begins the same way.
func TestServiceRunsItsBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service", Description: "test", Args: []string{"asks", "watch"}, LogName: "test.log"}
	if s.Runs("/x/aiq") {
		t.Fatal("runs with nothing installed")
	}
	path, _ := s.Installed()
	if path == "" {
		t.Skip("no service manager on " + runtime.GOOS)
	}
	def := s.unit("/x/aiq", "/usr/bin")
	if runtime.GOOS == "darwin" {
		def = s.plist("/x/aiq", "/usr/bin")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.Runs("/x/aiq") {
		t.Error("does not see its own binary")
	}
	if s.Runs("/x/ai") || s.Runs("/y/aiq") {
		t.Error("sees another binary")
	}
}
