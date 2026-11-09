package longrun

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

// step is one tick for one lease, as Tick runs it.
func (h *holdPool) step(t *testing.T, id int64) state.Lease {
	t.Helper()
	l, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	h.s.waits = map[int64]waiting{}
	h.s.evaluate(l, time.Now())
	h.s.apply(l, time.Now())
	got, _ := h.st.GetLease(id)
	return got
}

// renames records the window names the daemon sets.
func (h *holdPool) renames() *[]string {
	var names []string
	h.s.rename = func(pane, name string) error {
		names = append(names, pane+" "+name)
		return nil
	}
	return &names
}

func (h *holdPool) why(t *testing.T, l state.Lease) string {
	t.Helper()
	whys, err := h.st.LeaseWhys()
	if err != nil {
		t.Fatal(err)
	}
	return whys[state.LeaseRef{ID: l.ID, StartedAt: l.StartedAt}]
}

func TestStanding(t *testing.T) {
	none := &nowhere{lead: "no account to move to; it keeps going on the 4% left.", reasons: []string{"claude/acct-b: 2% left, at or below the 4% floor"}}
	out := &nowhere{lead: "its account is out: disabled.", reasons: []string{"x"}, word: "disabled", login: true}
	base := state.Lease{AccountID: "codex/acct-a", Pane: "%1", Provider: "codex"}
	with := func(edit func(*state.Lease)) state.Lease { l := base; edit(&l); return l }
	for _, c := range []struct {
		name        string
		l           state.Lease
		f           facts
		label, line string
	}{
		{"running", base, facts{found: true}, "codex/acct-a", ""},
		{"no pane", with(func(l *state.Lease) { l.Drain, l.Pane = state.DrainRequested, "" }), facts{found: true}, "", "no tmux pane recorded; it cannot be moved"},
		{"no launcher", with(func(l *state.Lease) { l.Drain = state.DrainWaiting }), facts{wait: waiting{key: waitNoLauncher, provider: "claude", launcher: "box"}},
			"codex/acct-a · cannot move", `no launcher for claude in the "box" chain, so it cannot move there`},
		{"respawn failed", with(func(l *state.Lease) { l.Drain = state.DrainWaiting }), facts{wait: waiting{key: waitRespawn}},
			"codex/acct-a · cannot move", "the move could not start in its pane; see the daemon log"},
		{"nowhere", with(func(l *state.Lease) { l.Drain = state.DrainDraining }), facts{none: none},
			"codex/acct-a · no account to move to", none.line()},
		{"out, nowhere", base, facts{blocked: true, none: out},
			"codex/acct-a · disabled, no account to move to: log in (aiq doctor)", out.line()},
		{"out, moving", base, facts{blocked: true, found: true, own: "exhausted until 14:00", word: "exhausted"},
			"codex/acct-a · exhausted, moving", "its account is out: exhausted until 14:00. It moves to another account at the next check"},
		{"silent launcher", with(func(l *state.Lease) { l.Drain, l.Launcher = state.DrainRequested, "box" }), facts{found: true},
			"codex/acct-a · moves after next turn", `no turn seen yet from launcher "box"; it moves after its first reported turn, or at once if its account runs out. If it has had turns, its hooks cannot reach aiq`},
		{"requested", with(func(l *state.Lease) { l.Drain, l.TurnEndedAt = state.DrainRequested, 2 }), facts{found: true},
			"codex/acct-a · moves after next turn", "asks for a handoff note at its next prompt or turn end, then moves to another account"},
		{"draining", with(func(l *state.Lease) { l.Drain = state.DrainDraining }), facts{found: true},
			"codex/acct-a · writing handoff note", "writing its handoff note; moves to another account when this turn ends"},
		{"ready in a turn", with(func(l *state.Lease) { l.Drain, l.TurnStartedAt = state.DrainReady, 5 }), facts{found: true},
			"codex/acct-a · moves when this turn ends", "wrapped up; moves to another account when the current turn ends"},
		{"waiting, idle", with(func(l *state.Lease) { l.Drain = state.DrainWaiting }), facts{found: true},
			"codex/acct-a · moving", "wrapped up; moves to another account at the next check"},
		{"literal id", with(func(l *state.Lease) { l.AccountID = "codex/a#[x];" }), facts{found: true}, "codex/a_[x]_", ""},
	} {
		label, line := standing(c.l, c.f)
		if label != c.label || line != c.line {
			t.Errorf("%s: got %q / %q, want %q / %q", c.name, label, line, c.label, c.line)
		}
	}
}

// The reported case: the account is out and nothing can take over. The line
// and the window say so, once, and go when the account comes back.
func TestStandingOfAnAccountThatIsOut(t *testing.T) {
	h := newHoldPool(t, false, false)
	names := h.renames()
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.st.SetEnabled("codex/acct-a", false)
	for i := 0; i < 3; i++ {
		h.step(t, l.ID)
	}
	want := "its account is out: disabled. No other account can take it. It stays here; aiq long stop . ends it.\ncodex: no other account\nclaude: no other account"
	if got := h.why(t, l); got != want {
		t.Fatalf("why = %q, want %q", got, want)
	}
	if len(*names) != 1 || (*names)[0] != "%1 codex/acct-a · disabled, no account to move to" {
		t.Fatalf("renames = %q", *names)
	}
	h.st.SetEnabled("codex/acct-a", true)
	h.step(t, l.ID)
	if got := h.why(t, l); got != "" {
		t.Fatalf("why = %q after the account came back", got)
	}
	if len(*names) != 2 || (*names)[1] != "%1 codex/acct-a" {
		t.Fatalf("renames = %q", *names)
	}
}

// A lease the tick moved, or whose id another lease took meanwhile, is not
// shown: its window is the successor's now.
func TestStandingSkipsAMovedLease(t *testing.T) {
	h := newHoldPool(t, true, false)
	names := h.renames()
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	readopted := int64(0)
	h.s.move = func(l state.Lease, _ state.Account, _ time.Time) {
		h.st.ReleaseLease(l.ID)
		retry := l
		retry.ID, retry.PID = 0, l.PID+1
		readopted, _ = h.st.AddLease(retry)
	}
	h.step(t, l.ID)
	if readopted != l.ID {
		t.Fatalf("the re-adopted lease is %d, want the id %d reused", readopted, l.ID)
	}
	if len(*names) != 0 || h.why(t, l) != "" {
		t.Fatalf("renames = %q, why = %q; want neither", *names, h.why(t, l))
	}
}

// After a restart the daemon renames only what is not running normally.
func TestStandingAfterARestartRenamesNothingRunning(t *testing.T) {
	h := newHoldPool(t, true, false)
	names := h.renames()
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.step(t, l.ID)
	if len(*names) != 0 {
		t.Fatalf("renames = %q", *names)
	}
}

// A failed rename is not retried until the name changes.
func TestStandingRenameFailureIsNotRetried(t *testing.T) {
	h := newHoldPool(t, false, false)
	tries := 0
	h.s.rename = func(string, string) error { tries++; return fmt.Errorf("no server") }
	l := h.lease(t, nil)
	for i := 0; i < 3; i++ {
		h.step(t, l.ID)
	}
	if tries != 1 {
		t.Fatalf("tries = %d, want 1", tries)
	}
}

// The waiting event carries the reasons and is said once while they change;
// the line follows them.
func TestNoSuccessorEventCarriesReasonsOnce(t *testing.T) {
	h := newHoldPool(t, true, false)
	h.renames()
	setUsed(t, h.st, "codex/acct-c", 97)
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	h.step(t, l.ID)
	first := h.why(t, l)
	setUsed(t, h.st, "codex/acct-c", 98)
	h.step(t, l.ID)
	if n := h.events(t, "no account to move to; it keeps going on the 4% left. codex/acct-c: 3% left, at or below the 4% floor"); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	if h.events(t, "acct-c: 2% left") != 0 {
		t.Fatal("a change of reasons logged again")
	}
	if second := h.why(t, l); first == second || !strings.Contains(second, "codex/acct-c: 2% left") {
		t.Fatalf("why %q then %q", first, second)
	}
}

// Login problems name the command, never an identity, and come first; a
// login is advised only where it would free an account.
func TestReasonsForLogins(t *testing.T) {
	h := newHoldPool(t, false, false)
	add := func(id string, used float64) {
		provider, name, _ := strings.Cut(id, "/")
		if err := h.st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true, Identity: "a@example.com"}); err != nil {
			t.Fatal(err)
		}
		setUsed(t, h.st, id, used)
	}
	add("codex/acct-d", 10)
	add("codex/acct-e", 10)
	h.st.SetCLILogin("codex/acct-e", true, "b@example.com", time.Now())
	h.st.SetCLILogin("codex/acct-d", false, "", time.Now())
	h.st.MarkExhausted("codex/acct-d", "test", time.Now().Add(time.Hour), time.Now())
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	n := h.s.nowhere(l, time.Now())
	all := n.line() + n.event()
	if strings.Contains(all, "@") {
		t.Fatalf("an identity reached the text: %q", all)
	}
	if !strings.HasPrefix(n.reasons[0], "codex/acct-d: CLI not logged in — run: aiq account login codex/acct-d") ||
		!strings.HasPrefix(n.reasons[1], "codex/acct-e: CLI logged in as another identity — run: aiq account login codex/acct-e") {
		t.Fatalf("reasons = %q", n.reasons)
	}
	if !n.login {
		t.Fatal("acct-e frees with a login, but login is not advised")
	}
	h.st.MarkExhausted("codex/acct-e", "test", time.Now().Add(time.Hour), time.Now())
	if h.s.nowhere(l, time.Now()).login {
		t.Fatal("login advised for accounts that are also exhausted")
	}
}

// No account in the pool is still a reason, and a long list is cut.
func TestReasonsAreNeverEmptyAndAreCapped(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, nil)
	if got := h.s.nowhere(l, time.Now()).reasons; strings.Join(got, "|") != "codex: no other account|claude: no other account" {
		t.Fatalf("reasons = %q", got)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("codex/acct-%02d", i)
		h.st.AddAccount(state.Account{ID: id, Provider: "codex", Name: id[6:], Enabled: true, Native: true})
		setUsed(t, h.st, id, 99)
	}
	got := h.s.nowhere(l, time.Now()).reasons
	if len(got) != maxReasons+1 || got[0] != "codex/acct-00: 1% left, at or below the 4% floor" || got[maxReasons] != "and 3 more" {
		t.Fatalf("reasons = %q", got)
	}
}

// Showing a standing never changes a decision.
func TestStandingDoesNotChangeMoves(t *testing.T) {
	run := func(apply bool) (drains []string, moves []string) {
		h := newHoldPool(t, false, true)
		h.renames()
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-1111", 1, 2 })
		for i := 0; i < 3; i++ {
			cur, _ := h.st.GetLease(l.ID)
			h.s.evaluate(cur, time.Now())
			if apply {
				h.s.apply(cur, time.Now())
			}
			got, _ := h.st.GetLease(l.ID)
			drains = append(drains, got.Drain)
			if i == 1 {
				h.st.SetLeaseDrain(l.ID, state.DrainReady, time.Now())
			}
		}
		return drains, h.moves
	}
	d1, m1 := run(false)
	d2, m2 := run(true)
	if strings.Join(d1, ",") != strings.Join(d2, ",") || strings.Join(m1, ",") != strings.Join(m2, ",") {
		t.Fatalf("without: %v %v; with: %v %v", d1, m1, d2, m2)
	}
}

// The line is the daemon's alone: a drain change keeps it, and a new lease
// starts without one.
func TestWhyStaysWithItsLease(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, nil)
	if err := h.st.SetLeaseWhy(l.ID, l.StartedAt, l.PID+1, "other pid"); err != nil {
		t.Fatal(err)
	}
	if h.why(t, l) != "" {
		t.Fatal("written for another pid")
	}
	h.st.SetLeaseWhy(l.ID, l.StartedAt, l.PID, "a\x1b[31m\nb")
	h.st.SetLeaseDrain(l.ID, state.DrainReady, time.Now())
	h.st.MarkTurn(l.ID, true, time.Now())
	if got := h.why(t, l); got != "a?[31m\nb" {
		t.Fatalf("why = %q", got)
	}
	cur, _ := h.st.GetLease(l.ID)
	cur.ID = 0
	id, _ := h.st.AddLease(cur)
	if h.why(t, state.Lease{ID: id, StartedAt: cur.StartedAt}) != "" {
		t.Fatal("a new lease carried the line")
	}
	v, err := (&pool.Pool{Cfg: h.s.Pool.Cfg, St: h.st}).View(0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"why":"a?[31m\nb"`) {
		t.Fatalf("the view has no why: %s", b)
	}
}

// Every phrase comes from the account's fields. A reason text the pool set,
// or a provider's window label, never reaches it.
func TestOutPhraseUsesFieldsOnly(t *testing.T) {
	now := time.Now()
	soon := now.Add(time.Hour).Unix()
	ok := selector.Candidate{ID: "codex/acct-b", Enabled: true, HasCredential: true}
	with := func(edit func(*selector.Candidate)) selector.Candidate { c := ok; edit(&c); return c }
	for _, c := range []struct {
		c            selector.Candidate
		phrase, word string
		login        bool
	}{
		{with(func(c *selector.Candidate) { c.Enabled = false }), "disabled", "disabled", false},
		{with(func(c *selector.Candidate) { c.HasCredential = false }), "no credential — run: aiq account login codex/acct-b", "logged out", true},
		{with(func(c *selector.Candidate) {
			c.UnavailableReason, c.Unavailable = "CLI logged in as b@example.com, not a@example.com", selector.UnavailableLoginMismatch
		}), "CLI logged in as another identity — run: aiq account login codex/acct-b", "logged out", true},
		{with(func(c *selector.Candidate) { c.Unavailable = selector.UnavailableAuthRequired }),
			"not logged in (authentication required) — run: aiq account login codex/acct-b", "logged out", true},
		{with(func(c *selector.Candidate) { c.UnavailableReason = "a@example.com" }), "unavailable", "unavailable", false},
		{with(func(c *selector.Candidate) { c.CooldownUntil = soon }), "exhausted until " + resetAt(soon, now), "exhausted", false},
		{with(func(c *selector.Candidate) {
			c.Windows = []state.Window{{Label: "<b>\x1b[31m", Kind: state.KindWeekly, UsedPct: 100, ResetsAt: soon}}
		}), "weekly window exhausted until " + resetAt(soon, now), "exhausted", false},
	} {
		phrase, word, login := outPhrase(c.c, "", now)
		if phrase != c.phrase || word != c.word || login != c.login {
			t.Errorf("got %q %q %v, want %q %q %v", phrase, word, login, c.phrase, c.word, c.login)
		}
	}
}

// An account that could serve says which pass of the search left it out.
func TestPassedOver(t *testing.T) {
	now := time.Now()
	c := selector.Candidate{Windows: []state.Window{{Kind: state.KindWeekly, UsedPct: 98, ResetsAt: now.Add(time.Hour).Unix()}}}
	spent := selector.Candidate{Windows: []state.Window{{Kind: state.KindWeekly, UsedPct: 100}}}
	for _, c := range []struct {
		c       selector.Candidate
		rung    int
		blocked bool
		want    string
	}{
		{c, selector.RungPlan, false, "2% left, at or below the 4% floor"},
		{spent, selector.RungPlan, true, "no quota left"},
		{c, selector.RungExtra, false, "extra usage; this session is already on extra usage"},
		{c, selector.RungLastResort + 1, false, "last resort; used only once codex/acct-a runs out"},
		{spent, selector.RungLastResort + 1, true, "no quota left"},
	} {
		if got := passedOver(c.c, selector.Ranked{Eligible: true, Rung: c.rung}, "", now, c.blocked, "codex/acct-a", 4); got != c.want {
			t.Errorf("rung %d blocked %v: %q, want %q", c.rung, c.blocked, got, c.want)
		}
	}
}

// A provider the search skips for the session's tier is named as such.
func TestReasonsNameATierSkip(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.Args, l.Fallback = "--model gpt-6-astra", "codex,agy" })
	if got := strings.Join(h.s.nowhere(l, time.Now()).reasons, "|"); got != "codex: no other account|agy: no model of this session's tier" {
		t.Fatalf("reasons = %q", got)
	}
}

// Tick shows each lease's standing after its decision, so the tick that asks
// for a drain already says so.
func TestTickShowsTheStandingAfterTheDecision(t *testing.T) {
	h := newHoldPool(t, true, false)
	h.renames()
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.s.Tick()
	if got := h.why(t, l); got != "asks for a handoff note at its next prompt or turn end, then moves to another account" {
		t.Fatalf("why = %q", got)
	}
}
