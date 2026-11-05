package asks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/redact"
	"github.com/orlenko/aiq/internal/state"
)

const (
	maxTextBytes  = 8 << 10
	maxQuoteBytes = 4 << 10
	maxParagraphs = 20
)

// Engine plans registry changes without writing them. The watcher commits the
// complete plan and its transcript position together only after Score succeeds.
type Engine struct {
	St    *state.Store
	Model Model
	Now   func() time.Time
}

func (e *Engine) Score(ctx context.Context, entry Entry) (state.AskPlan, error) {
	entry.Text = cleanLimit(entry.Text, maxTextBytes)
	if strings.TrimSpace(entry.Text) == "" {
		return state.AskPlan{}, nil
	}
	entry.Chain = redact.Clean(entry.Chain)
	if strings.TrimSpace(entry.Chain) == "" || e.St == nil {
		return state.AskPlan{}, errors.New("asks: no registry chain")
	}
	if entry.At == 0 {
		entry.At = time.Now().Unix()
		if e.Now != nil {
			entry.At = e.Now().Unix()
		}
	}
	switch entry.Kind {
	case EntryAnswer:
		return e.answer(ctx, entry)
	case EntryReply:
		return e.reply(ctx, entry)
	}
	return state.AskPlan{}, errors.New("asks: unknown entry kind")
}

func (e *Engine) ask(ctx context.Context, text string, names ...string) (map[string]float64, error) {
	if e.Model == nil || ctx.Err() != nil {
		return nil, &ModelError{Class: ClassServer}
	}
	questions := make(map[string]Question, len(names))
	for _, name := range names {
		questions[name] = ruleQuestions[name]
	}
	out, err := e.Model.Ask(ctx, cleanLimit(text, maxTextBytes), questions)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, &ModelError{Class: ClassServer}
	}
	for _, name := range names {
		p, ok := out[name]
		if !ok || !probability(p) {
			return nil, &ModelError{Class: ClassMalformed}
		}
	}
	return out, nil
}

// maxLaterCandidates is how many open items one later message is checked against.
const maxLaterCandidates = 12

// Later moves open items a later message of the agent's shows are finished, to probably-done
// and never further: only the person's reply closes. Only items sharing a reference with the
// message are checked. An item the person restored stays put, since they said it is not done.
func (e *Engine) Later(ctx context.Context, entry Entry, except map[int64]bool) ([]state.AskMove, error) {
	entry.Text = cleanLimit(entry.Text, maxTextBytes)
	refs := References(entry.Text)
	if len(refs) == 0 {
		return nil, nil
	}
	items, err := e.St.ListAsks(entry.Chain, state.AskOpen)
	if err != nil {
		return nil, errors.New("asks: cannot read the registry")
	}
	var moves []state.AskMove
	checked := 0
	for _, a := range items {
		if checked == maxLaterCandidates {
			break
		}
		if except[a.ID] || a.RaisedAt >= entry.At || a.StateReason == state.AskPerson || !sharesReference(a.Refs, refs) {
			continue
		}
		checked++
		p, err := e.ask(ctx, fmt.Sprintf("ITEM:\n%s\n\nLATER:\n%s", a.FirstQuote, entry.Text), "finished")
		if err != nil {
			return nil, err
		}
		if p["finished"] >= .5 {
			moves = append(moves, state.AskMove{ID: a.ID, From: a.State, FromAt: a.StateAt, To: state.AskProbablyDone, At: entry.At,
				Reason: "later-message", Text: cleanLimit(entry.Text, maxQuoteBytes)})
		}
	}
	return moves, nil
}

// answer applies the detect and merge rules, then the later-message rule. An item this message
// repeats is still being raised, so it is not finished by the same message.
func (e *Engine) answer(ctx context.Context, entry Entry) (state.AskPlan, error) {
	plan, err := e.detect(ctx, entry)
	if err != nil {
		return state.AskPlan{}, err
	}
	except := map[int64]bool{}
	for _, r := range plan.Repeats {
		except[r.ID] = true
	}
	plan.Moves, err = e.Later(ctx, entry, except)
	if err != nil {
		return state.AskPlan{}, err
	}
	return plan, nil
}

func (e *Engine) detect(ctx context.Context, entry Entry) (state.AskPlan, error) {
	detect, err := e.ask(ctx, entry.Text, "asks_reader", "risk_for_reader")
	if err != nil {
		return state.AskPlan{}, err
	}
	if detect["asks_reader"] < .5 && detect["risk_for_reader"] < .8 {
		return state.AskPlan{}, nil
	}
	paragraphs := splitParagraphs(entry.Text)
	type ranked struct {
		text  string
		score float64
	}
	var quotes []ranked
	for _, paragraph := range paragraphs {
		p, err := e.ask(ctx, paragraph, "holds_item")
		if err != nil {
			return state.AskPlan{}, err
		}
		quotes = append(quotes, ranked{paragraph, p["holds_item"]})
	}
	sort.SliceStable(quotes, func(i, j int) bool { return quotes[i].score > quotes[j].score })
	quote := quotes[0].text
	if len(quotes) > 1 && quotes[1].score >= .5 {
		quote += "\n\n" + quotes[1].text
	}
	quote = cleanLimit(quote, maxQuoteBytes)
	refs := References(entry.Text)
	items, err := e.St.ListAsks(entry.Chain)
	if err != nil {
		return state.AskPlan{}, errors.New("asks: cannot read the registry")
	}
	for _, a := range candidates(items, refs, 12, 6, 12) {
		p, err := e.ask(ctx, fmt.Sprintf("A:\n%s\n\nB:\n%s", a.FirstQuote, quote), "same")
		if err != nil {
			return state.AskPlan{}, err
		}
		if p["same"] >= .7 {
			return state.AskPlan{Repeats: []state.AskRepeat{{ID: a.ID, Quote: quote, Refs: refs, Risk: detect["risk_for_reader"], At: entry.At, Aiq: entry.Aiq}}}, nil
		}
	}
	return state.AskPlan{Inserts: []state.Ask{{Chain: entry.Chain, Workspace: redact.Clean(entry.Workspace), Provider: redact.Clean(entry.Provider),
		SessionID: redact.Clean(entry.SessionID), RaisedAt: entry.At, State: state.AskOpen, StateAt: entry.At, FirstQuote: quote, LastQuote: quote,
		AsksReader: detect["asks_reader"], Risk: detect["risk_for_reader"], Refs: refs, LastSeen: entry.At}}}, nil
}

func (e *Engine) reply(ctx context.Context, entry Entry) (state.AskPlan, error) {
	if !MayClose(entry.Text) {
		return state.AskPlan{}, nil
	}
	text, ok := replyText(entry.Text)
	if !ok {
		return state.AskPlan{}, nil
	}
	items, err := e.St.ListAsks(entry.Chain, state.AskOpen, state.AskProbablyDone)
	if err != nil {
		return state.AskPlan{}, errors.New("asks: cannot read the registry")
	}
	limit, extra := 12, 12
	if utf8.RuneCountInString(text) <= 30 {
		limit, extra = 1, 0
	}
	plan := state.AskPlan{}
	for _, a := range candidates(items, References(text), limit, 0, extra) {
		p, err := e.ask(ctx, fmt.Sprintf("ITEM:\n%s\n\nREPLY:\n%s", a.FirstQuote, text), "settles")
		if err != nil {
			return state.AskPlan{}, err
		}
		if p["settles"] >= .5 {
			to := state.AskProbablyDone
			if p["settles"] >= .85 {
				to = state.AskClosed
			}
			plan.Moves = append(plan.Moves, state.AskMove{ID: a.ID, From: a.State, FromAt: a.StateAt, To: to, At: entry.At, Reason: "reply", Text: cleanLimit(text, maxQuoteBytes)})
		}
	}
	return plan, nil
}

// candidates preserves newest-first priority and adds reference matches once.
// probably-done is pending for reply selection, but non-open for answer merging.
func candidates(items []state.Ask, refs []string, openLimit, otherLimit, extraLimit int) []state.Ask {
	var out []state.Ask
	seen := map[int64]bool{}
	for _, a := range items {
		pending := a.State == state.AskOpen || otherLimit == 0 && a.State == state.AskProbablyDone
		if pending && openLimit > 0 {
			out = append(out, a)
			seen[a.ID] = true
			openLimit--
		}
	}
	for _, a := range items {
		if a.State != state.AskOpen && !seen[a.ID] && otherLimit > 0 {
			out = append(out, a)
			seen[a.ID] = true
			otherLimit--
		}
	}
	for _, a := range items {
		if !seen[a.ID] && extraLimit > 0 && sharesReference(a.Refs, refs) {
			out = append(out, a)
			seen[a.ID] = true
			extraLimit--
		}
	}
	return out
}

func sharesReference(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

var blankLine = regexp.MustCompile(`\n[\t ]*\n`)

func splitParagraphs(text string) []string {
	var out []string
	for _, p := range blankLine.Split(text, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
		if len(out) == maxParagraphs {
			break
		}
	}
	return out
}

func cleanLimit(text string, limit int) string {
	text = redact.Clean(text)
	if len(text) <= limit {
		return text
	}
	for !utf8.ValidString(text[:limit]) {
		limit--
	}
	return text[:limit]
}

var referencePattern = regexp.MustCompile(`(?i)#([0-9]+)\b|\bPR\s+([0-9]+)\b|\b([0-9a-f]{7,40})\b`)

// References returns normalized, distinct PR numbers and short commit hashes.
func References(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range referencePattern.FindAllStringSubmatch(redact.Clean(text), -1) {
		ref := ""
		switch {
		case m[1] != "":
			ref = "#" + m[1]
		case m[2] != "":
			ref = "#" + m[2]
		case strings.ContainsAny(m[3], "0123456789"):
			ref = strings.ToLower(m[3][:7])
		}
		if ref != "" && !seen[ref] {
			out = append(out, ref)
			seen[ref] = true
		}
	}
	return out
}

var watcherTag = regexp.MustCompile(`^\[(?:\p{Lu}[\p{L}\p{N}_-]* ){1,3}watcher\]`)

// MayClose is a conservative hint, not an authority boundary. Real text following
// a pasted block remains a reply; slash commands and automatic notices do not.
func MayClose(text string) bool {
	text = strings.TrimSpace(pastedBlock.ReplaceAllString(redact.Clean(text), ""))
	if text == "" || strings.HasPrefix(text, "/") || !strings.Contains(text, "\n") && watcherTag.MatchString(text) {
		return false
	}
	for _, prefix := range []string{"<task-notification", "[Image", "Base directory", "Stop hook"} {
		if strings.HasPrefix(text, prefix) {
			return false
		}
	}
	return true
}
