package tmux

import (
	"os"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := map[string][]string{
		`aiq run claude --long -- -p`:                  {"aiq", "run", "claude", "--long", "--", "-p"},
		`'a b' 'it'\''s' ''`:                           {"a b", "it's", ""},
		`'{"hooks":{"Stop":[]}}' '$HOME' 'x;y' '*.go'`: {`{"hooks":{"Stop":[]}}`, "$HOME", "x;y", "*.go"},
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

func TestIsDefaultDragEnd(t *testing.T) {
	const table = "copy-mode-vi"
	cases := []struct {
		name, keys string
		want       bool
	}{
		{"default", "bind-key  -T copy-mode-vi MouseDown1Pane    select-pane\nbind-key  -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe-and-cancel", true},
		{"changed", "bind-key -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe", false},
		{"already ours", "bind-key -T copy-mode-vi MouseDragEnd1Pane if-shell -F \"#{@aiq-keep-copy}\" \"send-keys -X copy-pipe-no-clear\" \"send-keys -X copy-pipe-and-cancel\"", false},
		{"unbound", "bind-key -T copy-mode-vi q send-keys -X cancel", false},
		{"other table", "bind-key -T copy-mode MouseDragEnd1Pane send-keys -X copy-pipe-and-cancel", false},
	}
	for _, c := range cases {
		if got := isDefaultDragEnd(c.keys, table); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestKeepPlaceOnCopyOnARealServer(t *testing.T) {
	if !Available() {
		t.Skip("no tmux")
	}
	// Not t.TempDir: its long path does not fit a socket's on macOS.
	dir, err := os.MkdirTemp("", "tx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	pane, err := run("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "keep", "sleep 60")
	if err != nil {
		t.Skipf("cannot start a server here: %v", err)
	}
	defer run("kill-server")
	if err := KeepPlaceOnCopy(pane); err != nil {
		t.Fatal(err)
	}
	keys, err := run("list-keys", "-T", "copy-mode")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keys, "copy-pipe-no-clear") {
		t.Fatalf("not bound:\n%s", keys)
	}
	if got, _ := run("display-message", "-p", "-t", pane, "#{"+keepCopyOption+"}"); got != "1" {
		t.Fatalf("pane option: %q", got)
	}
	// A second convo in the same server finds the binding in place, and a person's own stays.
	if _, err := run("bind-key", "-T", "copy-mode-vi", "MouseDragEnd1Pane", "send-keys", "-X", "copy-selection"); err != nil {
		t.Fatal(err)
	}
	if err := KeepPlaceOnCopy(pane); err != nil {
		t.Fatal(err)
	}
	vi, _ := run("list-keys", "-T", "copy-mode-vi")
	if !strings.Contains(vi, "MouseDragEnd1Pane send-keys -X copy-selection") {
		t.Fatalf("a changed binding was replaced:\n%s", vi)
	}
}
