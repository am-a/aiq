package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func asksServiceTestHome(t *testing.T) (installs *int32, installed *bool) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config", "config.toml"))
	var n int32
	present := false
	oldInstall, oldUninstall, oldInstalled := installAsksService, uninstallAsksService, asksServiceInstalled
	t.Cleanup(func() {
		installAsksService, uninstallAsksService, asksServiceInstalled = oldInstall, oldUninstall, oldInstalled
	})
	installAsksService = func(self string) (string, error) {
		if self == "" {
			t.Error("installed with no binary path")
		}
		atomic.AddInt32(&n, 1)
		present = true
		return "/example/aiq-asks.service", nil
	}
	uninstallAsksService = func() (string, error) { present = false; return "/example/aiq-asks.service", nil }
	asksServiceInstalled = func() (string, bool) { return "/example/aiq-asks.service", present }
	return &n, &present
}

func writeAsksTestKey(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(asksKeyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asksKeyPath(), []byte("example-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With no key, installing the service refuses, so nothing is set up that could only fail.
func TestAsksServiceInstallNeedsAKey(t *testing.T) {
	installs, _ := asksServiceTestHome(t)
	if err := asksServiceCmd([]string{"install"}); err == nil {
		t.Fatal("installed with no key")
	}
	if *installs != 0 {
		t.Fatal("the service was written with no key")
	}
	writeAsksTestKey(t)
	if err := asksServiceCmd([]string{"install"}); err != nil {
		t.Fatal(err)
	}
	if *installs != 1 {
		t.Fatalf("installs = %d", *installs)
	}
}

// Refresh, which the installer runs after every update, restarts an installed service and
// never installs one: an update must not start sending.
func TestAsksServiceRefreshOnlyRestartsAnInstalledService(t *testing.T) {
	installs, installed := asksServiceTestHome(t)
	writeAsksTestKey(t)
	if err := asksServiceCmd([]string{"refresh"}); err != nil {
		t.Fatal(err)
	}
	if *installs != 0 || *installed {
		t.Fatal("refresh installed a service that was not there")
	}
	*installed = true
	if err := asksServiceCmd([]string{"refresh"}); err != nil {
		t.Fatal(err)
	}
	if *installs != 1 {
		t.Fatalf("refresh did not restart the installed service: installs = %d", *installs)
	}
	if err := asksServiceCmd([]string{"uninstall"}); err != nil || *installed {
		t.Fatalf("uninstall: %v, installed %v", err, *installed)
	}
}

// A service watcher waits while another watcher runs, says so once, and starts when it stops.
func TestAsksServiceWaitsForTheWatcherPlace(t *testing.T) {
	asksServiceTestHome(t)
	held, err := holdAsksWatch()
	if err != nil {
		t.Fatal(err)
	}
	if got := asksWatcherState(); got != "running" {
		t.Fatalf("state with a watcher = %q", got)
	}
	var said int32
	logf := func(string, ...any) { atomic.AddInt32(&said, 1) }
	got := make(chan *os.File, 1)
	go func() {
		f, err := waitAsksWatch(context.Background(), 10*time.Millisecond, logf)
		if err != nil {
			t.Error(err)
		}
		got <- f
	}()
	select {
	case <-got:
		t.Fatal("took the place while another watcher held it")
	case <-time.After(200 * time.Millisecond):
	}
	if n := atomic.LoadInt32(&said); n != 1 {
		t.Fatalf("said it was waiting %d times", n)
	}
	held.Close()
	select {
	case f := <-got:
		if f == nil {
			t.Fatal("no lock after the other watcher stopped")
		}
		f.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("did not start after the other watcher stopped")
	}
	if got := asksWatcherState(); got != "not running" {
		t.Fatalf("state with none = %q", got)
	}
	// Stopped while waiting, it returns without the place.
	held, err = holdAsksWatch()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, err := waitAsksWatch(ctx, time.Hour, func(string, ...any) {}); f != nil || err != nil {
		t.Fatalf("stopped wait gave %v, %v", f, err)
	}
}
