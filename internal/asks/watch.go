// Package asks tracks what agents asked of the person, from outside the agents.
// It reads transcripts and writes only its own tables. Nothing in it can start a
// process, type into a pane or launch a provider.
package asks

import (
	"fmt"
	"os"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// Meta says where a text came from.
type Meta struct {
	Chain     string
	Workspace string
	Provider  string
	SessionID string
	At        int64
}

// Handler applies the rules to one text. An error leaves the turn to be tried again.
type Handler interface {
	Answer(m Meta, text string) error
	Reply(m Meta, text string) error
}

// DefaultBackfill is how many finished turns of a session first seen are read. Older
// ones are marked seen, so a long session does not turn into thousands of model calls.
const DefaultBackfill = 20

// maxTranscript is the largest file a pass parses.
const maxTranscript = 64 << 20

// Watcher handles the finished turns of this machine's long sessions.
type Watcher struct {
	St       *state.Store
	H        Handler
	Hostname string
	Backfill int
	Logf     func(string, ...any)
	// stamps holds each transcript's last size and mtime, so a pass parses only
	// what changed. A failed turn clears its entry, so it is tried again.
	stamps map[string]stamp
	big    map[string]bool
}

type stamp struct {
	size  int64
	mtime time.Time
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// Pass looks at each long session once and returns the first error it met. It stops
// at a session's first failure, so turns are handled in order.
func (w *Watcher) Pass() error {
	leases, err := w.St.ListLeases()
	if err != nil {
		return err
	}
	if w.stamps == nil {
		w.stamps = map[string]stamp{}
		w.big = map[string]bool{}
	}
	var first error
	for _, l := range leases {
		if l.Mode != state.ModeLong || l.Hostname != w.Hostname || l.Transcript == "" || l.InTurn() {
			continue
		}
		if err := w.lease(l); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (w *Watcher) lease(l state.Lease) error {
	info, err := os.Stat(l.Transcript)
	if err != nil {
		return nil // not written yet, or moved: the next pass looks again
	}
	if info.Size() > maxTranscript {
		if !w.big[l.Transcript] {
			w.big[l.Transcript] = true
			w.logf("asks: skipping %s, larger than the limit", l.Transcript)
		}
		return nil
	}
	now := stamp{info.Size(), info.ModTime()}
	if w.stamps[l.Transcript] == now {
		return nil
	}
	sess, err := parse(l.Provider, l.Transcript)
	if err != nil {
		return fmt.Errorf("asks: reading a transcript: %w", err)
	}
	if sess == nil || sess.Worker {
		w.stamps[l.Transcript] = now
		return nil
	}
	chain := l.Workspace
	if chain == "" {
		chain = l.Cwd
	}
	if err := w.session(l, chain, sess); err != nil {
		return err
	}
	w.stamps[l.Transcript] = now
	return nil
}

func parse(provider, path string) (*transcript.Session, error) {
	switch provider {
	case "claude":
		return transcript.ParseClaude(path)
	case "codex":
		return transcript.ParseCodex(path)
	case "agy":
		return transcript.ParseAgy(path)
	}
	return nil, nil
}

// finished is the number of leading turns whose answers can no longer change.
func finished(s *transcript.Session) int {
	n := len(s.Turns)
	for n > 0 {
		t := s.Turns[n-1]
		last := n == len(s.Turns)
		if t.Open || (last && s.Unfinished) {
			n--
			continue
		}
		break
	}
	return n
}

func (w *Watcher) session(l state.Lease, chain string, s *transcript.Session) error {
	done := finished(s)
	started := s.Started.Unix()
	backfill := w.Backfill
	if backfill == 0 {
		backfill = DefaultBackfill
	}
	// A session never seen before reads only its last turns.
	any := false
	for i := 0; i < done && !any; i++ {
		seen, err := w.St.AskTurnSeen(chain, s.ID, started, i)
		if err != nil {
			return err
		}
		any = seen
	}
	skipBelow := 0
	if !any && done > backfill {
		skipBelow = done - backfill
	}
	for i := 0; i < done; i++ {
		seen, err := w.St.AskTurnSeen(chain, s.ID, started, i)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if i >= skipBelow {
			if err := w.turn(Meta{Chain: chain, Workspace: l.Workspace, Provider: l.Provider, SessionID: s.ID}, s.Turns[i]); err != nil {
				return err
			}
		}
		if err := w.St.MarkAskTurnSeen(chain, s.ID, started, i, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// turn gives the handler what the person typed, then the agent's final answer. A
// retried turn sends its texts again, which is safe: an item already closed is no
// longer a candidate, and a repeat merges.
func (w *Watcher) turn(m Meta, t transcript.Turn) error {
	m.At = t.Ended.Unix()
	if t.Ended.IsZero() {
		m.At = t.Started.Unix()
	}
	if t.Source == transcript.Human && t.Prompt != "" {
		if err := w.H.Reply(m, t.Prompt); err != nil {
			return err
		}
	}
	for _, steer := range t.Steers {
		if err := w.H.Reply(m, steer); err != nil {
			return err
		}
	}
	if t.Reply != "" {
		return w.H.Answer(m, t.Reply)
	}
	return nil
}
