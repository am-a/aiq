// Package codex implements the Codex provider. Each account is a CODEX_HOME
// overlay: every entry of the real ~/.codex is symlinked in (config,
// sessions, skills, sqlite state), and only auth.json belongs to the account.
package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/overlay"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
)

// Provider launches Codex. Command builds a child process for the real CLI
// (the caller decides how the binary is found).
type Provider struct {
	Command func(args []string, env []string) *exec.Cmd
	// DeviceAuth makes Login use `codex login --device-auth`: a code typed
	// into a browser on any machine, instead of a callback to localhost.
	DeviceAuth bool
}

// Headless reports whether a browser login cannot reach this machine: an
// SSH session, or Linux with no display. The browser flow redirects to a
// server on localhost:1455, which a browser elsewhere cannot reach.
func Headless(getenv func(string) string, goos string) bool {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return true
	}
	if goos == "darwin" || goos == "windows" {
		return false
	}
	return getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == ""
}

// DefaultDeviceAuth is Headless for the current process.
func DefaultDeviceAuth() bool { return Headless(os.Getenv, runtime.GOOS) }

// OverlaySpec describes an account home layered over the real ~/.codex.
func OverlaySpec(home string) overlay.Spec {
	return overlay.Spec{
		Real:    paths.RealCodexHome(),
		Overlay: home,
		// The app-server daemon runs under one account's auth and Codex
		// refuses a symlinked state directory, so each account keeps its own.
		Exclude:      []string{"auth.json", "app-server-daemon", "app-server-control"},
		SkipPrefixes: []string{".DS_Store", ".aiq-"},
		LockDir:      paths.LocksDir(),
	}
}

// AuthPath returns the credential file inside a home.
func AuthPath(home string) string { return filepath.Join(home, "auth.json") }

// HasCredential reports whether the home holds an auth.json.
func HasCredential(home string) bool {
	_, err := os.Stat(AuthPath(home))
	return err == nil
}

// ImportAuth copies a credential file into the account home.
func ImportAuth(src, home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := fsutil.CopyFileAtomic(src, AuthPath(home)); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return nil
}

// Env builds the child environment for a launch on the given home.
func (p *Provider) Env(home string, native bool, inheritAuthEnv bool) []string {
	env := os.Environ()
	if !inheritAuthEnv {
		env = proc.SanitizeEnv(env, "OPENAI_API_KEY", "CODEX_API_KEY")
	}
	env = proc.SanitizeEnv(env, "CODEX_HOME")
	if !native {
		env = append(env, "CODEX_HOME="+home)
	} else if real := paths.RealCodexHome(); real != filepath.Join(userHome(), ".codex") {
		env = append(env, "CODEX_HOME="+real)
	}
	return env
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// Login runs `codex login` interactively inside the account home.
func (p *Provider) Login(home string, native bool) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	// The credential must land in auth.json, not an OS keyring: the overlay
	// home is what tells accounts apart.
	args := []string{"-c", `cli_auth_credentials_store="file"`, "login"}
	if p.DeviceAuth {
		args = append(args, "--device-auth")
		fmt.Fprintln(os.Stderr, "Running `codex login --device-auth` — open the link on any device, enter the code, and sign in as the account you are enrolling.\n"+
			"If ChatGPT rejects the code, enable device code authorization in its security settings, or rerun with --browser.")
	} else {
		fmt.Fprintln(os.Stderr, "Running `codex login` — authenticate in the browser as the account you are enrolling.\n"+
			"On a remote or headless machine, rerun with --device-auth.")
	}
	code, err := proc.RunInteractive(p.Command(args, p.Env(home, native, false)))
	if err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("codex login exited with code %d", code)
	}
	if !native && !HasCredential(home) {
		return fmt.Errorf("login finished but %s was not created (is cli_auth_credentials_store set to keyring in config.toml?)", AuthPath(home))
	}
	return nil
}

var passthrough = map[string]bool{
	"login": true, "logout": true, "completion": true,
	"--version": true, "-V": true, "--help": true, "-h": true,
}

// Passthrough reports whether args should bypass routing.
func Passthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return passthrough[args[0]]
}

// IsWorker classifies an invocation: `codex exec` or a non-terminal stdout
// means a disposable worker.
func IsWorker(args []string) bool {
	for _, a := range args {
		if a == "exec" || a == "e" {
			return true
		}
	}
	return !term.IsTerminal(int(os.Stdout.Fd()))
}

var modelRe = regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"`)

// DefaultModel reads `model = "..."` from the real config.toml.
func DefaultModel() string {
	data, err := os.ReadFile(filepath.Join(paths.RealCodexHome(), "config.toml"))
	if err != nil {
		return ""
	}
	if m := modelRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// UnattendedArgs turn off the TUI popups that stop a long session with
// nobody at the keyboard. Near a weekly limit Codex asks, after a turn,
// whether to switch to a cheaper model, with the switch preselected; the
// session sits on that menu until someone answers.
var UnattendedArgs = []string{"-c", "notice.hide_rate_limit_model_nudge=true"}

// rateLimitPrompt matches the model-switch menu as it renders at the bottom
// of the pane. Option 2 is "Keep current model".
var rateLimitPrompt = regexp.MustCompile(`(?m)^\s*Approaching rate limits\s*$[\s\S]*^\s*(?:› )?2\. Keep current model\s*$[\s\S]*^\s*Press enter to confirm or esc to go back\s*$`)

// RateLimitPromptOpen reports whether a captured pane shows the model-switch
// menu. Only the last lines count, so a transcript that quotes the menu does
// not match.
func RateLimitPromptOpen(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return rateLimitPrompt.MatchString(strings.Join(lines, "\n"))
}

// HookArgs renders the -c overrides that wire aiq's hooks into a long
// session, plus the flags that let them run without a trust prompt.
func HookArgs(aiqBin string) []string {
	hook := func(event, name string) string {
		return fmt.Sprintf(`hooks.%s=[{hooks=[{type="command",command=%q,timeout=30}]}]`, event, aiqBin+" codex-hook "+name)
	}
	return []string{
		"--enable", "hooks", "--dangerously-bypass-hook-trust",
		"-c", hook("SessionStart", "sessionstart"),
		"-c", hook("UserPromptSubmit", "userpromptsubmit"),
		"-c", hook("Stop", "stop"),
	}
}
