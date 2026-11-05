package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

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

func TestAsksGroupingAndNoPreviewCap(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		a    state.Ask
		want string
	}{
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix() - 48*3600, AsksReader: .6}, "Asks"},
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix() - 48*3600 - 1, AsksReader: .6}, "Other open"},
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .599, Risk: .8}, "Warnings"},
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .6, Risk: .8}, "Asks"},
		{state.Ask{State: state.AskProbablyDone}, "Probably done"},
		{state.Ask{State: state.AskClosed}, "Closed and dismissed"},
		{state.Ask{State: state.AskDismissed}, "Closed and dismissed"},
	} {
		if got := asksGroups([]state.Ask{tc.a}, now); len(got[tc.want]) != 1 {
			t.Errorf("%+v grouped as %v, want %s", tc.a, got, tc.want)
		}
	}
	b := newAsksScreen()
	for i := int64(1); i <= 12; i++ {
		b.snapshot.items = append(b.snapshot.items, state.Ask{ID: i, State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9})
	}
	b.layout(now)
	seen := map[int64]bool{}
	for _, r := range b.rows {
		if r.id != 0 {
			seen[r.id] = true
		}
	}
	if len(seen) != 12 || b.sections["Other open"] || b.sections["Probably done"] || b.sections["Closed and dismissed"] {
		t.Fatalf("visible items %d, section defaults %v", len(seen), b.sections)
	}
}

func TestAsksTokenizerFragmentedAndRejectedMouse(t *testing.T) {
	var p asksTokenizer
	if got := p.feed([]byte("\x1b[<0;7")); len(got) != 0 {
		t.Fatal(got)
	}
	got := p.feed([]byte(";6M\x1b[<0;7;6m\x1b[<32;7;6M\x1b[<64;1;1M\x1b[A"))
	want := []asksInput{{mouse: true, x: 7, y: 6}, {mouse: true, button: 64, x: 1, y: 1}, {key: keypress{code: keyUp}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Legacy payload contains action keys; none may be interpreted as a key.
	if got := p.feed([]byte("\x1b[Mdrq")); len(got) != 0 {
		t.Fatalf("legacy mouse became input: %+v", got)
	}
	if got := p.feed([]byte("\x1b[<x;3;4M\x1b[<0;-1;2M")); len(got) != 0 {
		t.Fatal(got)
	}
	if got := p.feed([]byte{'\xc3'}); len(got) != 0 {
		t.Fatal(got)
	}
	got = p.feed([]byte{'\xa9', 'g', '\t'})
	if len(got) != 3 || got[0].key.r != 'é' || got[1].key.r != 'g' || got[2].key.r != '\t' {
		t.Fatal(got)
	}
	p.feed([]byte("\x1b[" + strings.Repeat("1", 200)))
	if len(p.pending) > 128 {
		t.Fatal("unbounded partial sequence")
	}
	p.feed([]byte("\x1b[<0;"))
	if got := p.feed([]byte{3}); len(got) != 1 || got[0].key.code != keyQuit {
		t.Fatal("partial mouse report swallowed Ctrl-C")
	}
}

func TestAsksRowsActionsAndMovedHeader(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.loaded = 40, 20, true
	now := time.Now()
	b.snapshot.items = []state.Ask{{ID: 12, Workspace: "/example/project", Provider: "claude", State: state.AskOpen,
		RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: strings.Repeat("some words ", 30), LastQuote: "latest quote"}}
	b.snapshot.moved = []state.Ask{{ID: 13, Workspace: "/another/project"}}
	if name := b.workspaceName("/example/project"); name != "example/project" {
		t.Fatalf("moved-item collision hidden: %q", name)
	}
	b.layout(now)
	for _, r := range b.rows {
		if r.action == "dismiss" {
			t.Fatal("action shown on unselected item")
		}
	}
	b.selected = "ask:12"
	b.layout(now)
	quotes := 0
	for _, r := range b.rows {
		if strings.HasPrefix(r.text, "   some") {
			quotes++
		}
	}
	if quotes != 2 {
		t.Fatalf("collapsed quote rows: %d", quotes)
	}
	if _, send, _ := b.action(keypress{r: 'r'}); send {
		t.Fatal("open item allowed restore")
	}
	if req, send, _ := b.action(keypress{r: 'd'}); !send || req.id != 12 || req.to != state.AskDismissed {
		t.Fatalf("dismiss: %+v, %v", req, send)
	}
	b.action(keypress{r: '\t'})
	if b.selected != "section:Warnings" {
		t.Fatal(b.selected)
	}
	b.selected = "moved-header"
	b.action(keypress{code: keyEnter})
	if !b.moved {
		t.Fatal("keyboard cannot open moved items")
	}
	b.message = "Dismissed #12. u undo"
	head := b.header(now)
	if !strings.HasPrefix(head[0].text, "Off:") || !strings.HasPrefix(head[1].text, "> Moved today:") || head[2].text != b.message {
		t.Fatal(head)
	}
}

func TestAsksStateChangesRefreshAndSessions(t *testing.T) {
	reg, leases := asksTestStores(t)
	now := time.Now()
	asksSeed(t, reg, state.Ask{Workspace: "/example/project", Provider: "claude", SessionID: "example-session", Chain: "example",
		RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Please check the example build"})
	first := asksApply(reg, leases, asksRequest{}, now)
	if first.err != nil || len(first.snapshot.items) != 1 {
		t.Fatalf("load: err=%v, items=%d", first.err, len(first.snapshot.items))
	}
	id := first.snapshot.items[0].ID
	result := asksApply(reg, leases, asksRequest{action: "state", id: id, to: state.AskDismissed}, now)
	if result.err != nil || result.message != fmt.Sprintf("Dismissed #%d. u undo", id) || len(result.snapshot.items) != 0 {
		t.Fatalf("dismiss: err=%v, message=%q, items=%d", result.err, result.message, len(result.snapshot.items))
	}
	result = asksApply(reg, leases, asksRequest{action: "undo", id: id}, now)
	if result.err != nil || len(result.snapshot.items) != 1 || result.snapshot.items[0].State != state.AskOpen {
		t.Fatalf("undo: err=%v, items=%d", result.err, len(result.snapshot.items))
	}
	if r := asksApply(reg, leases, asksRequest{action: "state", id: id, to: state.AskOpen}, now); r.err == nil {
		t.Fatal("invalid restore accepted")
	}
	// Another connection can update the registry between UI refreshes.
	other, err := state.Open(filepath.Join(os.Getenv("AIQ_DATA_DIR"), "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.SetAskByPerson(id, state.AskDismissed, now.Unix()); err != nil {
		t.Fatal(err)
	}
	result = asksApply(reg, leases, asksRequest{closed: true}, now)
	if result.err != nil || result.snapshot.items[0].State != state.AskDismissed {
		t.Fatalf("refresh: err=%v", result.err)
	}
	ws := "/example/project"
	result = asksApply(reg, leases, asksRequest{action: "workspace", ws: ws, off: true}, now)
	if result.err != nil || !result.snapshot.off[ws] {
		t.Fatalf("workspace: err=%v, off=%v", result.err, result.snapshot.off[ws])
	}
	b := newAsksScreen()
	b.snapshot, b.sessions = result.snapshot, true
	b.layout(now)
	if len(b.rows) < 2 || b.rows[1].workspace != ws || !strings.Contains(b.rows[1].text, "off") {
		t.Fatal(b.rows)
	}
	result = asksApply(reg, leases, asksRequest{action: "go", ws: ws}, now)
	if result.err != nil || result.goTo == nil || result.goTo.Pane != "%123" {
		t.Fatalf("navigation: err=%v, target found=%v", result.err, result.goTo != nil)
	}
	result = asksApply(reg, leases, asksRequest{action: "go", ws: "tmux; bogus quote"}, now)
	if result.goTo != nil || !strings.Contains(result.message, "ended") {
		t.Fatalf("quote affected navigation: target found=%v, message=%q", result.goTo != nil, result.message)
	}
}

func TestAsksRenderSanitizesAndAnchors(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.noColor, b.loaded = 40, 16, true, true
	now := time.Now()
	for i := int64(1); i <= 8; i++ {
		b.snapshot.items = append(b.snapshot.items, state.Ask{ID: i, State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9,
			Workspace: "/example/\x1b]52;clipboard\a", Provider: "claude\x1b[2J", FirstQuote: "please\rcheck\u202e now", LastQuote: "last\x1b[Hquote"})
	}
	b.selected = "ask:4"
	b.layout(now)
	b.top = 8
	oldOffset := 0
	for i, r := range b.rows {
		if r.key == b.selected {
			oldOffset = i - b.top
			break
		}
	}
	b.snapshot.items = append([]state.Ask{{ID: 99, State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9}}, b.snapshot.items...)
	b.layout(now)
	checked := false
	for i, r := range b.rows {
		if r.key == b.selected && !checked {
			if i-b.top != oldOffset {
				t.Fatal("refresh shifted the selected item")
			}
			checked = true
		}
		for _, c := range r.text {
			if unicode.IsControl(c) || c == '\u202e' {
				t.Fatalf("untrusted control rendered: %q", r.text)
			}
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "screen")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := b.draw(f, now); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.Name())
	if strings.Contains(string(raw), sgrReverse) || strings.Contains(string(raw), sgrBold) {
		t.Fatal("NO_COLOR screen contains SGR styling")
	}
	plain := strings.NewReplacer("\x1b[H", "", "\x1b[K", "", "\x1b[J", "").Replace(string(raw))
	for _, row := range strings.Split(plain, "\r\n") {
		if cells(row) > 40 {
			t.Fatalf("narrow row has %d cells", cells(row))
		}
	}
}

func TestAsksExpandedQuoteCanScroll(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.loaded = 40, 12, true
	b.snapshot.items = []state.Ask{{ID: 1, State: state.AskOpen, RaisedAt: time.Now().Unix(), AsksReader: .9,
		LastQuote: strings.Repeat("read this invented long quote ", 100)}}
	b.selected, b.expanded[1] = "ask:1", true
	b.layout(time.Now())
	b.action(keypress{code: keyPgDn})
	f, err := os.CreateTemp(t.TempDir(), "scrolled")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := b.draw(f, time.Now()); err != nil {
		t.Fatal(err)
	}
	if b.top < 4 || b.selected != "ask:1" {
		t.Fatal("long quote scrolled back to its first line")
	}
	top := b.top
	b.click(asksInput{mouse: true, button: 65}, time.Now())
	if b.top != top+3 || b.selected != "ask:1" {
		t.Fatal("mouse wheel changed the selected item instead of scrolling")
	}
}

// Run this opt-in helper under a disposable pseudo-terminal to exercise the real
// input/refresh/restore loop. It seeds invented rows and never starts a watcher.
func TestAsksTUIPreviewHelper(t *testing.T) {
	if os.Getenv("AIQ_ASKS_TUI_HELPER") != "1" {
		t.Skip("opt-in terminal preview helper")
	}
	reg, leases := asksTestStores(t)
	now := time.Now()
	asksSeed(t, reg,
		state.Ask{Chain: "example", Workspace: "/example/project", Provider: "claude", SessionID: "example-session", RaisedAt: now.Unix(), AsksReader: .9,
			FirstQuote: "Please check the example build before proceeding. This invented quote wraps in a narrow pane.", LastQuote: "The example build needs your check."},
		state.Ask{Chain: "example", Workspace: "/example/older", Provider: "codex", SessionID: "other-example", State: state.AskClosed, StateReason: "reply", StateText: "The invented reply closed this item", StateAt: now.Unix(), RaisedAt: now.Add(-time.Hour).Unix(), FirstQuote: "Is the example ready?"})
	if _, err := runAsksScreen(reg, leases, os.Stdin, os.Stdout, os.Getenv("AIQ_ASKS_TEST_NO_MOUSE") != "1"); err != nil {
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

func TestAsksFilterKeyAndHiddenCount(t *testing.T) {
	b := newAsksScreen()
	now := time.Now()
	b.loaded = true
	b.here, b.filtered = "/w/alpha", true
	b.snapshot.items = []state.Ask{
		{ID: 1, State: state.AskOpen, Workspace: "/w/alpha", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "mine"},
		{ID: 2, State: state.AskOpen, Workspace: "/w/beta", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "theirs"},
	}
	text := func() string {
		b.layout(now)
		out := ""
		for _, r := range b.rows {
			out += r.text + "\n"
		}
		for _, h := range b.header(now) {
			out += h.text + "\n"
		}
		return out
	}
	got := text()
	if !strings.Contains(got, "mine") || strings.Contains(got, "theirs") {
		t.Fatalf("filtered:\n%s", got)
	}
	if !strings.Contains(got, "1 open elsewhere") {
		t.Fatalf("hidden asks must be counted:\n%s", got)
	}
	b.action(keypress{r: 'f'})
	got = text()
	if !strings.Contains(got, "mine") || !strings.Contains(got, "theirs") || strings.Contains(got, "elsewhere") {
		t.Fatalf("all:\n%s", got)
	}
}

func TestAsksResetNeedsConfirmation(t *testing.T) {
	b := newAsksScreen()
	b.here, b.filtered = "/w/alpha", true
	if _, send, _ := b.action(keypress{r: 'X'}); send || !b.confirmReset {
		t.Fatal("X must only ask")
	}
	if _, send, _ := b.action(keypress{r: 'n'}); send || b.confirmReset {
		t.Fatal("another key must keep")
	}
	b.action(keypress{r: 'X'})
	req, send, _ := b.action(keypress{r: 'y'})
	if !send || req.action != "reset" || req.ws != "/w/alpha" {
		t.Fatalf("%+v %v", req, send)
	}
	b.filtered = false
	b.action(keypress{r: 'X'})
	if req, _, _ := b.action(keypress{r: 'y'}); req.ws != "" {
		t.Fatalf("with the filter off a reset covers everything: %+v", req)
	}
}
