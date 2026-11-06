package main

import (
	"errors"
	"fmt"
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
	uninstallAsksService = asksService.Uninstall
	asksServiceInstalled = asksService.Installed
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
		// The installer calls this after an update, so a service runs the new binary. With no
		// service installed it does nothing: an update never starts sending.
		if _, ok := asksServiceInstalled(); !ok {
			return nil
		}
		path, err := installAsksServiceHere()
		if err != nil {
			return err
		}
		fmt.Printf("asks service: restarted (%s)\n", path)
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
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return installAsksService(self)
}

// asksWatcherState says whether some process holds the one watcher's place on this machine.
func asksWatcherState() string {
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), asksWatchLock), os.O_RDONLY, 0)
	if err != nil {
		return "not running"
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
