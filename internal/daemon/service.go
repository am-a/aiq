package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/binpath"
	"github.com/orlenko/aiq/internal/paths"
)

// A Service is one aiq user service: a launchd agent on macOS, a systemd --user unit on
// Linux. It runs the aiq binary with Args, restarts it when it exits, and starts at login.
type Service struct {
	Label       string   // the launchd label
	Unit        string   // the systemd unit file name
	Description string   // the systemd description
	Args        []string // the arguments after the aiq binary
	LogName     string   // the log file under paths.LogDir, on macOS
	// Env names variables to carry from the installing shell when set, so the service
	// reads the same data and config as the commands that installed it.
	Env []string
}

// Daemon is aiq's daemon service.
var Daemon = Service{Label: "dev.aiq.daemon", Unit: "aiq.service", Description: "aiq quota-aware account router",
	Args: []string{"daemon", "run"}, LogName: "daemon.log"}

func (s Service) launchdPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", s.Label+".plist")
}

func (s Service) systemdUnit() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", s.Unit)
}

// LogPath is where the service's output goes on macOS. On Linux it goes to the journal.
func (s Service) LogPath() string { return filepath.Join(paths.LogDir(), s.LogName) }

// carried lists the Env variables that are set, as NAME=value. They all name paths, and a
// relative one is made absolute against the installing directory: the service runs
// elsewhere, where the same text would mean another place.
func (s Service) carried() [][2]string {
	var out [][2]string
	for _, name := range s.Env {
		if v := os.Getenv(name); v != "" {
			if abs, err := filepath.Abs(v); err == nil {
				v = abs
			}
			out = append(out, [2]string{name, v})
		}
	}
	return out
}

// manage runs launchctl or systemctl. Tests replace it with a fake manager.
var manage = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// manageErr names the manager and its subcommand, the first argument that is not a flag.
func manageErr(name string, args []string, out []byte, err error) error {
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			sub = " " + a
			break
		}
	}
	return fmt.Errorf("%s%s: %v: %s", name, sub, err, strings.TrimSpace(string(out)))
}

func (s Service) launchdTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), s.Label) }

// Active reports whether the service manager has the service loaded and running. For
// launchd, loaded is running: KeepAlive starts it again whenever it exits.
func (s Service) Active() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := manage("launchctl", "print", s.launchdTarget())
		return err == nil
	case "linux":
		_, err := manage("systemctl", "--user", "is-active", "--quiet", s.Unit)
		return err == nil
	}
	return false
}

// ServicePATH is the PATH baked into the service: the shim directory, then
// the current PATH, then any entries only the user's interactive login shell
// adds. A takeover respawns `aiq run` with the daemon's environment, so a
// PATH captured from a bare ssh or cron shell (no nvm, no ~/.local/bin
// additions from .zshrc) leaves every successor with "claude not found on
// PATH". The shims lead because provider lookup starts after them: merging
// two PATHs can otherwise leave the shims behind /opt/homebrew/bin, and then
// the daemon finds no CLI at all.
func ServicePATH() string {
	return binpath.WithDirFirst(mergePATH(os.Getenv("PATH"), loginShellPATH()), paths.ShimsDir())
}

// FixPATH puts the shim directory first in this process's PATH and reports
// whether that changed anything. The daemon calls it at startup so a service
// installed with an older, badly ordered PATH heals itself on restart
// instead of failing every poll that has to launch a CLI.
func FixPATH() (changed bool) {
	current := os.Getenv("PATH")
	fixed := binpath.WithDirFirst(current, paths.ShimsDir())
	if fixed == current {
		return false
	}
	os.Setenv("PATH", fixed)
	return true
}

func mergePATH(first, extra string) string {
	seen := map[string]bool{}
	var out []string
	for _, list := range []string{first, extra} {
		for _, d := range filepath.SplitList(list) {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// loginShellPATH asks $SHELL, as an interactive login shell, for its PATH.
// Markers fence the value off from whatever the rc files print.
func loginShellPATH() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-lic", `printf '\n__AIQ_PATH__%s__AIQ_PATH__\n' "$PATH"`)
	cmd.Stdin = nil
	// A process the rc files start in the background can inherit stdout
	// and hold the pipe open after the shell is killed; stop waiting for
	// it shortly after, keeping whatever the shell printed.
	cmd.WaitDelay = 2 * time.Second
	out, _ := cmd.Output()
	_, rest, ok := strings.Cut(string(out), "__AIQ_PATH__")
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, "__AIQ_PATH__")
	if !ok {
		return ""
	}
	return value
}

// Install registers the daemon as a user service (launchd on macOS,
// systemd --user on Linux) and starts it. The service gets ServicePATH so
// it, and every session it respawns, can find the provider CLIs.
func Install(aiqPath string) (string, error) { return Daemon.Install(aiqPath) }

// Uninstall stops and removes the daemon service.
func Uninstall() (string, error) { return Daemon.Uninstall() }

// Installed reports whether the daemon's service definition exists.
func Installed() (string, bool) { return Daemon.Installed() }

// Install writes the service definition and starts it, replacing a running one.
func (s Service) Install(aiqPath string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return s.installLaunchd(aiqPath)
	case "linux":
		return s.installSystemd(aiqPath)
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

// Uninstall stops the service and removes its definition. When the stop fails, the
// definition stays, so the service can still be found and stopped, and the error says so.
func (s Service) Uninstall() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		plist := s.launchdPlist()
		if s.Active() {
			args := []string{"bootout", s.launchdTarget()}
			if out, err := manage("launchctl", args...); err != nil {
				return plist, fmt.Errorf("could not stop the service, so %s stays: %w", plist, manageErr("launchctl", args, out, err))
			}
		}
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return plist, nil
	case "linux":
		unit := s.systemdUnit()
		if _, err := os.Stat(unit); err == nil {
			args := []string{"--user", "disable", "--now", s.Unit}
			if out, err := manage("systemctl", args...); err != nil {
				return unit, fmt.Errorf("could not stop the service, so %s stays: %w", unit, manageErr("systemctl", args, out, err))
			}
		}
		if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		manage("systemctl", "--user", "daemon-reload")
		return unit, nil
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

// Restart restarts an installed service as it was installed, without rewriting its
// definition, so the binary path and environment it was given are kept. A service that
// is not running stays stopped: restarted reports whether it was running.
func (s Service) Restart() (restarted bool, err error) {
	if !s.Active() {
		return false, nil
	}
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "launchctl", []string{"kickstart", "-k", s.launchdTarget()}
	case "linux":
		name, args = "systemctl", []string{"--user", "restart", s.Unit}
	default:
		return false, fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	if out, err := manage(name, args...); err != nil {
		return false, manageErr(name, args, out, err)
	}
	return true, nil
}

// Runs reports whether the installed definition starts the binary at aiqPath. An error
// means the definition could not be read, which says nothing either way.
func (s Service) Runs(aiqPath string) (bool, error) {
	path, ok := s.Installed()
	if !ok {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	switch runtime.GOOS {
	case "darwin":
		return strings.Contains(string(b), "<string>"+xmlEscape(aiqPath)+"</string>"), nil
	default:
		return strings.Contains(string(b), "\nExecStart="+systemdWord(aiqPath)+" "), nil
	}
}

// Installed reports whether the service definition exists.
func (s Service) Installed() (path string, ok bool) {
	switch runtime.GOOS {
	case "darwin":
		path = s.launchdPlist()
	case "linux":
		path = s.systemdUnit()
	default:
		return "", false
	}
	_, err := os.Stat(path)
	return path, err == nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// plist is the launchd definition for the service.
func (s Service) plist(aiqPath, path string) string {
	home, _ := os.UserHomeDir()
	var args, env strings.Builder
	for _, a := range append([]string{aiqPath}, s.Args...) {
		fmt.Fprintf(&args, "    <string>%s</string>\n", xmlEscape(a))
	}
	for _, kv := range append([][2]string{{"PATH", path}, {"HOME", home}}, s.carried()...) {
		fmt.Fprintf(&env, "    <key>%s</key><string>%s</string>\n", xmlEscape(kv[0]), xmlEscape(kv[1]))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>EnvironmentVariables</key>
  <dict>
%s  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, xmlEscape(s.Label), args.String(), env.String(), xmlEscape(s.LogPath()), xmlEscape(s.LogPath()))
}

// systemdWord is one word of a systemd command line or assignment, as systemd reads it
// back: a plain word stays as it is; one with a space, quote or backslash is quoted.
// Specifiers (%) and variables ($) are doubled, so they are never expanded.
func systemdWord(w string) string {
	w = strings.NewReplacer("%", "%%", "$", "$$").Replace(w)
	if !strings.ContainsAny(w, " \t\"'\\;") {
		return w
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w) + `"`
}

// unit is the systemd definition for the service.
func (s Service) unit(aiqPath, path string) string {
	var words []string
	for _, w := range append([]string{aiqPath}, s.Args...) {
		words = append(words, systemdWord(w))
	}
	var env strings.Builder
	fmt.Fprintf(&env, "Environment=%s\n", systemdAssign("PATH", path))
	for _, kv := range s.carried() {
		fmt.Fprintf(&env, "Environment=%s\n", systemdAssign(kv[0], kv[1]))
	}
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target

[Service]
ExecStart=%s
%sRestart=always
RestartSec=5

[Install]
WantedBy=default.target
`, s.Description, strings.Join(words, " "), env.String())
}

// systemdAssign is NAME=value for Environment=, quoted whole when the value needs it.
// Environment= expands specifiers but not variables, so only % is doubled.
func systemdAssign(name, value string) string {
	kv := name + "=" + strings.ReplaceAll(value, "%", "%%")
	if !strings.ContainsAny(kv, " \t\"'\\") {
		return kv
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(kv) + `"`
}

func (s Service) installLaunchd(aiqPath string) (string, error) {
	plist := s.launchdPlist()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(plist, []byte(s.plist(aiqPath, ServicePATH())), 0o644); err != nil {
		return "", err
	}
	manage("launchctl", "unload", plist)
	if out, err := manage("launchctl", "load", plist); err != nil {
		return plist, fmt.Errorf("launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return plist, nil
}

func (s Service) installSystemd(aiqPath string) (string, error) {
	unit := s.systemdUnit()
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(unit, []byte(s.unit(aiqPath, ServicePATH())), 0o644); err != nil {
		return "", err
	}
	if out, err := manage("systemctl", "--user", "daemon-reload"); err != nil {
		return unit, fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := manage("systemctl", "--user", "enable", s.Unit); err != nil {
		return unit, fmt.Errorf("systemctl enable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// restart, not enable --now: a reinstall has to replace a running
	// service, as launchctl unload/load does on macOS.
	if out, err := manage("systemctl", "--user", "restart", s.Unit); err != nil {
		return unit, fmt.Errorf("systemctl restart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return unit, nil
}
