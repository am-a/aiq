package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if got := mustDef(Daemon.plist(bin, path)); got != plist {
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
	if got := mustDef(Daemon.unit(bin, path)); got != unit {
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
	plist := mustDef(s.plist("/bin/aiq", "/usr/bin"))
	if !strings.Contains(plist, "<key>AIQ_DATA_DIR</key><string>/data/a&amp;b 50% &quot;x&quot;</string>") || strings.Contains(plist, "AIQ_CONFIG") {
		t.Errorf("plist env:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>asks</string>\n    <string>watch</string>") {
		t.Errorf("plist args:\n%s", plist)
	}
	unit := mustDef(s.unit("/bin/aiq", "/usr/bin"))
	if !strings.Contains(unit, "ExecStart=/bin/aiq asks watch\n") || !strings.Contains(unit, `Environment="AIQ_DATA_DIR=/data/a&b 50%% \"x\""`+"\n") ||
		strings.Contains(unit, "AIQ_CONFIG") {
		t.Errorf("unit:\n%s", unit)
	}
}

// Runs tells whether the installed definition starts a given binary, and not one whose
// path only begins the same way. A definition it cannot read is an error, not a mismatch.
func TestServiceRunsItsBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service", Description: "test", Args: []string{"asks", "watch"}, LogName: "test.log"}
	if runs, err := s.Runs("/x/aiq"); runs || err != nil {
		t.Fatalf("with nothing installed: %v, %v", runs, err)
	}
	path := writeTestDefinition(t, s, "/x a/aiq")
	if runs, err := s.Runs("/x a/aiq"); !runs || err != nil {
		t.Errorf("own binary: %v, %v", runs, err)
	}
	for _, other := range []string{"/x a/ai", "/y/aiq"} {
		if runs, _ := s.Runs(other); runs {
			t.Errorf("sees %s", other)
		}
	}
	if os.Getuid() != 0 {
		os.Chmod(path, 0)
		defer os.Chmod(path, 0o644)
		if _, err := s.Runs("/x a/aiq"); err == nil {
			t.Error("an unreadable definition read as an answer")
		}
	}
}

func writeTestDefinition(t *testing.T, s Service, bin string) string {
	t.Helper()
	path, _ := s.Installed()
	if path == "" {
		t.Skip("no service manager on " + runtime.GOOS)
	}
	def := mustDef(s.unit(bin, "/usr/bin"))
	if runtime.GOOS == "darwin" {
		def = mustDef(s.plist(bin, "/usr/bin"))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeManager stands in for launchctl and systemctl: active answers print and is-active,
// stopErr answers bootout and disable, restartErr answers kickstart and restart.
type fakeManager struct {
	active              bool
	stopErr, restartErr error
	queryErr            error         // answers print and is-active instead of active
	userDomain          bool          // launchd: loaded in the user domain, not the GUI one
	stopOnRestart       bool          // the owner stops it just before the restart
	downAfterRestart    bool          // the restarted process fails at once (launchd: loaded, cannot spawn)
	respawning          bool          // launchd: running, but a new pid at each look (it keeps exiting)
	briefly             time.Duration // launchd: runs this long after a bootstrap, then exits
	started             time.Time
	pid                 int
	noUnit              bool // systemd: no such unit loaded
	noGUIDomain         bool // launchd: no GUI login, so print gui/... exits 112
	calls               []string
}

// exitErr is a manager's exit status, as exec reports it.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func (f *fakeManager) use(t *testing.T) {
	old := manage
	t.Cleanup(func() { manage = old })
	manage = func(name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		for _, a := range args {
			switch a {
			case "print", "is-active":
				if f.queryErr != nil {
					return []byte("Failed to connect to bus"), f.queryErr
				}
				if name == "launchctl" && f.noGUIDomain && strings.HasPrefix(args[len(args)-1], "gui/") {
					return []byte("Could not find specified domain"), exitErr(112)
				}
				loadedHere := true
				if name == "launchctl" {
					loadedHere = strings.HasPrefix(args[len(args)-1], "user/") == f.userDomain
				}
				if !f.active || !loadedHere {
					// launchctl: no such service; systemctl: inactive.
					return nil, map[string]exitErr{"launchctl": 113, "systemctl": 3}[name]
				}
				if name == "launchctl" && (f.downAfterRestart || f.briefly > 0 && time.Since(f.started) > f.briefly) {
					return []byte("\tstate = spawn scheduled\n"), nil
				}
				f.pid++ // a process that keeps running answers with one pid
				if !f.respawning {
					f.pid = 42
				}
				return []byte(fmt.Sprintf("\tstate = running\n\tpid = %d\n", f.pid)), nil
			case "bootstrap":
				if f.restartErr != nil {
					return []byte("Bootstrap failed: 5: Input/output error"), f.restartErr
				}
				f.active, f.started = true, time.Now()
				return nil, nil
			case "bootout", "disable", "stop":
				if name == "launchctl" && f.stopOnRestart {
					f.active = false
					return []byte("Boot-out failed: 3: No such process"), exitErr(3)
				}
				if name == "systemctl" && a == "stop" && f.noUnit {
					return []byte("Unit not loaded."), exitErr(5)
				}
				if f.stopErr == nil {
					f.active = false
				}
				return []byte("refused"), f.stopErr
			case "unload":
				t.Error("launchctl unload exits 0 even when it fails, so it proves nothing")
				return nil, nil
			case "kickstart", "try-restart":
				if f.stopOnRestart || f.downAfterRestart {
					f.active = false
					if name == "launchctl" && f.stopOnRestart {
						return nil, exitErr(113)
					}
					return nil, nil // try-restart leaves a stopped unit alone
				}
				return []byte("refused"), f.restartErr
			}
		}
		return nil, nil
	}
}

func (f *fakeManager) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(" "+c+" ", " "+sub+" ") {
			return true
		}
	}
	return false
}

// A stop that fails keeps the definition, so the service can still be found and stopped,
// and says so; a service that is not running is removed without a stop.
func TestServiceUninstallKeepsADefinitionItCouldNotStop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service", Description: "test", Args: []string{"asks", "watch"}, LogName: "test.log"}
	path := writeTestDefinition(t, s, "/x/aiq")
	f := &fakeManager{active: true, stopErr: errors.New("exit status 23")}
	f.use(t)
	if _, err := s.Uninstall(); err == nil || !strings.Contains(err.Error(), "could not stop") {
		t.Fatalf("a failed stop gave %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the definition went with the service still running")
	}
	f.stopErr = nil
	if _, err := s.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the definition stayed after a stop")
	}
	if runtime.GOOS == "darwin" {
		// Loaded in the user domain, as by a load over ssh with no GUI login: stopped there.
		path = writeTestDefinition(t, s, "/x/aiq")
		f.active, f.userDomain, f.calls = true, true, nil
		if _, err := s.Uninstall(); err != nil {
			t.Fatal(err)
		}
		if !f.called("user/"+strconv.Itoa(os.Getuid())+"/dev.aiq.test") || f.active {
			t.Errorf("calls %q: the job in the user domain was left running", f.calls)
		}
		f.active, f.stopErr = true, errors.New("exit status 5")
		path = writeTestDefinition(t, s, "/x/aiq")
		if _, err := s.Uninstall(); err == nil {
			t.Fatal("a failed stop in the user domain read as removed")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("the definition went with the service still running")
		}
		// No GUI login, so no GUI domain: the user domain is still checked and stopped.
		f.active, f.stopErr, f.noGUIDomain, f.calls = true, nil, true, nil
		if _, err := s.Uninstall(); err != nil {
			t.Fatalf("no GUI domain: %v", err)
		}
		if f.active {
			t.Errorf("calls %q: the job in the user domain was left running", f.calls)
		}
		path = writeTestDefinition(t, s, "/x/aiq")
		f.noGUIDomain = false
		// Loaded nowhere: nothing to stop, and the plist goes.
		f.active, f.stopErr, f.calls = false, nil, nil
		if _, err := s.Uninstall(); err != nil {
			t.Fatalf("a service not loaded: %v", err)
		}
		if f.called("bootout") {
			t.Errorf("calls %q: stopped a service not loaded", f.calls)
		}
		if _, err := os.Stat(path); err == nil {
			t.Error("the definition stayed")
		}
	}
}

// Restart restarts only a running service; one that is stopped stays stopped.
func TestServiceRestartLeavesAStoppedServiceStopped(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service"}
	f := &fakeManager{}
	f.use(t)
	oldWait, oldPoll := restartWait, restartPoll
	oldSettle := restartSettle
	restartWait, restartSettle, restartPoll = 200*time.Millisecond, 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { restartWait, restartSettle, restartPoll = oldWait, oldSettle, oldPoll })
	if got, err := s.Restart(); got != NotRunning || err != nil {
		t.Fatalf("stopped service: %v, %v", got, err)
	}
	if f.called("kickstart") || f.called("try-restart") || f.called("bootstrap") {
		t.Fatal("started a stopped service")
	}
	f.active = true
	if got, err := s.Restart(); got != Running || err != nil {
		t.Fatalf("running service: %v, %v", got, err)
	}
	if runtime.GOOS == "darwin" {
		// Loaded afresh from its plist, as install does; kickstart reuses a job that may
		// not spawn the updated binary.
		if f.called("kickstart") || !f.called("bootout") || !f.called("bootstrap") {
			t.Fatalf("restart on macOS: %q", f.calls)
		}
	}
	f.restartErr = errors.New("exit status 1")
	if _, err := s.Restart(); err == nil {
		t.Fatal("a failed restart reported success")
	}
	f.restartErr, f.active = nil, true
	// Stopped between the check and the restart: it stays stopped, and the restart says
	// it is down rather than that it was never running.
	f.restartErr, f.stopOnRestart = nil, true
	if got, err := s.Restart(); got != DownAfterRestart || err != nil {
		t.Fatalf("stopped during the restart: %v, %v", got, err)
	}
	if f.active {
		t.Fatal("the restart started a service stopped just before it")
	}
	// A manager that cannot answer, such as with no user bus or no GUI domain, is not
	// "stopped": the restart fails and nothing is restarted.
	f.active, f.stopOnRestart, f.queryErr, f.calls = true, false, exitErr(112), nil
	if got, err := s.Restart(); err == nil || got != NotRunning {
		t.Fatalf("a failed query gave %v, %v", got, err)
	}
	if f.called("kickstart") || f.called("try-restart") {
		t.Fatal("restarted without knowing the state")
	}
	if runtime.GOOS == "linux" {
		f.queryErr = exitErr(4)
		if got, err := s.Restart(); err != nil || got != NotRunning {
			t.Fatalf("no such unit gave %v, %v", got, err)
		}
	}
	// Running, restarted, and the new process fails at once (on macOS: loaded, and the
	// spawn fails): down, not "was stopped" and not "restarted".
	f.active, f.queryErr, f.downAfterRestart = true, nil, true
	if got, err := s.Restart(); got != DownAfterRestart || err != nil {
		t.Fatalf("a restart that failed gave %v, %v", got, err)
	}
	if runtime.GOOS == "darwin" {
		// Running for a moment at each spawn, then gone: not restarted.
		f.active, f.downAfterRestart, f.respawning = true, false, true
		if got, err := s.Restart(); got != DownAfterRestart || err != nil {
			t.Fatalf("a process that keeps exiting gave %v, %v", got, err)
		}
		// Running for a moment, with one pid, and then gone: not restarted either.
		f.active, f.respawning, f.briefly = true, false, 3*time.Millisecond
		if got, err := s.Restart(); got != DownAfterRestart || err != nil {
			t.Fatalf("a process that ran for a moment gave %v, %v", got, err)
		}
	}
}

// Uninstall when the manager cannot say whether the service runs: on macOS the plist
// stays; on Linux, with no unit file, the job is stopped whatever is-active says, since
// "not active" includes a job waiting to restart.
func TestServiceUninstallWhenTheStateIsUnclear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service", Description: "test", Args: []string{"asks", "watch"}, LogName: "test.log"}
	switch runtime.GOOS {
	case "darwin":
		path := writeTestDefinition(t, s, "/x/aiq")
		// 5: an I/O error from a domain that exists.
		f := &fakeManager{queryErr: exitErr(5)}
		f.use(t)
		if _, err := s.Uninstall(); err == nil {
			t.Fatal("removed a service it could not see")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("the plist went")
		}
	case "linux":
		f := &fakeManager{active: true}
		f.use(t)
		if _, err := s.Uninstall(); err != nil {
			t.Fatal(err)
		}
		if !f.called("stop") || f.active {
			t.Errorf("a loaded job with no unit file was left running: %q", f.calls)
		}
		f.calls = nil
		if _, err := s.Uninstall(); err != nil {
			t.Fatal(err)
		}
		if !f.called("stop") {
			t.Errorf("a job that is not active, which may be waiting to restart, was not stopped: %q", f.calls)
		}
		f.noUnit = true
		if _, err := s.Uninstall(); err != nil {
			t.Errorf("no such job: %v", err)
		}
		f.noUnit, f.stopErr = false, exitErr(1)
		if _, err := s.Uninstall(); err == nil {
			t.Error("a failed stop read as nothing to stop")
		}
	default:
		t.Skip("no service manager on " + runtime.GOOS)
	}
}

// A carried path that cannot be made absolute, as when the installing directory was
// deleted, stops the install before anything is written or started.
func TestServiceInstallRefusesAPathItCannotPlace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Abs("data"); err == nil {
		t.Skip("this OS still names a deleted working directory")
	}
	t.Setenv("AIQ_DATA_DIR", "data")
	s := Service{Label: "dev.aiq.test", Unit: "aiq-test.service", Description: "test", Args: []string{"asks", "watch"},
		LogName: "test.log", Env: []string{"AIQ_DATA_DIR"}}
	f := &fakeManager{}
	f.use(t)
	if _, err := s.Install("/x/aiq"); err == nil {
		t.Fatal("installed with a path it could not place")
	}
	if path, ok := s.Installed(); ok {
		t.Fatalf("wrote %s", path)
	}
	if len(f.calls) != 0 {
		t.Fatalf("called the manager: %q", f.calls)
	}
	// The definitions refuse it too, so none is ever written without its paths.
	if _, err := s.unit("/x/aiq", "/usr/bin"); err == nil {
		t.Error("a unit without its carried paths")
	}
	if _, err := s.plist("/x/aiq", "/usr/bin"); err == nil {
		t.Error("a plist without its carried paths")
	}
}

// A unit for a home with spaces, quotes or specifiers reads back as the same words.
func TestSystemdUnitQuotesWhatNeedsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIQ_TEST_ROOT", "/data/50%")
	s := Service{Description: "test", Args: []string{"asks", "a$b"}, Env: []string{"AIQ_TEST_ROOT"}}
	unit := mustDef(s.unit("/home/a b/$x/aiq", "/home/a b/bin:/usr/bin"))
	for _, want := range []string{
		// systemd expands $ in arguments but not in the executable path.
		"ExecStart=\"/home/a b/$x/aiq\" asks a$$b\n",
		"Environment=\"PATH=/home/a b/bin:/usr/bin\"\n",
		"Environment=AIQ_TEST_ROOT=/data/50%%\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}

// A relative path is made absolute against the installing directory.
func TestServiceCarriesRelativePathsAsAbsolute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("AIQ_DATA_DIR", "data")
	s := Service{Description: "test", Args: []string{"asks", "watch"}, Env: []string{"AIQ_DATA_DIR"}}
	want := filepath.Join(dir, "data")
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		// Abs uses the working directory as the OS reports it, which may resolve links.
		if cwd, _ := os.Getwd(); cwd == real {
			want = filepath.Join(real, "data")
		}
	}
	unit := mustDef(s.unit("/bin/aiq", "/usr/bin"))
	if !strings.Contains(unit, "Environment=AIQ_DATA_DIR="+want+"\n") {
		t.Errorf("unit:\n%s\nwant AIQ_DATA_DIR=%s", unit, want)
	}
}

func mustDef(def string, err error) string {
	if err != nil {
		panic(err)
	}
	return def
}
