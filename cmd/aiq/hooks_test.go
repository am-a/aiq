package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// runHook runs one hook event as the CLI would, and returns what it printed.
func runHook(t *testing.T, provider, event, payload string) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(in, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, out
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	cmdHook(provider, []string{event})
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A session held for its wrap-up gets it from its next prompt, and is ready to
// move when that turn ends.
func TestHeldSessionGetsItsWrapUpFromTheNextPrompt(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	t.Setenv("TMUX_PANE", "")
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddLease(state.Lease{AccountID: "codex/acct-a", Mode: state.ModeLong, Hostname: pool.Hostname(), Provider: "codex",
		Workspace: "/home/me/proj", Pane: "%1", SessionID: "0000-aaaa", Drain: state.DrainRequested,
		DrainAt: time.Now().Add(-time.Minute).Unix(), StartedAt: time.Now().Add(-2 * time.Minute).Unix(), PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIQ_LONG", "1")
	t.Setenv("AIQ_LEASE", strconv.FormatInt(id, 10))

	payload := `{"session_id":"0000-aaaa"}`
	out := runHook(t, "codex", "userpromptsubmit", payload)
	if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "handoff note") {
		t.Fatalf("no wrap-up at the prompt: %q", out)
	}
	l, _ := st.GetLease(id)
	if l.Drain != state.DrainDraining || !l.InTurn() {
		t.Fatalf("after the prompt: drain %q, in turn %v", l.Drain, l.InTurn())
	}
	if out := runHook(t, "codex", "stop", payload); out != "" {
		t.Fatalf("stop printed %q after the wrap-up was given", out)
	}
	if l, _ := st.GetLease(id); l.Drain != state.DrainReady || l.InTurn() {
		t.Fatalf("after the turn: drain %q, in turn %v", l.Drain, l.InTurn())
	}
}
