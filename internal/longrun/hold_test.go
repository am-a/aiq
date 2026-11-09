package longrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// holdPool is a Codex account at 4% left, with an optional Codex account and
// an optional Claude account well above the drain floor. Moves are recorded,
// not made, and every pane is alive.
type holdPool struct {
	s     *Supervisor
	st    *state.Store
	moves []string
}

func newHoldPool(t *testing.T, codexTarget, claudeTarget bool) *holdPool {
	t.Helper()
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	add := func(id string, used float64) {
		provider, name, _ := strings.Cut(id, "/")
		if err := st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true}); err != nil {
			t.Fatal(err)
		}
		setUsed(t, st, id, used)
	}
	add("codex/acct-a", 96)
	if codexTarget {
		add("codex/acct-c", 10)
	}
	if claudeTarget {
		add("claude/acct-b", 10)
	}
	cfg := config.Default()
	cfg.Providers.Claude.ModelScope = ""
	h := &holdPool{st: st}
	h.s = &Supervisor{Pool: &pool.Pool{Cfg: cfg, St: st}, Logf: func(string, ...any) {}}
	h.s.paneAlive = func(string) (bool, bool) { return true, true }
	h.s.move = func(l state.Lease, acc state.Account, _ time.Time) { h.moves = append(h.moves, acc.ID) }
	return h
}

// lease adds an idle Codex long lease asked to drain a minute ago.
func (h *holdPool) lease(t *testing.T, edit func(*state.Lease)) state.Lease {
	t.Helper()
	l := state.Lease{AccountID: "codex/acct-a", Mode: state.ModeLong, Hostname: pool.Hostname(), Provider: "codex",
		Workspace: "/home/me/proj", Pane: "%1", Fallback: "codex,claude", Drain: state.DrainRequested,
		DrainAt: time.Now().Add(-time.Minute).Unix(), StartedAt: time.Now().Add(-2 * time.Minute).Unix(), PID: os.Getpid()}
	if edit != nil {
		edit(&l)
	}
	id, err := h.st.AddLease(l)
	if err != nil {
		t.Fatal(err)
	}
	// Turn marks come from the hooks, not from the lease's first write.
	if l.TurnStartedAt > 0 {
		h.st.MarkTurn(id, true, time.Unix(l.TurnStartedAt, 0))
	}
	if l.TurnEndedAt > 0 {
		h.st.MarkTurn(id, false, time.Unix(l.TurnEndedAt, 0))
	}
	got, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (h *holdPool) events(t *testing.T, contains string) int {
	t.Helper()
	evs, err := h.st.ListEvents(500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if strings.Contains(e.Detail, contains) {
			n++
		}
	}
	return n
}

func (h *holdPool) tick(t *testing.T, id int64) state.Lease {
	t.Helper()
	l, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	h.s.evaluate(l, time.Now())
	got, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// codexRollout writes a Codex transcript for session id with one turn, or
// none.
func codexRollout(t *testing.T, id string, turns bool) string {
	t.Helper()
	lines := `{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/home/me/proj"}}` + "\n"
	if turns {
		lines += `{"timestamp":"2026-01-01T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"fix the test"}}` + "\n" +
			`{"timestamp":"2026-01-01T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"fixed"}}` + "\n"
	}
	p := filepath.Join(t.TempDir(), "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecideIdle(t *testing.T) {
	codex := state.Lease{Provider: "codex"}
	manual := state.Lease{Provider: "codex", DrainManual: true}
	copilot := state.Lease{Provider: "copilot"}
	for _, c := range []struct {
		name                       string
		l                          state.Lease
		found, hasContext, carries bool
		want                       idleAction
	}{
		{"fresh successor would lose the conversation", codex, true, true, false, idleHold},
		{"same transcript resumes", codex, true, true, true, idleMove},
		{"nothing to lose", codex, true, false, false, idleMove},
		{"no successor", codex, false, true, false, idleStranded},
		{"the person asked for the move", manual, true, true, false, idleMove},
		{"no hooks to deliver a wrap-up", copilot, true, true, false, idleMove},
	} {
		if got := decideIdle(c.l, c.found, c.hasContext, c.carries); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A resumed session sitting at its prompt on a nearly spent account must not
// be replaced by a fresh session on another provider: it has not been asked
// for a note, and its next prompt would ask it.
func TestIdleResumedSessionIsHeldForItsWrapUp(t *testing.T) {
	h := newHoldPool(t, false, true)
	tr := codexRollout(t, "0000-aaaa", true)
	l := h.lease(t, func(l *state.Lease) { l.SessionID, l.Transcript = "0000-aaaa", tr })
	for i := 0; i < 3; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainRequested {
			t.Fatalf("tick %d: drain = %q, want it still asked", i, got.Drain)
		}
	}
	if len(h.moves) != 0 {
		t.Fatalf("moved to %v, want held", h.moves)
	}
	if n := h.events(t, "no turn reported yet"); n != 1 {
		t.Fatalf("%d hold events, want 1", n)
	}

	// Its next turn brings the wrap-up (the hooks' part), then it moves.
	now := time.Now()
	h.st.MarkTurn(l.ID, true, now)
	h.st.SetLeaseDrain(l.ID, state.DrainDraining, now)
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moved mid-turn to %v", h.moves)
	}
	h.st.MarkTurn(l.ID, false, now)
	h.st.SetLeaseDrain(l.ID, state.DrainReady, now)
	h.tick(t, l.ID)
	if len(h.moves) != 1 || h.moves[0] != "claude/acct-b" {
		t.Fatalf("moves = %v, want one to claude/acct-b after the wrap-up", h.moves)
	}
}

// A session that has had turns is held the same way, and says so differently.
func TestIdleSessionWithTurnsIsHeld(t *testing.T) {
	h := newHoldPool(t, false, true)
	l := h.lease(t, func(l *state.Lease) {
		l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-bbbb", time.Now().Add(-time.Hour).Unix(), time.Now().Add(-50*time.Minute).Unix()
	})
	h.tick(t, l.ID)
	if len(h.moves) != 0 || h.events(t, "once it has written a handoff note") != 1 {
		t.Fatalf("moves = %v; want held with one event", h.moves)
	}
}

func TestIdleMovesWhenNothingIsLost(t *testing.T) {
	t.Run("same transcript resumes", func(t *testing.T) {
		h := newHoldPool(t, true, true)
		tr := codexRollout(t, "0000-cccc", true)
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.Transcript = "0000-cccc", tr })
		h.tick(t, l.ID)
		if len(h.moves) != 1 || h.moves[0] != "codex/acct-c" {
			t.Fatalf("moves = %v, want codex/acct-c", h.moves)
		}
	})
	t.Run("fresh session with nothing in it", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		l := h.lease(t, nil)
		h.tick(t, l.ID)
		if len(h.moves) != 1 {
			t.Fatalf("moves = %v, want one", h.moves)
		}
	})
	t.Run("transcript with no turn", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		tr := codexRollout(t, "0000-dddd", false)
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.Transcript = "0000-dddd", tr })
		h.tick(t, l.ID)
		if len(h.moves) != 1 {
			t.Fatalf("moves = %v, want one", h.moves)
		}
	})
	t.Run("drain the person asked for", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-eeee", 1, 2 })
		if err := h.st.RequestDrainByUser(l.ID, time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		h.tick(t, l.ID)
		if len(h.moves) != 1 {
			t.Fatalf("moves = %v, want one", h.moves)
		}
	})
}

// A resume aiq knows only by id (given in the CLI's own arguments) or whose
// transcript cannot be read counts as having a conversation.
func TestIdleUnknownTranscriptIsHeld(t *testing.T) {
	for name, edit := range map[string]func(*state.Lease){
		"id only": func(l *state.Lease) { l.SessionID = "0000-ffff" },
		"unreadable": func(l *state.Lease) {
			l.SessionID, l.Transcript = "0000-ffff", t.TempDir() // a directory does not parse
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHoldPool(t, false, true)
			l := h.lease(t, edit)
			h.tick(t, l.ID)
			if len(h.moves) != 0 {
				t.Fatalf("moves = %v, want held", h.moves)
			}
		})
	}
}

// With no account to take it, an idle session stays asked: in DrainWaiting
// its next prompt would bring no wrap-up.
func TestIdleWithNoSuccessorStaysAsked(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.TurnStartedAt, l.TurnEndedAt = 1, 2 })
	for i := 0; i < 2; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainRequested || got.DrainAt != l.DrainAt {
			t.Fatalf("tick %d: drain = %q at %d, want requested at %d", i, got.Drain, got.DrainAt, l.DrainAt)
		}
	}
	if n := h.events(t, "no account with headroom"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

// Recording the session id at launch must not let a launched session whose
// hooks never reported be moved: it may be in the middle of a turn.
func TestSilentLauncherIsNotMoved(t *testing.T) {
	h := newHoldPool(t, true, false)
	tr := codexRollout(t, "0000-1111", true)
	l := h.lease(t, func(l *state.Lease) { l.Launcher, l.SessionID, l.Transcript = "safe-codex", "0000-1111", tr })
	h.tick(t, l.ID)
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moves = %v, want none", h.moves)
	}
	if n := h.events(t, "has reported no turn yet"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

// A lease that changed after it was read (a prompt typed meanwhile) is left
// for the next tick.
func TestIdleRereadsTheLease(t *testing.T) {
	h := newHoldPool(t, true, false)
	l := h.lease(t, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-2222", 1, 2 })
	h.st.MarkTurn(l.ID, true, time.Now())
	h.s.evaluate(l, time.Now()) // the stale copy says idle
	if len(h.moves) != 0 {
		t.Fatalf("moves = %v, want none", h.moves)
	}
}

// A blocked account with no successor asks for the wrap-up rather than
// waiting unasked, so a block that lifts with little left holds the session.
func TestBlockedWithNoSuccessorAsksForTheWrapUp(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt, l.TurnStartedAt, l.TurnEndedAt = state.DrainNone, 0, 1, 2 })
	h.st.MarkExhausted("codex/acct-a", "test", time.Now().Add(time.Hour), time.Now())
	if got := h.tick(t, l.ID); got.Drain != state.DrainRequested {
		t.Fatalf("drain = %q, want requested", got.Drain)
	}
	// The block lifts and a Claude account appears: held, not moved fresh.
	if err := h.st.MarkReady("codex/acct-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AddAccount(state.Account{ID: "claude/acct-b", Provider: "claude", Name: "acct-b", Enabled: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	setUsed(t, h.st, "claude/acct-b", 10)
	got, _ := h.st.GetLease(l.ID)
	h.st.SetLeaseDrain(l.ID, state.DrainRequested, time.Unix(got.DrainAt, 0).Add(-time.Minute))
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moves = %v, want held", h.moves)
	}
}

func TestWaitForSuccessorAfterTheWrapUp(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	if got := h.tick(t, l.ID); got.Drain != state.DrainWaiting {
		t.Fatalf("drain = %q, want waiting", got.Drain)
	}
	h.tick(t, l.ID)
	if n := h.events(t, "no account with headroom"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

// What was said about a lease is said again for a new lease that reuses its id.
func TestSayOnceKnowsAReusedID(t *testing.T) {
	s := &Supervisor{}
	l := state.Lease{ID: 7, StartedAt: 100, DrainAt: 200}
	if !s.sayOnce(l, "x") || s.sayOnce(l, "x") {
		t.Fatal("not once")
	}
	if !s.sayOnce(l, "y") {
		t.Fatal("another message was silenced")
	}
	reused := l
	reused.StartedAt = 300
	if !s.sayOnce(reused, "x") {
		t.Fatal("a reused id was silenced")
	}
	s.forget([]state.Lease{reused})
	if !s.sayOnce(l, "x") {
		t.Fatal("a gone lease's record was kept")
	}
}

func TestTranscriptTurnsIsRememberedUntilTheFileChanges(t *testing.T) {
	s := &Supervisor{}
	tr := codexRollout(t, "0000-3333", false)
	l := state.Lease{Provider: "codex", SessionID: "0000-3333", Transcript: tr}
	if got := s.transcriptTurns(l); got != turnsNone {
		t.Fatalf("got %d, want none", got)
	}
	if _, ok := s.turns[tr]; !ok {
		t.Fatal("not remembered")
	}
	full, _ := os.ReadFile(codexRollout(t, "0000-3333", true))
	if err := os.WriteFile(tr, full, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.transcriptTurns(l); got != turnsSome {
		t.Fatalf("got %d after the file grew, want some", got)
	}
	s.forget(nil)
	if len(s.turns) != 0 {
		t.Fatal("a transcript no lease names was kept")
	}
}

func TestCarriesOver(t *testing.T) {
	s := &Supervisor{}
	codexAcc := state.Account{Provider: "codex"}
	claudeAcc := state.Account{Provider: "claude"}
	withTurns := state.Lease{Provider: "codex", SessionID: "0000-4444", Transcript: codexRollout(t, "0000-4444", true)}
	empty := state.Lease{Provider: "codex", SessionID: "0000-5555", Transcript: codexRollout(t, "0000-5555", false)}
	marked := state.Lease{Provider: "codex", SessionID: "0000-6666", TurnStartedAt: 1}
	switch {
	case !s.carriesOver(withTurns, codexAcc):
		t.Error("a transcript with turns does not resume")
	case s.carriesOver(withTurns, claudeAcc):
		t.Error("another provider resumes")
	case s.carriesOver(empty, codexAcc):
		t.Error("a transcript with no turn resumes")
	case !s.carriesOver(marked, codexAcc):
		t.Error("a session with a turn does not resume")
	}
}

func TestResumeTranscript(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) string {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	claudeFile := write("projects/x/1111.jsonl")
	codexFile := write("sessions/2026/01/01/rollout-2026-01-01T00-00-00-2222.jsonl")
	agyFile := write("brain/3333/.system_generated/logs/transcript.jsonl")
	link := filepath.Join(dir, "4444.jsonl")
	os.Symlink(claudeFile, link)
	for _, c := range []struct {
		provider, id, path string
		ok                 bool
	}{
		{"claude", "1111", claudeFile, true},
		{"codex", "2222", codexFile, true},
		{"agy", "3333", agyFile, true},
		{"claude", "9999", claudeFile, false},
		{"codex", "2222", claudeFile, false},
		{"claude", "1111", "projects/x/1111.jsonl", false},
		{"claude", "x", filepath.Join(dir, "projects", "x"), false},
		{"claude", "4444", link, false},
		{"claude", "5555", filepath.Join(dir, "5555.jsonl"), false},
		{"claude", "", claudeFile, false},
		{"copilot", "1111", claudeFile, false},
	} {
		got, err := ResumeTranscript(c.provider, c.id, c.path)
		if (err == nil) != c.ok {
			t.Errorf("%s %s %s: err = %v, want ok=%v", c.provider, c.id, c.path, err, c.ok)
		}
		if c.ok && got != c.path {
			real, _ := filepath.EvalSymlinks(c.path)
			if got != real {
				t.Errorf("%s: recorded %s", c.path, got)
			}
		}
	}
}
