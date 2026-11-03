package main

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if !strings.Contains(err.Error(), "may still be listening") || !strings.Contains(err.Error(), cfg) {
		t.Errorf("the refusal does not warn about an older daemon: %v", err)
	}
	if err := cmdDaemon([]string{"run"}); err != nil || !ran {
		t.Fatalf("run did not reach the server: err %v, ran %v", err, ran)
	}
}

// While the address is refused, the plan must not lead with an install that is
// certain to fail, and the only step is the one that fixes the address.
func TestDoctorPutsTheListenFixFirstWhenRefused(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	cfg := filepath.Join(root, "config.toml")
	t.Setenv("AIQ_CONFIG", cfg)
	if err := os.WriteFile(cfg, []byte("[daemon]\nlisten = \"0.0.0.0:7379\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	listen := 0
	for _, f := range a.doctorChecks().fixes {
		if f.command == "aiq daemon install" {
			t.Errorf("the plan has the refused install: %q", f.title)
		}
		if f.key == "daemon:listen" {
			listen++
		}
	}
	if listen != 1 {
		t.Errorf("%d listen fixes, want 1", listen)
	}
}

func TestDaemonStateSaysWhatIsRunning(t *testing.T) {
	data := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	answering := strings.TrimPrefix(srv.URL, "http://")
	closed := httptest.NewServer(http.NotFoundHandler())
	silent := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	check := func(when, listen, want string) {
		t.Helper()
		if got := daemonState(listen); !strings.Contains(got, want) {
			t.Errorf("%s: %q does not say %q", when, got, want)
		}
	}
	check("refused, no lock", "0.0.0.0:7379", "not running")
	check("nothing there", silent, "not answering")
	check("answers, no lock", answering, "holds no daemon lock")

	f, err := os.OpenFile(filepath.Join(data, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	check("refused, locked", "0.0.0.0:7379", "running, polling and supervising")
	check("answers, locked", answering, "running at http://")
	check("silent, locked", silent, "running, but not answering")
}
