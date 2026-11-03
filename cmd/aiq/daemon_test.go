package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refused address stops an install, and must not stop the daemon itself: the
// service runs `aiq daemon run`, which has to reach the mode that keeps polling and
// supervising without serving HTTP.
func TestDaemonListenRefusedOnInstallOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	cfg := filepath.Join(root, "config.toml")
	t.Setenv("AIQ_CONFIG", cfg)
	body := "[daemon]\nlisten = \"0.0.0.0:7379\"\n[telemetry]\nclaude_statusline = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, ran := false, false
	oldInstall, oldRun := installDaemonService, runDaemonServer
	defer func() { installDaemonService, runDaemonServer = oldInstall, oldRun }()
	installDaemonService = func(string) (string, error) { installed = true; return "", nil }
	runDaemonServer = func(*app, *log.Logger) error { ran = true; return nil }

	err := cmdDaemon([]string{"install"})
	if err == nil || installed {
		t.Fatalf("install was not refused: err %v, installed %v", err, installed)
	}
	if !strings.Contains(err.Error(), "may still be listening") {
		t.Errorf("the refusal does not warn about an older daemon: %v", err)
	}
	if err := cmdDaemon([]string{"run"}); err != nil || !ran {
		t.Fatalf("run did not reach the server: err %v, ran %v", err, ran)
	}
}
