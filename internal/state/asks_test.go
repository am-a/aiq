package state

import (
	"errors"
	"path/filepath"
	"testing"
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
