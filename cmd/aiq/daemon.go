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
		"A daemon that is already running keeps the address it started with, which can reach the network if that address was not loopback. " + findDaemonListeners +
		fmt.Sprintf(" Set daemon.listen in %s to an address such as 127.0.0.1:7379, then run: aiq daemon install. To read the page from another machine, use an SSH tunnel.", paths.ConfigFile())
	if _, installed := daemon.Installed(); installed {
		s += " To stop the service altogether, which also ends polling and long-session supervision: aiq daemon uninstall"
	}
	return s
}

// findDaemonListeners is how to see a daemon aiq cannot detect, such as an older one
// started by hand, which the service can neither stop nor replace.
const findDaemonListeners = "To see what any aiq process listens on, run `lsof -nP -a -c aiq -iTCP -sTCP:LISTEN`; no output means none, and a daemon serving no HTTP does not appear. Stop one started by hand yourself."

// lockFinding says what the lock shows, as an observation: see daemon.LockHeld for
// what it cannot show.
func lockFinding() (held bool, unknown string) {
	held, err := daemon.LockHeld()
	if err != nil {
		return false, fmt.Sprintf("the daemon lock could not be read (%v), so aiq does not know whether a daemon holds it", err)
	}
	return held, ""
}

// daemonState is one line on what aiq can observe. It asks the port first: a daemon
// takes the lock before it binds, so one that answers already holds the lock.
func daemonState(listen string) string {
	if err := daemon.CheckListen(listen); err != nil {
		held, unknown := lockFinding()
		switch {
		case unknown != "":
			return fmt.Sprintf("%v. %s. %s", err, unknown, listenRefusedAdvice())
		case held:
			return fmt.Sprintf("a daemon holds the lock, so one is polling and supervising. If it started with this setting it serves no status page or API: %v. %s", err, listenRefusedAdvice())
		}
		return fmt.Sprintf("no daemon holds the lock: %v. An older daemon would not show here. %s", err, listenRefusedAdvice())
	}
	alive := daemon.Alive(listen)
	held, unknown := lockFinding()
	switch {
	case unknown != "" && alive:
		return fmt.Sprintf("something answers on http://%s/, but %s", listen, unknown)
	case unknown != "":
		return fmt.Sprintf("not answering on %s, and %s", listen, unknown)
	case alive && held:
		return fmt.Sprintf("running at http://%s/", listen)
	case alive:
		return fmt.Sprintf("something answers on http://%s/, and no daemon holds the lock in %s. It may be an older daemon, or one using another data directory. Run: aiq daemon install. If it still answers, the service did not start it. %s", listen, paths.DataDir(), findDaemonListeners)
	case held:
		return fmt.Sprintf("a daemon holds the lock, but nothing answers on %s; see %s", listen, daemon.LogPath())
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
		fmt.Printf("installed %s\nlog: %s\nui:  http://%s/ (to see whether it started: aiq daemon status)\n", path, daemon.LogPath(), listen)
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
		if path, installed := daemon.Installed(); !installed {
			fmt.Printf("no service installed at %s\n", path)
			return nil
		}
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
