package asks

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type stubHandler struct {
	got  []string
	fail error
}

func (h *stubHandler) Answer(m Meta, text string) error {
	if h.fail != nil {
		return h.fail
	}
	h.got = append(h.got, "answer:"+text)
	return nil
}

func (h *stubHandler) Reply(m Meta, text string) error {
	if h.fail != nil {
		return h.fail
	}
	h.got = append(h.got, "reply:"+text)
	return nil
}

func session(n int) *transcript.Session {
	s := &transcript.Session{ID: "s", Started: time.Unix(1000, 0)}
	for i := 0; i < n; i++ {
		s.Turns = append(s.Turns, transcript.Turn{Prompt: "p", Source: transcript.Human, Reply: "r", Ended: time.Unix(int64(2000+i), 0)})
	}
	return s
}

func newWatcher(t *testing.T, h Handler) *Watcher {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Watcher{St: st, H: h, Hostname: "h"}
}

func TestATurnIsHandledOnceInOrder(t *testing.T) {
	h := &stubHandler{}
	w := newWatcher(t, h)
	s := session(2)
	s.Turns[1].Steers = []string{"steer"}
	if err := w.session(state.Lease{}, "c", s); err != nil {
		t.Fatal(err)
	}
	want := []string{"reply:p", "answer:r", "reply:p", "reply:steer", "answer:r"}
	if len(h.got) != len(want) {
		t.Fatalf("%v", h.got)
	}
	for i := range want {
		if h.got[i] != want[i] {
			t.Fatalf("%v", h.got)
		}
	}
	h.got = nil
	if err := w.session(state.Lease{}, "c", s); err != nil || len(h.got) != 0 {
		t.Fatalf("handled again: %v %v", h.got, err)
	}
}

func TestOnlyWhatThePersonTypedIsAReply(t *testing.T) {
	h := &stubHandler{}
	w := newWatcher(t, h)
	s := session(1)
	s.Turns[0].Source = transcript.Notice
	if err := w.session(state.Lease{}, "c", s); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 1 || h.got[0] != "answer:r" {
		t.Fatalf("%v", h.got)
	}
}

func TestAFailureLeavesTheTurnForLater(t *testing.T) {
	h := &stubHandler{fail: errors.New("down")}
	w := newWatcher(t, h)
	s := session(1)
	if err := w.session(state.Lease{}, "c", s); err == nil {
		t.Fatal("no error")
	}
	h.fail = nil
	if err := w.session(state.Lease{}, "c", s); err != nil || len(h.got) != 2 {
		t.Fatalf("not retried: %v %v", h.got, err)
	}
}

func TestAnOpenOrUnfinishedTurnWaits(t *testing.T) {
	h := &stubHandler{}
	w := newWatcher(t, h)
	s := session(2)
	s.Unfinished = true
	if err := w.session(state.Lease{}, "c", s); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 2 {
		t.Fatalf("%v", h.got)
	}
	h.got = nil
	s.Unfinished = false
	s.Turns = append(s.Turns, transcript.Turn{Prompt: "open", Open: true})
	if err := w.session(state.Lease{}, "c", s); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 2 {
		t.Fatalf("%v", h.got)
	}
}

func TestANewSessionReadsOnlyItsLastTurns(t *testing.T) {
	h := &stubHandler{}
	w := newWatcher(t, h)
	w.Backfill = 2
	if err := w.session(state.Lease{}, "c", session(10)); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 4 {
		t.Fatalf("%v", h.got)
	}
	h.got = nil
	if err := w.session(state.Lease{}, "c", session(11)); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 2 {
		t.Fatalf("older turns came back: %v", h.got)
	}
}

func TestAPassSkipsOtherHostsAndWorkers(t *testing.T) {
	h := &stubHandler{}
	w := newWatcher(t, h)
	p := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(p, []byte("{}\n"), 0o600)
	if _, err := w.St.AddLease(state.Lease{AccountID: "a", PID: 1, Hostname: "other", Mode: state.ModeLong, Provider: "claude", Transcript: p}); err != nil {
		t.Fatal(err)
	}
	if err := w.Pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 0 {
		t.Fatalf("%v", h.got)
	}
}
