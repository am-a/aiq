package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func asWindows(t *testing.T) {
	t.Helper()
	old := goos
	goos = "windows"
	t.Cleanup(func() { goos = old })
}

// mainCommands returns every command name main dispatches on, read from main's switch, so a
// command added later cannot slip past the Windows gate untested.
func mainCommands(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if c, ok := n.(*ast.CaseClause); ok {
				for _, e := range c.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, _ := strconv.Unquote(lit.Value)
						names = append(names, s)
					}
				}
			}
			return true
		})
	}
	if len(names) < 20 {
		t.Fatalf("found only %d commands in main: %q", len(names), names)
	}
	return names
}

func TestWindowsGateCoversEveryCommand(t *testing.T) {
	asWindows(t)
	for _, name := range mainCommands(t) {
		quiet, err := windowsGate([]string{name})
		switch {
		case slices.Contains(windowsQuiet, name):
			if !quiet || err != nil {
				t.Errorf("%s: quiet %v, err %v; want quiet", name, quiet, err)
			}
		case name == "resume":
			if err == nil {
				t.Errorf("resume without --print runs on Windows")
			}
		case windowsReadOnly[name] == nil && hasKey(windowsReadOnly, name):
			if quiet || err != nil {
				t.Errorf("%s: quiet %v, err %v; want it to run", name, quiet, err)
			}
		default:
			if quiet || err == nil {
				t.Errorf("%s: quiet %v, err %v; want a refusal", name, quiet, err)
			}
		}
	}
}

func hasKey(m map[string][]string, k string) bool { _, ok := m[k]; return ok }

func TestWindowsGateForms(t *testing.T) {
	asWindows(t)
	cases := []struct {
		args []string
		runs bool
	}{
		{nil, true},
		{[]string{"status"}, true},
		{[]string{"status", "--json"}, true},
		{[]string{"status", "--refresh"}, false},
		{[]string{"account", "list"}, true},
		{[]string{"account"}, false},
		{[]string{"account", "add", "claude"}, false},
		{[]string{"account", "login", "claude/x"}, false},
		{[]string{"launcher", "list"}, true},
		{[]string{"launcher", "add", "x"}, false},
		{[]string{"asks", "list", "--all"}, true},
		{[]string{"asks", "setup"}, false},
		{[]string{"asks", "watch"}, false},
		{[]string{"asks", "service", "status"}, false},
		{[]string{"resume"}, false},
		{[]string{"resume", "--print"}, true},
		{[]string{"resume", "-p", "abc"}, true},
		{[]string{"resume", "abc", "--", "-p"}, false},
		{[]string{"resume", "--", "--print"}, false},
		{[]string{"resume", "--launcher", "-p"}, false},
		{[]string{"resume", "--launcher", "x", "--print"}, true},
		{[]string{"convo"}, true},
		{[]string{"daemon", "status"}, false},
		{[]string{"doctor"}, false},
		{[]string{"some-launcher", "--", "hi"}, false},
	}
	for _, c := range cases {
		quiet, err := windowsGate(c.args)
		if quiet || (err == nil) != c.runs {
			t.Errorf("%q: quiet %v, err %v; want runs=%v", c.args, quiet, err, c.runs)
		}
		if err != nil && !strings.Contains(err.Error(), "not supported on Windows yet") {
			t.Errorf("%q: refusal %q", c.args, err)
		}
	}
}

func TestWindowsGateOffElsewhere(t *testing.T) {
	old := goos
	goos = "linux"
	t.Cleanup(func() { goos = old })
	for _, name := range mainCommands(t) {
		if quiet, err := windowsGate([]string{name, "add"}); quiet || err != nil {
			t.Errorf("%s on linux: quiet %v, err %v", name, quiet, err)
		}
	}
}

// What runs on Windows writes nothing outside aiq's own data and config folders: no
// provider settings, no shims, nothing in the home directory or the working directory, and
// no lease rows are pruned. Each command must succeed, so it gets as far as printing.
func TestWindowsReadOnlyCommandsWriteOnlyAiqFolders(t *testing.T) {
	asWindows(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, data, config, work := filepath.Join(root, "home"), filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "work")
	for _, d := range []string{home, work} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows takes the home from here
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("AIQ_CONFIG", filepath.Join(config, "config.toml"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "TMUX", "TMUX_PANE", "AIQ_THEME"} {
		t.Setenv(v, "")
	}
	t.Chdir(work)
	// Provider settings and a transcript already in the home must come through untouched.
	const id = "5e551011-0000-4000-8000-000000000001"
	c := `"cwd":"` + work + `","sessionId":"` + id + `","timestamp":"2026-09-15T10:00:00Z"`
	planted := map[string]string{
		filepath.Join(home, ".claude", "settings.json"): `{"statusLine":{"type":"command","command":"mine"}}`,
		filepath.Join(home, ".codex", "config.toml"):    "model = \"x\"\n",
		filepath.Join(home, ".claude", "projects", claudeNonAlnum.ReplaceAllString(work, "-"), id+".jsonl"): `{"type":"user",` + c + `,"origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"first"}}` + "\n" +
			`{"type":"assistant",` + c + `,"message":{"id":"m1","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"one"}]}}` + "\n",
	}
	keep := map[string]bool{}
	for p, body := range planted {
		for d := p; d != home; d = filepath.Dir(d) {
			keep[d] = true
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Leases the commands must leave alone: this host's with a dead process, and another
	// host's older than the age at which pruning drops it.
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-state.MaxLeaseAge - time.Hour).Unix()
	for _, l := range []state.Lease{
		{AccountID: "claude/a", PID: 1 << 30, Hostname: pool.Hostname(), Mode: state.ModeInteractive, StartedAt: time.Now().Unix()},
		{AccountID: "claude/b", PID: 1 << 30, Hostname: "elsewhere", Mode: state.ModeInteractive, StartedAt: old},
	} {
		if _, err := st.AddLease(l); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	runs := []struct {
		args []string
		run  func() error
	}{
		{[]string{"status"}, func() error { return cmdStatus(nil) }},
		{[]string{"status", "--json"}, func() error { return cmdStatus([]string{"--json"}) }},
		{[]string{"account", "list"}, func() error { return cmdAccount([]string{"list"}) }},
		{[]string{"launcher", "list"}, func() error { return cmdLauncher([]string{"list"}) }},
		{[]string{"asks", "list", "--all"}, func() error { return cmdAsks([]string{"list", "--all"}) }},
		{[]string{"resume", "--print"}, func() error { return cmdResume([]string{"--print"}) }},
		{[]string{"resume", "-p", "5e551011"}, func() error { return cmdResume([]string{"-p", "5e551011"}) }},
		{[]string{"convo", "5e551011"}, func() error { return cmdConvo([]string{"5e551011"}) }},
	}
	for _, r := range runs {
		if quiet, err := windowsGate(r.args); quiet || err != nil {
			t.Fatalf("%q is not offered on Windows: %v", r.args, err)
		}
		stdout := os.Stdout
		os.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		err := r.run()
		os.Stdout.Close()
		os.Stdout = stdout
		if err != nil {
			t.Errorf("%q: %v", r.args, err)
		}
	}
	for p, body := range planted {
		if got, err := os.ReadFile(p); err != nil || string(got) != body {
			t.Errorf("%s changed: %q %v", p, got, err)
		}
	}
	if st, err = state.Open(paths.StateDB()); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if leases, err := st.ListLeases(); err != nil || len(leases) != 2 {
		t.Errorf("leases after the commands: %d, want 2 (%v)", len(leases), err)
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root || p == home || p == work || keep[p] {
			return err
		}
		if p == data || strings.HasPrefix(p, data+string(filepath.Separator)) ||
			p == config || strings.HasPrefix(p, config+string(filepath.Separator)) {
			return nil
		}
		t.Errorf("written outside aiq's folders: %s", p)
		return nil
	})
}

// The screens refuse on Windows on their own too, before they take the terminal, in case
// something reaches them past the gate.
func TestWindowsScreensRefuse(t *testing.T) {
	asWindows(t)
	if _, err := (&menu{}).run(nil); err != errWindowsScreen {
		t.Errorf("menu: %v", err)
	}
	if _, err := (&browser{}).run(); err != errWindowsScreen {
		t.Errorf("resume browser: %v", err)
	}
	if err := runAsksScreen(nil, nil, nil, nil, false); err != errWindowsScreen {
		t.Errorf("asks screen: %v", err)
	}
}
