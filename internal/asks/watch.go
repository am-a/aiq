package asks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/redact"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

const (
	// DefaultBackfill is how many turns of a session first seen are read.
	DefaultBackfill = 20
	// MaxCallsPerDay is the cap on model calls, and MaxBackfillCallsPerDay the part of it
	// older turns may use, so new turns always have room. Days are UTC.
	MaxCallsPerDay         = 5000
	MaxBackfillCallsPerDay = 3000
	// maxTranscript is the largest file a pass parses: parsing holds about the file's size in memory.
	maxTranscript = 2 << 30
	// quiet is how long an unfinished transcript must go unwritten before its last reply counts as final.
	quiet = 90 * time.Second
	// stuckTurn is how long a session reporting a running turn may go unwritten before the
	// turn is read anyway, because a turn can end with no hook.
	stuckTurn = 10 * time.Minute
	// maxTries is how many times one entry is tried before it is skipped and counted.
	maxTries = 5
	// reportEvery is how often a running pass writes the status row.
	reportEvery = 15 * time.Second
	// maxSkipsInRow stops a pass, because many entries rejected in a row means the request is wrong, not the entries.
	maxSkipsInRow = 3
)

// Watcher scores the finished parts of this machine's long sessions.
type Watcher struct {
	Asks     *state.Store // the registry
	Leases   *state.Store // aiq's leases, read only
	Scorer   Scorer
	Budget   *CappedModel // told which budget each entry spends; may be nil
	Hostname string
	KeyOK    func() bool // says the key is still there, checked before each entry
	Now      func() time.Time
	Logf     func(string, ...any)
	// stamps holds each transcript's size and mtime at its last clean read.
	stamps map[string]stamp
	// reported is when a running pass last wrote the status row.
	reported time.Time
}

type stamp struct {
	size  int64
	mtime time.Time
}

// Result is how a pass ended.
type Result struct {
	Err        error
	RetryAfter time.Duration
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// work is a session in a pass.
type work struct {
	row     state.AskSession
	live    bool
	inTurn  bool
	turns   []transcript.Turn
	session *transcript.Session
	mod     time.Time
	size    int64
	failed  bool
	waiting bool
}

// Pass reads every session and scores what is waiting, one entry per session per round so
// no session starves another. It reports through the status row and returns how it ended.
func (w *Watcher) Pass(ctx context.Context) Result {
	if w.stamps == nil {
		w.stamps = map[string]stamp{}
	}
	st := state.AsksStatus{PassAt: w.now().Unix()}
	if prev, err := w.Asks.GetAsksStatus(w.now()); err == nil {
		st.OkAt, st.ErrorAt, st.LastError = prev.OkAt, prev.ErrorAt, prev.LastError
	}
	// A first pass can run for many minutes, so report at its start and while it runs, or a
	// working watcher reads as no watcher.
	if err := w.Asks.SetAsksStatus(st); err != nil {
		return Result{Err: fmt.Errorf("asks: writing status: %w", err)}
	}
	res, works := w.pass(ctx, &st)
	st.Waiting = waiting(w, works)
	st.PassAt = w.now().Unix()
	for _, wk := range works {
		if wk.waiting {
			st.Capped = true // older turns are held until the day rolls over
		}
	}
	if res.Err == nil {
		st.OkAt, st.ErrorAt, st.LastError = st.PassAt, 0, ""
	} else if classOf(res.Err) == ClassCap {
		st.Capped = true
		res = Result{}
	} else {
		if st.ErrorAt == 0 || st.LastError != res.Err.Error() {
			st.ErrorAt = st.PassAt
		}
		st.LastError = res.Err.Error()
	}
	if res.RetryAfter > 0 {
		st.NextTryAt = w.now().Add(res.RetryAfter).Unix()
	}
	if err := w.Asks.SetAsksStatus(st); err != nil && res.Err == nil {
		res.Err = fmt.Errorf("asks: writing status: %w", err)
	}
	return res
}

func waiting(w *Watcher, works []*work) int {
	n := 0
	for _, wk := range works {
		n += len(w.pending(wk))
	}
	return n
}

// heartbeat reports a running pass, at most every reportEvery.
func (w *Watcher) heartbeat(st *state.AsksStatus, works []*work) {
	if st == nil || w.now().Sub(w.reported) < reportEvery {
		return
	}
	w.reported = w.now()
	st.PassAt, st.Waiting = w.now().Unix(), waiting(w, works)
	w.Asks.SetAsksStatus(*st)
}

func (w *Watcher) pass(ctx context.Context, st *state.AsksStatus) (Result, []*work) {
	leases, err := w.Leases.ListLeases()
	if err != nil {
		return Result{Err: errors.New("cannot read aiq's sessions")}, nil
	}
	off, err := w.Asks.AskWorkspacesOff()
	if err != nil {
		return Result{Err: errors.New("cannot read the registry")}, nil
	}
	live := map[string]state.Lease{}
	for _, l := range leases {
		if l.Mode != state.ModeLong || l.Hostname != w.Hostname {
			continue
		}
		name := l.Workspace
		if name == "" {
			name = l.Cwd
		}
		switch {
		case l.Provider != "claude" && l.Provider != "codex" && l.Provider != "agy":
			st.NotWatched = append(st.NotWatched, filepath.Base(name)+": "+l.Provider+" cannot be read yet")
			continue
		case off[name]:
			st.NotWatched = append(st.NotWatched, filepath.Base(name)+": turned off")
		case l.Transcript == "" || l.SessionID == "":
			st.NotWatched = append(st.NotWatched, filepath.Base(name)+": no transcript yet")
			continue
		}
		if _, _, err := w.Asks.UpsertAskSession(state.AskSession{Provider: l.Provider, SessionID: l.SessionID,
			Chain: name, Workspace: name, Transcript: l.Transcript}); err != nil {
			return Result{Err: errors.New("cannot write the registry")}, nil
		}
		live[l.Provider+"\x00"+l.SessionID] = l
	}
	rows, err := w.Asks.ListAskSessions()
	if err != nil {
		return Result{Err: errors.New("cannot read the registry")}, nil
	}
	var works []*work
	for _, row := range rows {
		l, isLive := live[row.Provider+"\x00"+row.SessionID]
		wk, reason := w.load(row, isLive, l.InTurn())
		if reason != "" {
			st.NotWatched = append(st.NotWatched, filepath.Base(row.Workspace)+": "+reason)
		}
		if isLive && reason == "" && !off[row.Workspace] {
			st.Watched++
		}
		if wk == nil {
			continue
		}
		if off[row.Workspace] {
			if err := w.skipToEnd(wk); err != nil {
				return Result{Err: errors.New("cannot write the registry")}, works
			}
			continue
		}
		if err := w.place(wk); err != nil {
			return Result{Err: errors.New("cannot write the registry")}, works
		}
		works = append(works, wk)
	}
	res := w.rounds(ctx, works, st)
	for _, wk := range works {
		if wk.failed || wk.waiting || len(w.pending(wk)) > 0 || w.held(wk) {
			continue
		}
		w.stamps[wk.row.Transcript] = stamp{wk.size, wk.mod}
		if !wk.live && w.now().Sub(wk.mod) >= quiet {
			if err := w.Asks.SetAskSessionDone(wk.row.Provider, wk.row.SessionID); err != nil && res.Err == nil {
				res.Err = errors.New("cannot write the registry")
			}
		}
	}
	return res, works
}

// load reads a session's file if it changed since its last clean read. A nil work with no
// reason means nothing to do this pass.
func (w *Watcher) load(row state.AskSession, live, inTurn bool) (*work, string) {
	info, err := os.Lstat(row.Transcript)
	if err != nil {
		if !live {
			w.Asks.SetAskSessionDone(row.Provider, row.SessionID)
		}
		return nil, ""
	}
	if !info.Mode().IsRegular() {
		return nil, "its transcript is not a regular file"
	}
	if info.Size() > maxTranscript {
		return nil, "its transcript is over 2 GB"
	}
	if inTurn && w.now().Sub(info.ModTime()) < stuckTurn {
		return nil, ""
	}
	if w.stamps[row.Transcript] == (stamp{info.Size(), info.ModTime()}) {
		return nil, ""
	}
	sess, err := parse(row.Provider, row.Transcript)
	if err != nil {
		return nil, "its transcript cannot be read"
	}
	if sess == nil || sess.Worker {
		w.stamps[row.Transcript] = stamp{info.Size(), info.ModTime()}
		return nil, ""
	}
	return &work{row: row, live: live, inTurn: inTurn, session: sess, turns: sess.Turns, mod: info.ModTime(), size: info.Size()}, ""
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

func started(t transcript.Turn) int64 {
	if !t.Started.IsZero() {
		return t.Started.UnixMilli()
	}
	if !t.Ended.IsZero() {
		return t.Ended.UnixMilli()
	}
	return 0
}

// place positions a session first seen at its last turns, and marks the turns already
// written as backfill.
func (w *Watcher) place(wk *work) error {
	if wk.row.Position != (state.AskPosition{}) || wk.row.BackfillUntil != 0 || len(wk.turns) == 0 {
		return nil
	}
	from := 0
	if len(wk.turns) > DefaultBackfill {
		from = len(wk.turns) - DefaultBackfill
	}
	pos := state.AskPosition{TurnStarted: started(wk.turns[from])}
	until := started(wk.turns[len(wk.turns)-1])
	if err := w.Asks.SetAskBackfill(wk.row.Provider, wk.row.SessionID, pos, from, until); err != nil {
		return err
	}
	wk.row.Position, wk.row.BackfillUntil, wk.row.Skipped = pos, until, wk.row.Skipped+from
	return nil
}

// skipToEnd moves a turned-off session past everything written, so nothing from the time it
// was off is sent when it is turned back on.
func (w *Watcher) skipToEnd(wk *work) error {
	if len(wk.turns) == 0 {
		return nil
	}
	last := wk.turns[len(wk.turns)-1]
	to := state.AskPosition{TurnStarted: started(last), ItemsDone: len(entriesOf(wk.row, last)), ReplySHA: sha(last.Reply)}
	if to == wk.row.Position {
		return nil
	}
	err := w.Asks.ApplyAskPlan(wk.row.Provider, wk.row.SessionID, wk.row.Position, to, 0, state.AskPlan{})
	if err == nil {
		wk.row.Position = to
		if wk.row.BackfillUntil == 0 {
			w.Asks.SetAskBackfill(wk.row.Provider, wk.row.SessionID, to, 0, to.TurnStarted)
		}
	}
	return err
}

// held says the newest text may still change.
func (w *Watcher) held(wk *work) bool {
	if len(wk.turns) == 0 {
		return false
	}
	if w.now().Sub(wk.mod) >= quiet {
		return false
	}
	return wk.session.Unfinished || wk.turns[len(wk.turns)-1].Open || wk.row.Provider == "agy"
}

// raw is one scoreable thing in a turn. A nil Entry is a place in the turn with nothing to score.
type raw struct {
	kind EntryKind
	text string
	at   time.Time
	skip bool
	// final says this is t.Reply, kept apart because its text can change in place.
	final bool
}

var pastedBlock = regexp.MustCompile(`(?s)<pasted_content[^>]*>.*?</pasted_content[^>]*>`)

// replyText is what the person's own words are: not a pasted block, a tool's tagged output or
// a slash command.
func replyText(s string) (string, bool) {
	s = strings.TrimSpace(pastedBlock.ReplaceAllString(s, ""))
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "<") {
		return "", false
	}
	return s, true
}

// entriesOf lists a turn's texts by index. The prompt is index 0, and Items follow, because
// Items only grow as a file does. Readers that keep no items use Steers and Earlier.
func entriesOf(row state.AskSession, t transcript.Turn) []raw {
	out := []raw{{kind: EntryReply, text: t.Prompt, at: t.Started, skip: t.Source != transcript.Human}}
	if len(t.Items) > 0 {
		for _, it := range t.Items {
			switch it.Kind {
			case transcript.ItemSteer:
				out = append(out, raw{kind: EntryReply, text: it.Text, at: it.At})
			case transcript.ItemAnswer:
				if it.Provisional {
					return out
				}
				out = append(out, raw{kind: EntryAnswer, text: it.Text, at: it.At})
			default:
				out = append(out, raw{skip: true})
			}
		}
		return out
	}
	for _, s := range t.Steers {
		out = append(out, raw{kind: EntryReply, text: s, at: t.Started})
	}
	for _, s := range t.Earlier {
		out = append(out, raw{kind: EntryAnswer, text: s, at: t.Started})
	}
	return out
}

func sha(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// step is one entry to score and where the session stands after it.
type step struct {
	entry    Entry
	skip     bool
	to       state.AskPosition
	backfill bool
}

// pending lists what is left to score in a session, in order. A turn's reply comes last,
// and only when its text differs from what was scored and no answer item already had it.
func (w *Watcher) pending(wk *work) []step {
	var out []step
	pos := wk.row.Position
	held := w.held(wk)
	for i, t := range wk.turns {
		s := started(t)
		if s == 0 || s < pos.TurnStarted {
			continue
		}
		list := entriesOf(wk.row, t)
		from, replySHA := 0, ""
		if s == pos.TurnStarted {
			from, replySHA = pos.ItemsDone, pos.ReplySHA
		}
		last := i == len(wk.turns)-1
		aiq := t.Source == transcript.Aiq
		for k := from; k < len(list); k++ {
			r := list[k]
			// An answer scored as the turn's reply can become an item later, once its
			// stretch closes. It was already scored.
			if r.kind == EntryAnswer && !r.skip && replySHA != "" && sha(r.text) == replySHA {
				r.skip = true
			}
			if last && held && k == len(list)-1 && r.kind == EntryAnswer && !r.skip {
				return out
			}
			out = append(out, w.stepFor(wk, r, aiq, s, state.AskPosition{TurnStarted: s, ItemsDone: k + 1, ReplySHA: replySHA}))
		}
		if t.Reply != "" && sha(t.Reply) != replySHA && !hasAnswer(list, t.Reply) {
			if last && (held || t.Open) {
				return out
			}
			at := t.Ended
			if at.IsZero() {
				at = t.Started
			}
			out = append(out, w.stepFor(wk, raw{kind: EntryAnswer, text: t.Reply, at: at}, aiq, s,
				state.AskPosition{TurnStarted: s, ItemsDone: len(list), ReplySHA: sha(t.Reply)}))
		}
	}
	return out
}

func hasAnswer(list []raw, text string) bool {
	for _, r := range list {
		if r.kind == EntryAnswer && !r.skip && r.text == text {
			return true
		}
	}
	return false
}

func (w *Watcher) stepFor(wk *work, r raw, aiq bool, turn int64, to state.AskPosition) step {
	st := step{to: to, skip: r.skip, backfill: turn <= wk.row.BackfillUntil}
	if r.skip {
		return st
	}
	text := r.text
	// An error the CLI wrote, such as a lost connection, is not the agent asking anything.
	if r.kind == EntryAnswer && strings.HasPrefix(strings.TrimSpace(text), "API Error:") {
		st.skip = true
		return st
	}
	if r.kind == EntryReply {
		var ok bool
		if text, ok = replyText(text); !ok {
			st.skip = true
			return st
		}
	}
	st.entry = Entry{Meta: Meta{Chain: wk.row.Chain, Workspace: wk.row.Workspace, Provider: wk.row.Provider, SessionID: wk.row.SessionID},
		Kind: r.kind, Text: redact.Clean(text), At: r.at.Unix(), Aiq: aiq && r.kind == EntryAnswer}
	return st
}

// rounds scores one entry per session per round until nothing is left or a failure stops it.
// A session past its backfill goes ahead of one still in it.
func (w *Watcher) rounds(ctx context.Context, works []*work, st *state.AsksStatus) Result {
	skips := 0
	for {
		w.heartbeat(st, works)
		progress := false
		for _, backfill := range []bool{false, true} {
			for _, wk := range works {
				if wk.failed || wk.waiting {
					continue
				}
				if ctx.Err() != nil {
					return Result{}
				}
				list := w.pending(wk)
				if len(list) == 0 || list[0].backfill != backfill {
					continue
				}
				if w.KeyOK != nil && !w.KeyOK() {
					return Result{Err: errors.New("the key file is gone or not private")}
				}
				if w.Budget != nil {
					w.Budget.SetBackfill(backfill)
				}
				err := w.score(ctx, wk, list[0])
				w.heartbeat(st, works)
				if err == nil {
					skips = 0
					progress = true
					continue
				}
				switch classOf(err) {
				case ClassCap:
					return Result{Err: err}
				case ClassBackfill:
					wk.waiting = true
				case ClassMalformed:
					wk.failed = true
					if done, e := w.tryAgainOrSkip(wk, list[0]); e != nil {
						return Result{Err: e}
					} else if done {
						progress = true
					}
				case ClassRejected:
					if e := w.skipEntry(wk, list[0]); e != nil {
						return Result{Err: e}
					}
					progress = true
					skips++
					if skips >= maxSkipsInRow {
						return Result{Err: err, RetryAfter: 15 * time.Minute}
					}
				case ClassAuth:
					return Result{Err: err, RetryAfter: 15 * time.Minute}
				case ClassRate:
					var me *ModelError
					errors.As(err, &me)
					return Result{Err: err, RetryAfter: me.RetryAfter}
				default:
					return Result{Err: err}
				}
			}
		}
		if !progress {
			return Result{}
		}
	}
}

// score runs the rules on one step and applies the result. A panic in the rules counts as a
// failure of this entry and logs only where it happened, since its value could hold quoted text.
func (w *Watcher) score(ctx context.Context, wk *work, st step) (err error) {
	defer func() {
		if r := recover(); r != nil {
			_, file, line, _ := runtime.Caller(2)
			w.logf("asks: panic at %s:%d", filepath.Base(file), line)
			err = &ModelError{Class: ClassMalformed}
		}
	}()
	var plan state.AskPlan
	if !st.skip {
		plan, err = w.Scorer.Score(ctx, st.entry)
		if err != nil {
			return err
		}
	}
	err = w.Asks.ApplyAskPlan(wk.row.Provider, wk.row.SessionID, wk.row.Position, st.to, 0, plan)
	if errors.Is(err, state.ErrAskPositionMoved) {
		row, e := w.Asks.GetAskSession(wk.row.Provider, wk.row.SessionID)
		if e == nil {
			wk.row = row
			return nil
		}
	}
	if err != nil {
		return errors.New("cannot write the registry")
	}
	wk.row.Position = st.to
	return nil
}

func (w *Watcher) skipEntry(wk *work, st step) error {
	err := w.Asks.ApplyAskPlan(wk.row.Provider, wk.row.SessionID, wk.row.Position, st.to, 1, state.AskPlan{})
	if err != nil && !errors.Is(err, state.ErrAskPositionMoved) {
		return errors.New("cannot write the registry")
	}
	wk.row.Position = st.to
	return nil
}

// tryAgainOrSkip counts a failure of an entry, and skips it once it has failed too often.
func (w *Watcher) tryAgainOrSkip(wk *work, st step) (bool, error) {
	n, err := w.Asks.AddAskAttempt(wk.row.Provider, wk.row.SessionID)
	if err != nil {
		return false, errors.New("cannot write the registry")
	}
	if n < maxTries {
		return false, nil
	}
	return true, w.skipEntry(wk, st)
}

// CappedModel counts each call before it is sent, in its own write, so spending is counted
// even when the entry later fails.
type CappedModel struct {
	Inner    Model
	St       *state.Store
	Now      func() time.Time
	Backfill bool
}

// SetBackfill says whether calls now spend the backfill budget too.
func (c *CappedModel) SetBackfill(on bool) { c.Backfill = on }

// Ask counts the call and passes it on.
func (c *CappedModel) Ask(ctx context.Context, st string, q map[string]Question) (map[string]float64, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	day := state.AsksDay(now)
	if c.Backfill {
		ok, err := c.St.TakeAskCall(day+"/backfill", MaxBackfillCallsPerDay)
		if err != nil {
			return nil, errors.New("cannot write the registry")
		}
		if !ok {
			return nil, &ModelError{Class: ClassBackfill}
		}
	}
	ok, err := c.St.TakeAskCall(day, MaxCallsPerDay)
	if err != nil {
		return nil, errors.New("cannot write the registry")
	}
	if !ok {
		return nil, &ModelError{Class: ClassCap}
	}
	return c.Inner.Ask(ctx, st, q)
}
