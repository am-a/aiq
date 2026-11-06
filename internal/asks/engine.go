package asks

import (
	"context"
	"errors"
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
		in, fits, _ := pair("ITEM", a.FirstQuote, "LATER", entry.Text)
		if !fits {
			continue
		}
		p, err := e.ask(ctx, in, "finished")
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
	type ranked struct {
		paragraph
		score float64
	}
	var quotes, points []ranked
	for _, p := range splitParagraphs(entry.Text) {
		score, err := e.ask(ctx, p.text, "holds_item")
		if err != nil {
			return state.AskPlan{}, err
		}
		quotes = append(quotes, ranked{p, score["holds_item"]})
		if p.point && score["holds_item"] >= .5 {
			points = append(points, quotes[len(quotes)-1])
		}
	}
	items, err := e.St.ListAsks(entry.Chain)
	if err != nil {
		return state.AskPlan{}, errors.New("asks: cannot read the registry")
	}
	plan := state.AskPlan{}
	matched := map[int64]bool{}
	// A numbered list of separate asks is one item per point, so each can be settled on its
	// own. A list of options for one choice stays one item.
	if len(points) >= 2 {
		sep, err := e.ask(ctx, entry.Text, "separate")
		if err != nil {
			return state.AskPlan{}, err
		}
		if sep["separate"] < .5 {
			points = nil
		}
	}
	if len(points) >= 2 {
		for _, p := range points {
			if err := e.place(ctx, entry, items, cleanLimit(p.text, maxQuoteBytes), References(p.text), detect, matched, &plan); err != nil {
				return state.AskPlan{}, err
			}
		}
		// Asks outside the list, such as one more request before or under it, make one more
		// item. A paragraph that introduces the list, ending in a colon, is part of it.
		var rest []string
		for i, q := range quotes {
			leadIn := i+1 < len(quotes) && quotes[i+1].point && strings.HasSuffix(strings.TrimSpace(q.text), ":")
			if !q.point && !leadIn && q.score >= .5 {
				rest = append(rest, q.text)
			}
		}
		if len(rest) > 0 {
			quote := strings.Join(rest, "\n\n")
			if err := e.place(ctx, entry, items, quoteAll(rest, maxQuoteBytes), References(quote), detect, matched, &plan); err != nil {
				return state.AskPlan{}, err
			}
		}
		return plan, nil
	}
	// Otherwise the item quotes every paragraph that holds one, in the message's order, so
	// no ask is left out; with none, the one most likely to.
	var held []string
	for _, q := range quotes {
		if q.score >= .5 {
			held = append(held, q.text)
		}
	}
	if len(held) == 0 {
		sort.SliceStable(quotes, func(i, j int) bool { return quotes[i].score > quotes[j].score })
		held = []string{quotes[0].text}
	}
	if err := e.place(ctx, entry, items, quoteAll(held, maxQuoteBytes), References(entry.Text), detect, matched, &plan); err != nil {
		return state.AskPlan{}, err
	}
	return plan, nil
}

// place adds quote to the plan as a repeat of an item it asks the same as, or as a new item.
// An item one point repeats is not offered to the next.
func (e *Engine) place(ctx context.Context, entry Entry, items []state.Ask, quote string, refs []string, detect map[string]float64,
	matched map[int64]bool, plan *state.AskPlan) error {
	for _, a := range candidates(items, refs, 12, 6, 12) {
		if matched[a.ID] {
			continue
		}
		in, fits, _ := pair("A", a.FirstQuote, "B", quote)
		if !fits {
			continue
		}
		p, err := e.ask(ctx, in, "same")
		if err != nil {
			return err
		}
		if p["same"] >= .7 {
			matched[a.ID] = true
			plan.Repeats = append(plan.Repeats, state.AskRepeat{ID: a.ID, Quote: quote, Refs: refs, Risk: detect["risk_for_reader"], At: entry.At, Aiq: entry.Aiq})
			return nil
		}
	}
	plan.Inserts = append(plan.Inserts, state.Ask{Chain: entry.Chain, Workspace: redact.Clean(entry.Workspace), Provider: redact.Clean(entry.Provider),
		SessionID: redact.Clean(entry.SessionID), RaisedAt: entry.At, State: state.AskOpen, StateAt: entry.At, FirstQuote: quote, LastQuote: quote,
		AsksReader: detect["asks_reader"], Risk: detect["risk_for_reader"], Refs: refs, LastSeen: entry.At})
	return nil
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
	// Newest is the item the agent raised last, counting repeats.
	sort.SliceStable(items, func(i, j int) bool {
		return max(items[i].RaisedAt, items[i].RepeatAt) > max(items[j].RaisedAt, items[j].RepeatAt)
	})
	// A short reply, such as "Sent", is checked against the newest item and any item it names
	// by reference.
	limit, extra := 12, 12
	if utf8.RuneCountInString(text) <= 30 {
		limit = 1
	}
	// A reply that settles an item moves it. When it settles none, it moves the one item it
	// takes up most, since a short reply such as "Sent" takes up many items on its face.
	plan := state.AskPlan{}
	move := func(a state.Ask, to state.AskState) {
		plan.Moves = append(plan.Moves, state.AskMove{ID: a.ID, From: a.State, FromAt: a.StateAt, To: to, At: entry.At, Reason: "reply", Text: cleanLimit(text, maxQuoteBytes)})
	}
	// Taking up needs a high score: a reply about one thing often scores 0.6 or 0.7 against
	// another item it shares words with.
	var best state.Ask
	bestScore := .85
	for _, a := range candidates(items, References(text), limit, 0, extra) {
		in, fits, whole := pair("ITEM", a.FirstQuote, "REPLY", text)
		if !fits {
			continue
		}
		p, err := e.ask(ctx, in, "settles", "responds")
		if err != nil {
			return state.AskPlan{}, err
		}
		switch {
		// An item seen only in part may ask for more than the reply settles, so it stays pending.
		case p["settles"] >= .85 && whole:
			move(a, state.AskClosed)
		case p["settles"] >= .5:
			move(a, state.AskProbablyDone)
		case p["responds"] >= bestScore && a.State == state.AskOpen:
			best, bestScore = a, p["responds"]
		}
	}
	if best.ID != 0 && len(plan.Moves) == 0 {
		move(best, state.AskProbablyDone)
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

// paragraph is a block of text, or one point of a numbered list.
type paragraph struct {
	text  string
	point bool
}

var numbered = regexp.MustCompile(`^\s*(?:\*\*)?\d{1,2}[.)]\s`)

// splitParagraphs splits text at blank lines, and a numbered list into its lead-in and one
// paragraph per point. A point runs until the next point or blank line, so a list with blank
// lines between its points splits the same as one without.
func splitParagraphs(text string) []paragraph {
	var out []paragraph
	total := 0
	for _, l := range strings.Split(text, "\n") {
		if numbered.MatchString(l) {
			total++
		}
	}
	add := func(p paragraph) bool {
		if p.text = strings.TrimSpace(p.text); p.text != "" {
			out = append(out, p)
		}
		return len(out) < maxParagraphs
	}
	for _, block := range blankLine.Split(text, -1) {
		lines := strings.Split(block, "\n")
		n := 0
		for _, l := range lines {
			if numbered.MatchString(l) {
				n++
			}
		}
		if n == 0 || total < 2 {
			if !add(paragraph{text: block}) {
				return out
			}
			continue
		}
		cur := paragraph{}
		for _, l := range lines {
			if numbered.MatchString(l) {
				if !add(cur) {
					return out
				}
				cur = paragraph{point: true}
			}
			cur.text += l + "\n"
		}
		if !add(cur) {
			return out
		}
	}
	return out
}

// minPairQuote is the least of an item's quote worth comparing against.
const minPairQuote = 512

// pair puts an item's quote and a later text into one input within maxTextBytes. The later
// text is never cut, since its end can hold what is still open; the quote keeps its start,
// and whole says it was not cut. When the later text leaves too little room, fits is false
// and nothing is compared.
func pair(la, quote, lb, text string) (in string, fits, whole bool) {
	quote, text = redact.Clean(quote), redact.Clean(text)
	room := maxTextBytes - len(la+":\n\n\n"+lb+":\n") - len(text)
	if room < minPairQuote {
		return "", false, false
	}
	whole = len(quote) <= room
	if !whole {
		quote = cutBytes(quote, room-len("…")) + "…"
	}
	return la + ":\n" + quote + "\n\n" + lb + ":\n" + text, true, whole
}

// quoteAll joins paragraphs into one quote of at most limit bytes. When they do not fit, each
// keeps its start: short ones whole, long ones cut to an even share and marked with an
// ellipsis, so no paragraph is lost to the one before it.
func quoteAll(parts []string, limit int) string {
	parts = append([]string(nil), parts...)
	for i := range parts {
		parts[i] = redact.Clean(parts[i])
	}
	if joined := strings.Join(parts, "\n\n"); len(joined) <= limit {
		return joined
	}
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(parts[order[a]]) < len(parts[order[b]]) })
	left := limit - 2*(len(parts)-1)
	for k, i := range order {
		share := max(left/(len(order)-k), 0)
		if len(parts[i]) > share {
			parts[i] = cutBytes(parts[i], max(share-len("…"), 0)) + "…"
		}
		left -= len(parts[i])
	}
	return cleanLimit(strings.Join(parts, "\n\n"), limit)
}

// cutBytes cuts text to at most n bytes without splitting a rune.
func cutBytes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.ValidString(text[:n]) {
		n--
	}
	return text[:n]
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

var pastedBlock = regexp.MustCompile(`(?s)<pasted_content[^>]*>.*?</pasted_content[^>]*>`)

// questionReply is how Codex records the person's answer to a question it put to them.
var questionReply = regexp.MustCompile(`(?s)^<send_user_message_question_reply>(.*?)(?:</send_user_message_question_reply>)?$`)

// replyText is what the person's own words are: not a pasted block, a tool's tagged output or
// a slash command. An answer to a question the agent asked is the person's own words.
func replyText(s string) (string, bool) {
	s = strings.TrimSpace(pastedBlock.ReplaceAllString(s, ""))
	if m := questionReply.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "<") {
		return "", false
	}
	return s, true
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
