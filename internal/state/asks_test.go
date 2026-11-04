package state

import (
	"path/filepath"
	"testing"
)

func TestAsksStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddAsk(Ask{Chain: "c", Workspace: "w", RaisedAt: 100, FirstQuote: "please merge #12", AsksReader: .9, Refs: []string{"#12"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RepeatAsk(id, "merge #12 now", []string{"#12", "abc1234"}, 200); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAsk(id)
	if err != nil {
		t.Fatal(err)
	}
	if a.FirstQuote != "please merge #12" || a.LastQuote != "merge #12 now" || a.Repeats != 1 || len(a.Refs) != 2 || a.State != AskOpen {
		t.Fatalf("%+v", a)
	}
	if err := st.SetAskState(id, AskDismissed, "person", "", 300); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.ListAsks("c", AskOpen); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	if all, _ := st.ListAsks(""); len(all) != 1 {
		t.Fatalf("nothing is ever deleted: %+v", all)
	}
	if seen, _ := st.AskTurnSeen("c", "s", 1, 0); seen {
		t.Fatal("seen too early")
	}
	st.MarkAskTurnSeen("c", "s", 1, 0, 5)
	if seen, _ := st.AskTurnSeen("c", "s", 1, 0); !seen {
		t.Fatal("not recorded")
	}
	st.CountAskCalls("d", 2)
	if n, _ := st.CountAskCalls("d", 3); n != 5 {
		t.Fatalf("calls %d", n)
	}
}
