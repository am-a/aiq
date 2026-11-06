package state

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func newAsksStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertAskSession(AskSession{Provider: "claude", SessionID: "s", Chain: "c", Workspace: "/w", Transcript: "/t"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func addAsk(t *testing.T, s *Store, at int64, quote string) Ask {
	t.Helper()
	row, err := s.GetAskSession("claude", "s")
	if err != nil {
		t.Fatal(err)
	}
	to := row.Position
	to.ItemsDone++
	err = s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Inserts: []Ask{{Chain: "c", RaisedAt: at, FirstQuote: quote}}})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListAsks("c")
	if err != nil {
		t.Fatal(err)
	}
	return items[0]
}

func TestPlanNeedsCurrentPosition(t *testing.T) {
	s := newAsksStore(t)
	addAsk(t, s, 100, "q")
	err := s.ApplyAskPlan("claude", "s", AskPosition{}, AskPosition{ItemsDone: 9}, 0, AskPlan{Inserts: []Ask{{Chain: "c", RaisedAt: 101, FirstQuote: "other"}}})
	if !errors.Is(err, ErrAskPositionMoved) {
		t.Fatalf("err = %v", err)
	}
	items, _ := s.ListAsks("c")
	if len(items) != 1 {
		t.Fatalf("a refused plan wrote %d items", len(items))
	}
}

func TestSameAskInsertsOnce(t *testing.T) {
	s := newAsksStore(t)
	addAsk(t, s, 100, "q")
	addAsk(t, s, 100, "q")
	items, _ := s.ListAsks("c")
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
}

func TestMoveKeepsPersonsWrite(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	if err := s.SetAskByPerson(a.ID, AskDismissed, 150); err != nil {
		t.Fatal(err)
	}
	row, _ := s.GetAskSession("claude", "s")
	to := row.Position
	to.ItemsDone++
	err := s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Moves: []AskMove{{ID: a.ID, From: AskOpen, FromAt: a.StateAt, To: AskClosed, Reason: "reply", Text: "ok", At: 200}}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAsk(a.ID)
	if got.State != AskDismissed {
		t.Fatalf("state = %s, the person's dismiss was overwritten", got.State)
	}
}

func TestOlderTextDoesNotMove(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	s.SetAskByPerson(a.ID, AskDismissed, 150)
	s.SetAskByPerson(a.ID, AskOpen, 300)
	got, _ := s.GetAsk(a.ID)
	row, _ := s.GetAskSession("claude", "s")
	to := row.Position
	to.ItemsDone++
	s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Moves: []AskMove{{ID: a.ID, From: AskOpen, FromAt: got.StateAt, To: AskClosed, Reason: "reply", At: 200}}})
	got, _ = s.GetAsk(a.ID)
	if got.State != AskOpen {
		t.Fatalf("an 11:00 reply closed an item restored at 12:00: %s", got.State)
	}
}

func repeatOn(t *testing.T, s *Store, r AskRepeat) {
	t.Helper()
	row, _ := s.GetAskSession("claude", "s")
	to := row.Position
	to.ItemsDone++
	if err := s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Repeats: []AskRepeat{r}}); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatByState(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(s *Store, id int64)
		aiq  bool
		want AskState
	}{
		{"open stays open", func(*Store, int64) {}, false, AskOpen},
		{"dismissed stays dismissed", func(s *Store, id int64) { s.SetAskByPerson(id, AskDismissed, 150) }, false, AskDismissed},
		{"probably done reopens", func(s *Store, id int64) {
			s.db.Exec(`UPDATE asks SET state='probably-done', state_at=150 WHERE id=?`, id)
		}, false, AskOpen},
		{"closed reopens", func(s *Store, id int64) { s.db.Exec(`UPDATE asks SET state='closed', state_at=150 WHERE id=?`, id) }, false, AskOpen},
		{"aiq text never reopens", func(s *Store, id int64) { s.db.Exec(`UPDATE asks SET state='closed', state_at=150 WHERE id=?`, id) }, true, AskClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAsksStore(t)
			a := addAsk(t, s, 100, "q")
			tc.move(s, a.ID)
			repeatOn(t, s, AskRepeat{ID: a.ID, Quote: "again", Risk: .9, At: 200, Aiq: tc.aiq})
			got, _ := s.GetAsk(a.ID)
			if got.State != tc.want {
				t.Fatalf("state = %s, want %s", got.State, tc.want)
			}
			if !tc.aiq && (got.LastQuote != "again" || got.Risk != .9 || got.Repeats != 1) {
				t.Fatalf("repeat not recorded: %+v", got)
			}
		})
	}
}

func TestOlderRepeatChangesNothing(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	repeatOn(t, s, AskRepeat{ID: a.ID, Quote: "late", At: 200})
	repeatOn(t, s, AskRepeat{ID: a.ID, Quote: "early", At: 150})
	got, _ := s.GetAsk(a.ID)
	if got.LastQuote != "late" || got.LastSeen != 200 {
		t.Fatalf("%+v", got)
	}
}

func TestPersonActionsAndUndo(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	if err := s.SetAskByPerson(a.ID, AskOpen, 110); err == nil {
		t.Fatal("restoring an open item should fail")
	}
	if err := s.SetAskByPerson(a.ID, AskDismissed, 120); err != nil {
		t.Fatal(err)
	}
	st, err := s.UndoAsk(a.ID, 130)
	if err != nil || st != AskOpen {
		t.Fatalf("undo = %s, %v", st, err)
	}
}

func TestRestoreKeepsWhatMovedIt(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	row, _ := s.GetAskSession("claude", "s")
	to := row.Position
	to.ItemsDone++
	s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Moves: []AskMove{{ID: a.ID, From: AskOpen, FromAt: a.StateAt, To: AskClosed, Reason: "reply", Text: "done, thanks", At: 200}}})
	if err := s.SetAskByPerson(a.ID, AskOpen, 300); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAsk(a.ID)
	if got.State != AskOpen || got.StateText != "done, thanks" {
		t.Fatalf("%+v", got)
	}
}

func TestCountAskCall(t *testing.T) {
	s := newAsksStore(t)
	for i := 0; i < 3; i++ {
		if err := s.CountAskCall("d"); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	if err := s.db.QueryRow(`SELECT calls FROM asks_usage WHERE day = 'd'`).Scan(&calls); err != nil || calls != 3 {
		t.Fatalf("%d %v", calls, err)
	}
}

func TestOpenAddsNoAsksTables(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'asks%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("Open made %d asks objects", n)
	}
}

func moveOn(t *testing.T, s *Store, m AskMove) {
	t.Helper()
	row, _ := s.GetAskSession("claude", "s")
	to := row.Position
	to.ItemsDone++
	if err := s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Moves: []AskMove{m}}); err != nil {
		t.Fatal(err)
	}
}

// A reset that writes the same place as the one a plan was read at still stops the plan.
func TestResetStopsAPlanReadBeforeIt(t *testing.T) {
	s := newAsksStore(t)
	if err := s.RestartAskSessions(time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	row, _ := s.GetAskSession("claude", "s")
	if _, err := s.ResetAsks(func(string) bool { return true }, time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetAskSession("claude", "s")
	if after.Position.TurnStarted != row.Position.TurnStarted || after.Position.ItemsDone != row.Position.ItemsDone {
		t.Fatalf("the test needs the same place: %+v %+v", row.Position, after.Position)
	}
	to := row.Position
	to.ItemsDone++
	err := s.ApplyAskPlan("claude", "s", row.Position, to, 0, AskPlan{Inserts: []Ask{{Chain: "c", Workspace: "/w", RaisedAt: 1, FirstQuote: "old"}}})
	if !errors.Is(err, ErrAskPositionMoved) {
		t.Fatalf("err = %v", err)
	}
	if items, _ := s.ListAsks("c"); len(items) != 0 {
		t.Fatalf("a plan from before the reset wrote %d items", len(items))
	}
}

// The agent asking again after a reply keeps the item open, in whichever order they are applied.
func TestReplyOlderThanARepeatDoesNotClose(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	repeatOn(t, s, AskRepeat{ID: a.ID, Quote: "q again", At: 300})
	moveOn(t, s, AskMove{ID: a.ID, From: AskOpen, FromAt: a.StateAt, To: AskClosed, Reason: "reply", At: 200})
	if got, _ := s.GetAsk(a.ID); got.State != AskOpen {
		t.Fatalf("a reply at 200 closed an item asked again at 300: %s", got.State)
	}
}

// An item repeated in a registry from before repeats were dated keeps that repeat: an older
// reply does not close it.
func TestOldRegistryKeepsItsRepeats(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	if _, err := s.db.Exec(`ALTER TABLE asks DROP COLUMN repeat_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE asks SET last_seen = 300, repeats = 1 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	moveOn(t, s, AskMove{ID: a.ID, From: AskOpen, FromAt: a.StateAt, To: AskClosed, Reason: "reply", At: 200})
	if got, _ := s.GetAsk(a.ID); got.State != AskOpen || got.RepeatAt != 300 {
		t.Fatalf("%+v", got)
	}
}

// Text aiq put in a turn does not hold back the agent's own, earlier repeat.
func TestAiqSightingDoesNotHideARepeat(t *testing.T) {
	s := newAsksStore(t)
	a := addAsk(t, s, 100, "q")
	moveOn(t, s, AskMove{ID: a.ID, From: AskOpen, FromAt: a.StateAt, To: AskClosed, Reason: "reply", At: 150})
	repeatOn(t, s, AskRepeat{ID: a.ID, Quote: "takeover list", At: 300, Aiq: true})
	repeatOn(t, s, AskRepeat{ID: a.ID, Quote: "q again", At: 200})
	got, _ := s.GetAsk(a.ID)
	if got.State != AskOpen || got.LastQuote != "q again" || got.LastSeen != 300 || got.RepeatAt != 200 {
		t.Fatalf("%+v", got)
	}
}

// A session first seen after a fence starts no earlier than it; a fence covers the
// directories under it, and an empty one covers every workspace.
func TestFenceHoldsNewSessions(t *testing.T) {
	s := newAsksStore(t)
	at := time.UnixMilli(5_000_500)
	if err := s.FenceAsks("/w/a", at); err != nil {
		t.Fatal(err)
	}
	for ws, want := range map[string]int64{"/w/a": at.UnixMilli(), "/w/a/sub": at.UnixMilli(), "/w/ab": 10, "/w": 10} {
		row, _, err := s.UpsertAskSession(AskSession{Provider: "claude", SessionID: "n" + ws, Chain: ws, Workspace: ws, Transcript: "/t", NotBefore: 10})
		if err != nil || row.NotBefore != want {
			t.Errorf("%s: %d, %v", ws, row.NotBefore, err)
		}
	}
	if err := s.FenceAskWorkspace("/v", at); err != nil {
		t.Fatal(err)
	}
	for ws, want := range map[string]int64{"/v": at.UnixMilli(), "/v/sub": 10} {
		row, _, err := s.UpsertAskSession(AskSession{Provider: "claude", SessionID: "e" + ws, Chain: ws, Workspace: ws, Transcript: "/t", NotBefore: 10})
		if err != nil || row.NotBefore != want {
			t.Errorf("exact %s: %d, %v", ws, row.NotBefore, err)
		}
	}
	if err := s.FenceAsks("", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	row, _, _ := s.UpsertAskSession(AskSession{Provider: "claude", SessionID: "late", Chain: "/x", Workspace: "/x", Transcript: "/t", NotBefore: 10})
	if row.NotBefore != at.Add(time.Second).UnixMilli() {
		t.Fatalf("%d", row.NotBefore)
	}
}

// Turning a workspace on while the watcher records a new session in it, on another
// connection, never leaves that session before the moment it was turned on.
func TestOnRacingANewSessionIsFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asks.db")
	open := func() *Store {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.EnsureAsks(); err != nil {
			t.Fatal(err)
		}
		return st
	}
	a, b := open(), open()
	for i := 0; i < 200; i++ {
		on := time.UnixMilli(int64(10_000 + i))
		done := make(chan error, 1)
		go func() { done <- a.SetAskWorkspaceOff("/w", false, on) }()
		id := fmt.Sprintf("s%d", i)
		if _, _, err := b.UpsertAskSession(AskSession{Provider: "claude", SessionID: id, Chain: "/w", Workspace: "/w", Transcript: "/t", NotBefore: 1}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		row, err := b.GetAskSession("claude", id)
		if err != nil || row.NotBefore < on.UnixMilli() {
			t.Fatalf("round %d: not before %d, turned on at %d (%v)", i, row.NotBefore, on.UnixMilli(), err)
		}
	}
}
