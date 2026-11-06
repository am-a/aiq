package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func asksTestStores(t *testing.T) (*state.Store, *state.Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	reg, err := state.Open(filepath.Join(root, "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if err := reg.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(root, "leases.db")
	writable, err := state.Open(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writable.AddLease(state.Lease{AccountID: "claude/example", Provider: "claude", Mode: state.ModeLong,
		Workspace: "/example/project", Cwd: "/example/project", PID: os.Getpid(), Hostname: pool.Hostname(), Pane: "%123", StartedAt: time.Now().Unix()})
	writable.Close()
	if err != nil {
		t.Fatal(err)
	}
	leases, err := state.OpenReadOnly(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leases.Close() })
	return reg, leases
}

func asksSeed(t *testing.T, reg *state.Store, items ...state.Ask) {
	t.Helper()
	s, _, err := reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "example-session", Chain: "example"})
	if err != nil {
		t.Fatal(err)
	}
	to := s.Position
	to.ItemsDone++
	if err := reg.ApplyAskPlan(s.Provider, s.SessionID, s.Position, to, 0, state.AskPlan{Inserts: items}); err != nil {
		t.Fatal(err)
	}
}

func TestAsksInDir(t *testing.T) {
	for _, tc := range []struct {
		ws, dir string
		want    bool
	}{
		{"/w/alpha", "/w/alpha", true},
		{"/w/alpha", "/w/alpha/sub", true},
		{"/w/alpha", "/w", true},
		{"/w/alpha", "/w/alpha2", false},
		{"/w/alpha2", "/w/alpha", false},
		{"", "/w", false},
		{"/w", "", false},
	} {
		if got := asksInDir(tc.ws, tc.dir); got != tc.want {
			t.Errorf("asksInDir(%q, %q) = %v", tc.ws, tc.dir, got)
		}
	}
}

func TestAsksGroupSections(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		a    state.Ask
		want string
	}{
		{state.Ask{RaisedAt: now - 48*3600, AsksReader: .6}, "Asks"},
		{state.Ask{RaisedAt: now - 48*3600 - 1, AsksReader: .6}, "Other open"},
		{state.Ask{RaisedAt: now, AsksReader: .599, Risk: .8}, "Warnings"},
		{state.Ask{RaisedAt: now, AsksReader: .6, Risk: .8}, "Asks"},
		{state.Ask{RaisedAt: now, AsksReader: .2}, "Other open"},
	} {
		if got := asksGroup(tc.a, now); got != tc.want {
			t.Errorf("%+v grouped as %s, want %s", tc.a, got, tc.want)
		}
	}
}

func TestAsksListRender(t *testing.T) {
	now := time.Now()
	st := state.AsksStatus{PassAt: now.Unix(), Watched: 2}
	items := []state.Ask{
		{ID: 1, State: state.AskOpen, Workspace: "/w/alpha", Provider: "claude", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Merge the example?\x1b[31m", Repeats: 1},
		{ID: 2, State: state.AskOpen, Workspace: "/w/beta", Provider: "codex", RaisedAt: now.Unix(), Risk: .9, FirstQuote: "The example key is exposed."},
		{ID: 3, State: state.AskProbablyDone, Workspace: "/w/alpha", Provider: "claude", RaisedAt: now.Unix(), FirstQuote: "Run the example check.", StateReason: "reply"},
		{ID: 4, State: state.AskDismissed, Workspace: "/w/alpha", Provider: "claude", RaisedAt: now.Unix(), FirstQuote: "Old example."},
	}
	out := renderAsksList(items, nil, st, true, false, now)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("escape reached the list: %q", out)
	}
	asksAt, warnAt := strings.Index(out, "\nAsks\n"), strings.Index(out, "\nWarnings\n")
	if asksAt < 0 || warnAt < asksAt || !strings.Contains(out, "asked 2x") {
		t.Fatalf("sections or repeat count missing:\n%s", out)
	}
	if strings.Contains(out, "Run the example check") || !strings.Contains(out, "1 probably done, 1 closed or dismissed") {
		t.Fatalf("settled items should only be counted without --all:\n%s", out)
	}
	all := renderAsksList(items, nil, st, true, true, now)
	if !strings.Contains(all, "\nProbably done\n") || !strings.Contains(all, "(reply)") || !strings.Contains(all, "Old example.") {
		t.Fatalf("--all should show settled items:\n%s", all)
	}
}

func TestAsksStatusLineSaysWhetherToTrustTheList(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		st    state.AsksStatus
		keyOK bool
		want  string
	}{
		{state.AsksStatus{PassAt: now.Unix()}, false, "Off: no key"},
		{state.AsksStatus{}, true, "Not watching."},
		{state.AsksStatus{PassAt: now.Add(-asksStaleAfter - time.Minute).Unix()}, true, "Not watching since"},
		{state.AsksStatus{PassAt: now.Unix(), LastError: "example", ErrorAt: now.Unix()}, true, "Failing since"},
		{state.AsksStatus{PassAt: now.Unix(), Waiting: 3}, true, "Reading: 3 texts waiting"},
		{state.AsksStatus{PassAt: now.Unix(), Watched: 1}, true, "Nothing open. Watching 1"},
	} {
		if got := asksStatusLine(tc.st, tc.keyOK, 0, now); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%+v: %q, want prefix %q", tc.st, got, tc.want)
		}
	}
}

func TestAsksResetKeepsOtherDirectories(t *testing.T) {
	reg, _ := asksTestStores(t)
	now := time.Now()
	asksSeed(t, reg,
		state.Ask{Chain: "example", Workspace: "/w/alpha", Provider: "claude", SessionID: "example-session", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Alpha example?"},
		state.Ask{Chain: "example", Workspace: "/w/beta", Provider: "claude", SessionID: "example-session", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Beta example?"})
	n, err := asksReset(reg, "/w/alpha", now)
	if err != nil || n != 1 {
		t.Fatalf("reset %d, %v", n, err)
	}
	left, err := reg.ListAsks("")
	if err != nil || len(left) != 1 || left[0].Workspace != "/w/beta" {
		t.Fatalf("left %+v, %v", left, err)
	}
	if n, err := asksReset(reg, "", now); err != nil || n != 1 {
		t.Fatalf("reset all %d, %v", n, err)
	}
}
