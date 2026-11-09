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
// provider settings, no shims, nothing in the home directory.
func TestWindowsReadOnlyCommandsWriteOnlyAiqFolders(t *testing.T) {
	asWindows(t)
	root := t.TempDir()
	home, data, config := filepath.Join(root, "home"), filepath.Join(root, "data"), filepath.Join(root, "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("AIQ_CONFIG", filepath.Join(config, "config.toml"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	// Provider settings already in the home must come through untouched.
	planted := map[string]string{
		filepath.Join(home, ".claude", "settings.json"): `{"statusLine":{"type":"command","command":"mine"}}`,
		filepath.Join(home, ".codex", "config.toml"):    "model = \"x\"\n",
	}
	for p, body := range planted {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// In order: status first, so the database exists for what reads it after.
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
		{[]string{"resume", "-p", "abc"}, func() error { return cmdResume([]string{"-p", "abc"}) }},
		{[]string{"convo"}, func() error { return cmdConvo(nil) }},
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
			t.Logf("%q: %v", r.args, err) // an error is fine; a write outside aiq's folders is not
		}
	}
	for p, body := range planted {
		if got, err := os.ReadFile(p); err != nil || string(got) != body {
			t.Errorf("%s changed: %q %v", p, got, err)
		}
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root || p == home {
			return err
		}
		if rel, _ := filepath.Rel(home, p); rel == ".claude" || rel == ".codex" ||
			rel == filepath.Join(".claude", "settings.json") || rel == filepath.Join(".codex", "config.toml") {
			return nil // planted above
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
