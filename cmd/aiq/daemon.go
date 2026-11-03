package main

import (
	"fmt"
	"log"
	"os"

	"github.com/orlenko/aiq/internal/binpath"
	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/paths"
)

// listenRefusedAdvice says only what aiq knows about a refused address. aiq sends
// nothing there, so it cannot tell whether a daemon from an older version is still
// serving on it, and uninstalling also ends polling and long-session supervision.
func listenRefusedAdvice() string {
	s := "This version serves no status page or API on that address and did not contact it. " +
		"A daemon started by an older version may still be listening on the address it started with, which can reach the network if that address was not loopback. " + findDaemonListeners +
		fmt.Sprintf(" Set daemon.listen in %s to an address such as 127.0.0.1:7379, then run: aiq daemon install.", paths.ConfigFile())
	if _, installed := daemon.Installed(); installed {
		s += " To stop the service altogether, which also ends polling and long-session supervision: aiq daemon uninstall"
	}
	return s
}

// findDaemonListeners is how to see a daemon aiq cannot detect: an older one that
// holds no lock and was not started by the service.
const findDaemonListeners = "To see what any aiq process is listening on: lsof -nP -a -c aiq -iTCP -sTCP:LISTEN."

// daemonState is one line on what is running. The lock tells a daemon apart from
// anything else answering on the port, and shows a daemon that serves no HTTP. If
// the lock cannot be read, it assumes a daemon holds it and raises no alarm.
func daemonState(listen string) string {
	held, err := daemon.LockHeld()
	if err != nil {
		held = true
	}
	if err := daemon.CheckListen(listen); err != nil {
		if held {
			return fmt.Sprintf("running, polling and supervising, with no status page or API: %v. %s", err, listenRefusedAdvice())
		}
		return fmt.Sprintf("not running: %v. %s", err, listenRefusedAdvice())
	}
	alive := daemon.Alive(listen)
	switch {
	case alive && held:
		return fmt.Sprintf("running at http://%s/", listen)
	case alive:
		return fmt.Sprintf("something answers on http://%s/ and holds no daemon lock, so it is probably an older daemon. Run: aiq daemon install. If it still answers, it was not started by the service. %s", listen, findDaemonListeners)
	case held:
		return fmt.Sprintf("running, but not answering on %s; see %s", listen, daemon.LogPath())
	}
	return fmt.Sprintf("not answering on %s", listen)
}

// These are variables so tests can check which path reaches them without
// installing a service or starting a daemon.
var (
	installDaemonService = daemon.Install
	runDaemonServer      = func(a *app, logger *log.Logger) error { return daemon.New(a.cfg, a.pool, logger).Run() }
)

func cmdDaemon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq daemon run|install|uninstall|status")
	}
	switch args[0] {
	case "run":
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.close()
		logger := log.New(os.Stderr, "", log.LstdFlags)
		a.maybeInstallStatusline()
		a.maybeInstallAgyStatusline()
		return runDaemonServer(a, logger)
	case "install":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		a, err := openApp()
		if err != nil {
			return err
		}
		if err := daemon.CheckListen(a.cfg.Daemon.Listen); err != nil {
			a.close()
			return fmt.Errorf("%w. %s", err, listenRefusedAdvice())
		}
		a.maybeInstallStatusline()
		a.maybeInstallAgyStatusline()
		listen := a.cfg.Daemon.Listen
		a.close()
		path, err := installDaemonService(self)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s\nlog: %s\nui:  http://%s/\n", path, daemon.LogPath(), listen)
		// Takeovers start the CLI with the service's PATH; say so now if
		// that cannot find one, rather than when a successor dies.
		servicePath := daemon.ServicePATH()
		for _, provider := range config.Providers {
			saved := os.Getenv("PATH")
			os.Setenv("PATH", servicePath)
			_, err := binpath.Resolve(provider, "", paths.ShimsDir(), nil)
			os.Setenv("PATH", saved)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: the daemon's PATH has no %s, so a long session cannot be moved onto it; "+
					"reinstall from a login shell that has it, or set providers.%s.binary in the config\n", provider, provider)
			}
		}
		return nil
	case "uninstall":
		path, err := daemon.Uninstall()
		if err != nil {
			return err
		}
		fmt.Printf("removed %s\n", path)
		return nil
	case "status":
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.close()
		path, installed := daemon.Installed()
		fmt.Printf("service: %s (%s)\n", map[bool]string{true: "installed", false: "not installed"}[installed], path)
		fmt.Printf("daemon:  %s\n", daemonState(a.cfg.Daemon.Listen))
		fmt.Printf("log:     %s\n", daemon.LogPath())
		return nil
	}
	return fmt.Errorf("unknown daemon subcommand %q", args[0])
}
