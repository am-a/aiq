package tmux

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := map[string][]string{
		`aiq run claude --long -- -p`:                          {"aiq", "run", "claude", "--long", "--", "-p"},
		`'a b' 'it'\''s' ''`:                                   {"a b", "it's", ""},
		`'{"hooks":{"Stop":[]}}' '$HOME' 'x;y' '*.go'`:         {`{"hooks":{"Stop":[]}}`, "$HOME", "x;y", "*.go"},
		`'=ls' a=b --fallback=claude,codex '^x' '~' 'a#b' '雪'`: {"=ls", "a=b", "--fallback=claude,codex", "^x", "~", "a#b", "雪"},
	}
	for want, args := range cases {
		if got := Quote(args); got != want {
			t.Errorf("Quote(%q) = %s, want %s", args, got, want)
		}
	}
}

func TestParsePanes(t *testing.T) {
	out := "%97:3176984:0:aiq-ops-9d58:/x/aiq run auto --long\n" +
		"%80:455204:0:aiq-ops2-3bd1:/x/aiq run codex --nudge 'note: read it' --\n" +
		"%3:77:1:fleet:\n" +
		"garbage\n"
	got := parsePanes(out)
	if len(got) != 3 {
		t.Fatalf("got %d panes: %+v", len(got), got)
	}
	if p := got[1]; p.ID != "%80" || p.PID != 455204 || p.Dead || p.Session != "aiq-ops2-3bd1" ||
		p.StartCommand != "/x/aiq run codex --nudge 'note: read it' --" {
		t.Fatalf("got %+v", p)
	}
	if !got[2].Dead || got[2].Session != "fleet" {
		t.Fatalf("got %+v", got[2])
	}
}

func TestParseClientThemes(t *testing.T) {
	got := parseClientThemes("/dev/ttys021 dark\n/dev/ttys004 \n/dev/pts/3 light\n\n")
	want := []ClientTheme{{"/dev/ttys021", "dark"}, {"/dev/ttys004", ""}, {"/dev/pts/3", "light"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %+v, want %+v", got[i], want[i])
		}
	}
}

// tmux 3.7 exits 0 with empty formats for a missing pane; PaneState must not
// read that as a dead pane that still exists.
func TestPaneStateMissingPane(t *testing.T) {
	if _, err := run("list-sessions"); err != nil {
		t.Skip("no tmux server")
	}
	exists, running, err := PaneState("%999999")
	if err != nil {
		t.Skipf("tmux reported the missing pane as an error: %v", err)
	}
	if exists || running {
		t.Fatalf("PaneState(missing) = exists %v, running %v; want false, false", exists, running)
	}
}

// An empty pane id is refused before tmux runs: tmux reads it as the current
// pane, which from outside tmux belongs to the newest session.
func TestEmptyPaneIsRefused(t *testing.T) {
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran")
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := Respawn("", "/", "true"); !errors.Is(err, errNoPane) {
		t.Errorf("Respawn: %v", err)
	}
	if err := RespawnLong("", "/", "true"); !errors.Is(err, errNoPane) {
		t.Errorf("RespawnLong: %v", err)
	}
	if exists, _, err := PaneState(""); exists || !errors.Is(err, errNoPane) {
		t.Errorf("PaneState: %v %v", exists, err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("tmux ran")
	}
}

// tmux runs the line in its default shell. Each word comes back as it went
// in under sh, bash and zsh, zsh with extended globbing included.
func TestQuoteSurvivesTheShell(t *testing.T) {
	args := []string{
		"=ls", "=nosuchcommandxyz", "a=b", "x=~", "~", "~root", "^x", "a^b", "#x", "a#b", "x~y", "!x", "$HOME", "`id`",
		"*", "?", "[a]", "{a,b}", "a,b", "%1", "@x", "+x", ":x", "-", "--yolo", "a b", "it's", "", "雪", "a\nb", "a\\b", "\"",
	}
	shells := map[string][]string{
		"sh": {"-c"}, "bash": {"-c"}, "zsh": {"-f", "-c"}, "zsh extendedglob": {"-f", "-o", "extendedglob", "-c"},
	}
	ran := 0
	for name, flags := range shells {
		bin, err := exec.LookPath(strings.Fields(name)[0])
		if err != nil {
			continue
		}
		ran++
		line := "printf '%s\\0' " + Quote(args)
		out, err := exec.Command(bin, append(flags, line)...).Output()
		if err != nil {
			t.Errorf("%s: %v running %s", name, err, line)
			continue
		}
		if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !reflect.DeepEqual(got, args) {
			t.Errorf("%s: got %q\nwant %q", name, got, args)
		}
	}
	if ran == 0 {
		t.Skip("no shell found")
	}
}
