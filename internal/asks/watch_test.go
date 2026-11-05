package asks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

type stubScorer struct {
	seen []Entry
	err  func(Entry) error
}

func (s *stubScorer) Score(_ context.Context, e Entry) (state.AskPlan, error) {
	if s.err != nil {
		if err := s.err(e); err != nil {
			return state.AskPlan{}, err
		}
	}
	s.seen = append(s.seen, e)
	if e.Kind == EntryAnswer && strings.Contains(e.Text, "?") {
		return state.AskPlan{Inserts: []state.Ask{{Chain: e.Chain, Workspace: e.Workspace, Provider: e.Provider, SessionID: e.SessionID, RaisedAt: e.At, FirstQuote: e.Text}}}, nil
	}
	return state.AskPlan{}, nil
}

func (s *stubScorer) texts() []string {
	out := []string{}
	for _, e := range s.seen {
		out = append(out, e.Text)
	}
	return out
}

type fixture struct {
	t      *testing.T
	dir    string
	path   string
	reg    *state.Store
	leases *state.Store
	lease  int64
	clock  time.Time
	lines  []string
	sc     *stubScorer
	w      *Watcher
	n      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	reg, err := state.Open(filepath.Join(dir, "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if err := reg.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	leases, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leases.Close() })
	path := filepath.Join(dir, "s.jsonl")
	id, err := leases.AddLease(state.Lease{Mode: state.ModeLong, Hostname: "h", Cwd: "/w", Workspace: "/w", Provider: "claude", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := leases.SetLeaseTranscript(id, path); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir, path: path, reg: reg, leases: leases, lease: id, clock: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC), sc: &stubScorer{}}
	f.w = &Watcher{Asks: reg, Leases: leases, Scorer: f.sc, Hostname: "h", Now: func() time.Time { return f.clock }}
	return f
}

func (f *fixture) ts() string {
	f.n++
	return f.clock.Add(-time.Hour).Add(time.Duration(f.n) * time.Second).Format(time.RFC3339)
}

func (f *fixture) user(text string) {
	f.lines = append(f.lines, fmt.Sprintf(`{"type":"user","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"message":{"role":"user","content":%q}}`, f.ts(), text))
}

func (f *fixture) agent(text string) {
	f.n++
	f.lines = append(f.lines, fmt.Sprintf(`{"type":"assistant","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"message":{"id":"m%d","model":"claude-opus-5","stop_reason":"end_turn","role":"assistant","content":[{"type":"text","text":%q}]}}`, f.ts(), f.n, text))
}

// end closes the turn the way current Claude Code does.
func (f *fixture) end() {
	f.lines = append(f.lines, fmt.Sprintf(`{"type":"system","subtype":"turn_duration","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"durationMs":1}`, f.ts()))
}

// write puts the file on disk, last written at the clock minus age.
func (f *fixture) write(age time.Duration) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(strings.Join(f.lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	at := f.clock.Add(-age)
	if err := os.Chtimes(f.path, at, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) pass() Result {
	f.t.Helper()
	return f.w.Pass(context.Background())
}

func TestAskVisibleBeforeTheReply(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("Your call: A or B?")
	f.write(2 * time.Minute)
	if res := f.pass(); res.Err != nil {
		t.Fatal(res.Err)
	}
	items, _ := f.reg.ListAsks("/w")
	if len(items) != 1 {
		t.Fatalf("items = %d: an ask must show while the agent waits, entries %q", len(items), f.sc.texts())
	}
}

func TestHeldAnswerIsReadOnceQuietWithNoNewWrite(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("Your call: A or B?")
	f.write(10 * time.Second)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 {
		t.Fatalf("a held answer was scored: %d", len(items))
	}
	f.clock = f.clock.Add(2 * time.Minute)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("items = %d: the hold must end with no new write", len(items))
	}
}

func TestNothingIsScoredTwice(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.pass()
	before := len(f.sc.seen)
	f.pass()
	f.w.stamps = nil // as after a restart
	f.pass()
	if len(f.sc.seen) != before {
		t.Fatalf("scored again: %q", f.sc.texts())
	}
	f.user("p1")
	f.agent("a1")
	f.end()
	f.write(time.Hour)
	f.pass()
	if got := f.sc.texts(); len(got) != before+2 || got[before] != "p1" {
		t.Fatalf("new turn: %q", got)
	}
}

func TestGapIsNeverSkippedAfterAnOutage(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.pass()
	f.sc.err = func(Entry) error { return &ModelError{Class: ClassServer, Status: 503} }
	for i := 1; i < 30; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	if res := f.pass(); res.Err == nil {
		t.Fatal("no error reported")
	}
	st, _ := f.reg.GetAsksStatus(f.clock)
	if st.LastError == "" || st.Skipped != 0 || st.Waiting == 0 {
		t.Fatalf("%+v", st)
	}
	f.sc.err = nil
	f.sc.seen = nil
	f.pass()
	if got := f.sc.texts(); len(got) != 58 {
		t.Fatalf("after the outage scored %d", len(got))
	}
}

func TestAuthFailureStopsAndSkipsNothing(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	calls := 0
	f.sc.err = func(Entry) error { calls++; return &ModelError{Class: ClassAuth, Status: 401} }
	res := f.pass()
	if res.Err == nil || res.RetryAfter == 0 || calls != 1 {
		t.Fatalf("res %+v calls %d", res, calls)
	}
	row, _ := f.reg.GetAskSession("claude", "s")
	if row.Skipped != 0 || row.Position.ItemsDone != 0 {
		t.Fatalf("%+v", row)
	}
}

func TestRejectedEntriesStopAfterThreeInARow(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	f.sc.err = func(Entry) error { return &ModelError{Class: ClassRejected, Status: 400} }
	if res := f.pass(); res.Err == nil {
		t.Fatal("no stop")
	}
	row, _ := f.reg.GetAskSession("claude", "s")
	if row.Skipped != 3 {
		t.Fatalf("skipped = %d", row.Skipped)
	}
}

func TestMalformedEntryIsSkippedAfterFiveTries(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	bad := true
	f.sc.err = func(e Entry) error {
		if bad && e.Text == "p0" {
			return &ModelError{Class: ClassMalformed}
		}
		return nil
	}
	for i := 0; i < 5; i++ {
		f.w.stamps = nil
		f.pass()
	}
	row, _ := f.reg.GetAskSession("claude", "s")
	if row.Skipped != 1 {
		t.Fatalf("skipped = %d", row.Skipped)
	}
	f.pass()
	if got := f.sc.texts(); len(got) != 1 || got[0] != "a0" {
		t.Fatalf("%q", got)
	}
}

func TestPanicInRulesDoesNotEscape(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.sc.err = func(Entry) error { panic("quoted text") }
	var logged string
	f.w.Logf = func(format string, args ...any) { logged += fmt.Sprintf(format, args...) }
	f.pass()
	if strings.Contains(logged, "quoted text") {
		t.Fatalf("panic value logged: %s", logged)
	}
}

func TestOffWorkspaceSendsNothingAndSkipsItsGap(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.reg.SetAskWorkspaceOff("/w", true, 1)
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("sent while off: %q", f.sc.texts())
	}
	f.user("p1")
	f.agent("a1")
	f.end()
	f.write(time.Hour)
	f.pass()
	f.reg.SetAskWorkspaceOff("/w", false, 2)
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("text from the off time was sent: %q", f.sc.texts())
	}
	f.user("p2")
	f.agent("a2")
	f.end()
	f.write(time.Hour)
	f.pass()
	if got := f.sc.texts(); len(got) != 2 || got[0] != "p2" {
		t.Fatalf("%q", got)
	}
}

func TestMissingKeySendsNothing(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.w.KeyOK = func() bool { return false }
	res := f.pass()
	if res.Err == nil || len(f.sc.seen) != 0 {
		t.Fatalf("%+v %q", res, f.sc.texts())
	}
}

func TestRepliesLeaveOutPastesToolOutputAndCommands(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/compact", ""},
		{"<pasted_content id=1>env dump</pasted_content>", ""},
		{"<pasted_content id=1>env dump</pasted_content>\nfix it", "fix it"},
		{"<bash-stdout>x</bash-stdout>", ""},
		{"go with A", "go with A"},
	} {
		got, ok := replyText(tc.in)
		if (tc.want != "") != ok || got != tc.want {
			t.Errorf("%q: %q %v", tc.in, got, ok)
		}
	}
}

func TestEndedSessionIsReadToItsEndThenDropped(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.pass()
	if err := f.leases.ReleaseLease(f.lease); err != nil {
		t.Fatal(err)
	}
	f.agent("Your call: A or B?")
	f.end()
	f.write(time.Hour)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("the final answer was lost: %d", len(items))
	}
	rows, _ := f.reg.ListAskSessions()
	if len(rows) != 0 {
		t.Fatalf("still reading %d", len(rows))
	}
}

func TestCapStopsWithoutAnError(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.sc.err = func(Entry) error { return &ModelError{Class: ClassCap} }
	if res := f.pass(); res.Err != nil {
		t.Fatal(res.Err)
	}
	st, _ := f.reg.GetAsksStatus(f.clock)
	if !st.Capped || st.LastError != "" {
		t.Fatalf("%+v", st)
	}
}

func TestErrorTextIsFixed(t *testing.T) {
	for _, e := range []*ModelError{{Class: ClassAuth, Status: 401}, {Class: ClassServer}, {Class: ClassRate}, {Class: ClassMalformed}, {Class: ClassRejected, Status: 413}} {
		if strings.ContainsAny(e.Error(), "\"/") {
			t.Errorf("%q", e.Error())
		}
	}
	if classOf(errors.New("x")) != ClassServer {
		t.Error("an unknown error must count as a service failure")
	}
}

type countModel struct{ calls int }

func (m *countModel) Ask(context.Context, string, map[string]Question) (map[string]float64, error) {
	m.calls++
	return map[string]float64{}, nil
}

func TestCappedModelCountsBeforeSending(t *testing.T) {
	f := newFixture(t)
	inner := &countModel{}
	c := &CappedModel{Inner: inner, St: f.reg, Now: func() time.Time { return f.clock }}
	day := state.AsksDay(f.clock)
	for i := 0; i < MaxCallsPerDay; i++ {
		if ok, _ := f.reg.TakeAskCall(day, MaxCallsPerDay); !ok {
			t.Fatal("cap too early")
		}
	}
	_, err := c.Ask(context.Background(), "x", nil)
	if classOf(err) != ClassCap || inner.calls != 0 {
		t.Fatalf("%v calls %d", err, inner.calls)
	}
}

func TestRunningPassReportsItself(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	var during state.AsksStatus
	f.sc.err = func(Entry) error {
		during, _ = f.reg.GetAsksStatus(f.clock)
		return nil
	}
	f.pass()
	if during.PassAt == 0 {
		t.Fatal("a running pass wrote no status, so it reads as no watcher")
	}
}

func TestCLIErrorIsNotAnAsk(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("API Error: Can't reach the API server, why?")
	f.end()
	f.write(time.Hour)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 {
		t.Fatalf("an API error became an ask: %d", len(items))
	}
}

func TestStartsFromTheWatchersStart(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.user(fmt.Sprintf("old p%d", i))
		f.agent("Old: your call?")
		f.end()
	}
	f.write(time.Hour)
	f.w.Start = f.clock
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("read from before the start: %q", f.sc.texts())
	}
	st, _ := f.reg.GetAsksStatus(f.clock)
	if st.Waiting != 0 || st.Skipped != 0 {
		t.Fatalf("%+v", st)
	}
	f.clock = f.clock.Add(2 * time.Hour)
	f.user("new p")
	f.agent("New: your call?")
	f.end()
	f.write(time.Minute * 5)
	f.pass()
	if got := f.sc.texts(); len(got) != 2 || got[0] != "new p" {
		t.Fatalf("%q", got)
	}
}

func TestResetErasesAndOldTextStaysGone(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Your call: A or B?")
	f.end()
	f.write(time.Hour)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	n, err := f.reg.ResetAsks(func(w string) bool { return w == "/w" }, f.clock)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	f.sc.seen = nil
	f.w.stamps = nil
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 || len(f.sc.seen) != 0 {
		t.Fatalf("old text came back: %d items, %q", len(items), f.sc.texts())
	}
	f.clock = f.clock.Add(2 * time.Hour)
	f.user("p1")
	f.agent("Next: your call?")
	f.end()
	f.write(5 * time.Minute)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("new asks after a reset: %d", len(items))
	}
}

func TestPlanScoredBeforeAResetDoesNotLand(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Your call: A or B?")
	f.end()
	f.write(time.Hour)
	f.sc.err = func(e Entry) error {
		if e.Kind == EntryAnswer {
			f.reg.ResetAsks(func(string) bool { return true }, f.clock)
		}
		return nil
	}
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 {
		t.Fatalf("a plan from before the reset landed: %d", len(items))
	}
}
