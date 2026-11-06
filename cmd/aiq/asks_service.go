package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/paths"
)

// asksService runs the watcher as a user service, so it starts at login and again after
// a crash. It carries the variables that place aiq's data and config.
var asksService = daemon.Service{Label: "dev.aiq.asks", Unit: "aiq-asks.service", Description: "aiq asks watcher",
	Args: []string{"asks", "watch", "--service"}, LogName: "asks.log",
	Env: []string{"AIQ_DATA_DIR", "XDG_DATA_HOME", "AIQ_CONFIG", "XDG_CONFIG_HOME"}}

// These are variables so tests can check what reaches them without installing a service.
var (
	installAsksService   = asksService.Install
	restartAsksService   = asksService.Restart
	uninstallAsksService = asksService.Uninstall
	asksServiceInstalled = asksService.Installed
	asksServiceRuns      = asksService.Runs
)

func asksServiceLog() string {
	if runtime.GOOS == "linux" {
		return "journalctl --user -u " + asksService.Unit
	}
	return asksService.LogPath()
}

func asksServiceCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: %s service install|uninstall|status", asksCmd)
	}
	switch args[0] {
	case "install":
		if asksKey() == "" {
			return errors.New("no key: run " + asksCmd + " setup first")
		}
		path, err := installAsksServiceHere()
		if err != nil {
			return err
		}
		fmt.Printf("installed %s\nThe watcher now runs in the background, starts at login, and restarts if it stops.\n"+
			"A watcher already running in a window keeps going; the service takes over when it stops.\nlog: %s\n", path, asksServiceLog())
		return nil
	case "refresh":
		// The installer calls this after an update, so a service runs the new binary. It
		// restarts the service as installed, keeping the data and config locations it was
		// given, whatever the updating shell has. With no service installed it does nothing:
		// an update never starts sending.
		// A service you stopped stays stopped: an update never starts one.
		path, ok := asksServiceInstalled()
		if !ok {
			return nil
		}
		result, err := restartAsksService()
		if err != nil {
			return err
		}
		switch result {
		case daemon.NotRunning:
			fmt.Printf("asks service: installed and not running, so not restarted (%s). A stopped service starts again at login; %s service uninstall keeps it off\n", path, asksCmd)
			return nil
		case daemon.DownAfterRestart:
			return fmt.Errorf("asks service: was running, and is not running after the restart (%s). If you stopped it, it stays stopped; if not, the new process failed or is waiting to retry: see %s", path, asksServiceLog())
		}
		fmt.Printf("asks service: restarted (%s)\n", path)
		if self, err := asksSelf(); err == nil {
			switch runs, err := asksServiceRuns(self); {
			case err != nil:
				fmt.Fprintf(os.Stderr, "warning: could not read %s to check which aiq it runs: %v\n", path, err)
			case !runs:
				fmt.Fprintf(os.Stderr, "warning: %s names another aiq binary than %s; to move it, run %s service install\n", path, self, asksCmd)
			}
		}
		return nil
	case "uninstall":
		_, installed := asksServiceInstalled()
		path, err := uninstallAsksService()
		if err != nil {
			return err
		}
		if installed {
			fmt.Printf("removed %s\n", path)
		} else {
			fmt.Printf("no service installed at %s\n", path)
		}
		return nil
	case "status":
		path, installed := asksServiceInstalled()
		fmt.Printf("service: %s (%s)\n", map[bool]string{true: "installed", false: "not installed"}[installed], path)
		fmt.Printf("watcher: %s\n", asksWatcherState())
		fmt.Printf("log:     %s\n", asksServiceLog())
		return nil
	}
	return fmt.Errorf("usage: %s service install|uninstall|status", asksCmd)
}

func installAsksServiceHere() (string, error) {
	self, err := asksSelf()
	if err != nil {
		return "", err
	}
	return installAsksService(self)
}

// asksSelf is this binary's path, as a service definition names it.
func asksSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

// asksWatcherState says whether some process holds the one watcher's place for this data
// directory. Only an absent lock file, or a free lock, reads as not running; a lock that
// cannot be checked says so.
func asksWatcherState() string {
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), asksWatchLock), os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "not running"
	}
	if err != nil {
		return "unknown: " + err.Error()
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "running"
		}
		return "unknown: " + err.Error()
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return "not running"
}
