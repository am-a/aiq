package asks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/state"
)

type modelFunc func(context.Context, string, map[string]Question) (map[string]float64, error)

func (f modelFunc) Ask(ctx context.Context, text string, q map[string]Question) (map[string]float64, error) {
	return f(ctx, text, q)
}

func ruleModel(score func(name, text string) (float64, error)) Model {
	return modelFunc(func(_ context.Context, text string, q map[string]Question) (map[string]float64, error) {
		out := map[string]float64{}
		for name := range q {
			p, err := score(name, text)
			if err != nil {
				return nil, err
			}
			out[name] = p
		}
		return out, nil
	})
}

func ruleStore(t *testing.T) *state.Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	s, err := state.Open(filepath.Join(root, "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertAskSession(state.AskSession{Provider: "codex", SessionID: "fixture-session", Chain: "chain"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func applyTestPlan(t *testing.T, s *state.Store, plan state.AskPlan) {
	t.Helper()
	row, err := s.GetAskSession("codex", "fixture-session")
	if err != nil {
		t.Fatal(err)
	}
	to := row.Position
	to.ItemsDone++
	if err := s.ApplyAskPlan("codex", "fixture-session", row.Position, to, 0, plan); err != nil {
		t.Fatal(err)
	}
}

func seedAsks(t *testing.T, s *state.Store, items ...state.Ask) []state.Ask {
	t.Helper()
	for i := range items {
		if items[i].Chain == "" {
			items[i].Chain = "chain"
		}
		if items[i].RaisedAt == 0 {
			items[i].RaisedAt = int64(100 + i)
		}
		if items[i].FirstQuote == "" {
			items[i].FirstQuote = fmt.Sprintf("decision %d", i)
		}
	}
	applyTestPlan(t, s, state.AskPlan{Inserts: items})
	return askSnapshot(t, s)
}

func askSnapshot(t *testing.T, s *state.Store) []state.Ask {
	t.Helper()
	rows, err := s.ListAsks("")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func answerEntry(text string) Entry {
	return Entry{Meta: Meta{Chain: "chain", Workspace: "fixture-workspace", Provider: "codex", SessionID: "fixture-session"}, Kind: EntryAnswer, Text: text, At: 1000}
}

func TestDetectThresholdsAndQuoteRanking(t *testing.T) {
	for _, tc := range []struct {
		reader, risk, second float64
		want                 bool
		quote                string
	}{
		{.499999, .799999, .5, false, ""}, {.5, 0, .5, true, "best\n\nsecond"}, {0, .8, .499999, true, "best"},
	} {
		t.Run(fmt.Sprint(tc.reader, tc.risk, tc.second), func(t *testing.T) {
			s := ruleStore(t)
			e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
				switch name {
				case "asks_reader":
					return tc.reader, nil
				case "risk_for_reader":
					return tc.risk, nil
				case "holds_item":
					switch text {
					case "best":
						return .9, nil
					case "second":
						return tc.second, nil
					}
					return .1, nil
				}
				t.Fatalf("unexpected question %s", name)
				return 0, nil
			})}
			plan, err := e.Score(context.Background(), answerEntry("low\n\nbest\n \nsecond"))
			if err != nil || (len(plan.Inserts) == 1) != tc.want {
				t.Fatalf("plan %v, error %v", plan, err)
			}
			if tc.want && (plan.Inserts[0].FirstQuote != tc.quote || plan.Inserts[0].Risk != tc.risk || plan.Inserts[0].AsksReader != tc.reader) {
				t.Fatalf("insert %+v", plan.Inserts[0])
			}
			if len(askSnapshot(t, s)) != 0 {
				t.Fatal("Score wrote to the registry")
			}
		})
	}
}

func TestMergeThresholdAndStoreRepeatTransitions(t *testing.T) {
	for _, original := range []state.AskState{state.AskOpen, state.AskProbablyDone, state.AskClosed, state.AskDismissed} {
		for _, aiq := range []bool{false, true} {
			t.Run(fmt.Sprint(original, aiq), func(t *testing.T) {
				s := ruleStore(t)
				before := seedAsks(t, s, state.Ask{State: original, FirstQuote: "original decision #7", LastQuote: "previous repeat", Risk: .9, Refs: []string{"#7"}})[0]
				e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
					switch name {
					case "asks_reader", "holds_item":
						return 1, nil
					case "risk_for_reader":
						return .8, nil
					case "same":
						if !strings.HasPrefix(text, "A:\noriginal decision #7\n") || strings.Contains(text, "previous repeat") {
							t.Error("merge did not use FirstQuote")
						}
						return .7, nil
					}
					t.Fatalf("unexpected question %s", name)
					return 0, nil
				})}
				entry := answerEntry("repeat the decision #8")
				entry.Aiq = aiq
				plan, err := e.Score(context.Background(), entry)
				if err != nil || len(plan.Repeats) != 1 || len(plan.Moves) != 0 || len(plan.Inserts) != 0 {
					t.Fatalf("plan %v, error %v", plan, err)
				}
				r := plan.Repeats[0]
				if r.Aiq != aiq || r.Risk != .8 || r.At != entry.At {
					t.Fatalf("repeat %+v", r)
				}
				if !reflect.DeepEqual(askSnapshot(t, s), []state.Ask{before}) {
					t.Fatal("Score wrote a repeat")
				}
				applyTestPlan(t, s, plan)
				a := askSnapshot(t, s)[0]
				want := original
				if !aiq && (original == state.AskClosed || original == state.AskProbablyDone) {
					want = state.AskOpen
				}
				if a.State != want || a.FirstQuote != before.FirstQuote || a.LastSeen != entry.At || a.Risk != .9 {
					t.Fatalf("repeat result %+v", a)
				}
				if aiq {
					if a.LastQuote != before.LastQuote || a.Repeats != 0 {
						t.Fatal("aiq repeat changed more than last-seen")
					}
				} else if a.LastQuote != entry.Text || a.Repeats != 1 || !reflect.DeepEqual(a.Refs, []string{"#7", "#8"}) {
					t.Fatalf("repeat result %+v", a)
				}
			})
		}
	}
	t.Run("below same threshold", func(t *testing.T) {
		s := ruleStore(t)
		seedAsks(t, s, state.Ask{FirstQuote: "first"})
		e := Engine{St: s, Model: ruleModel(func(name, _ string) (float64, error) {
			if name == "same" {
				return .699999, nil
			}
			return 1, nil
		})}
		plan, err := e.Score(context.Background(), answerEntry("new decision"))
		if err != nil || len(plan.Inserts) != 1 || len(plan.Repeats) != 0 {
			t.Fatalf("plan %v, error %v", plan, err)
		}
	})
}

func TestReplyThresholdsUseReadStateAndNoWrites(t *testing.T) {
	for _, tc := range []struct {
		p    float64
		want state.AskState
	}{{.499999, ""}, {.5, state.AskProbablyDone}, {.849999, state.AskProbablyDone}, {.85, state.AskClosed}} {
		t.Run(fmt.Sprint(tc.p), func(t *testing.T) {
			s := ruleStore(t)
			before := seedAsks(t, s, state.Ask{State: state.AskProbablyDone})
			e := Engine{St: s, Model: ruleModel(func(name, _ string) (float64, error) {
				switch name {
				case "settles":
					return tc.p, nil
				case "responds":
					return 0, nil
				}
				t.Error("wrong question")
				return 0, nil
			})}
			entry := answerEntry("yes")
			entry.Kind = EntryReply
			plan, err := e.Score(context.Background(), entry)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(plan.Moves) != 0 {
					t.Fatal("moved below threshold")
				}
			} else {
				if len(plan.Moves) != 1 {
					t.Fatalf("moves %v", plan.Moves)
				}
				m := plan.Moves[0]
				if m.ID != before[0].ID || m.From != before[0].State || m.FromAt != before[0].StateAt || m.To != tc.want || m.At != entry.At || m.Reason != "reply" || m.Text != "yes" {
					t.Fatalf("move %+v", m)
				}
			}
			if !reflect.DeepEqual(before, askSnapshot(t, s)) {
				t.Fatal("Score wrote a move")
			}
		})
	}
}

func TestLateModelErrorReturnsNoPlanOrChanges(t *testing.T) {
	s := ruleStore(t)
	before := seedAsks(t, s, state.Ask{}, state.Ask{})
	calls := 0
	expected := &ModelError{Class: ClassRate, RetryAfter: time.Minute}
	e := Engine{St: s, Model: ruleModel(func(name, _ string) (float64, error) {
		calls++
		if calls == 2 {
			return 0, expected
		}
		return 1, nil
	})}
	entry := answerEntry("This longer reply settles both choices completely.")
	entry.Kind = EntryReply
	plan, err := e.Score(context.Background(), entry)
	if err != expected || !reflect.DeepEqual(plan, state.AskPlan{}) || !reflect.DeepEqual(before, askSnapshot(t, s)) {
		t.Fatalf("partial work escaped, plan %v, error %v", plan, err)
	}
	for _, p := range []float64{math.NaN(), math.Inf(1), -.1, 1.1} {
		e.Model = ruleModel(func(string, string) (float64, error) { return p, nil })
		plan, err = e.Score(context.Background(), answerEntry("Please decide"))
		if me, ok := err.(*ModelError); !ok || me.Class != ClassMalformed || !reflect.DeepEqual(plan, state.AskPlan{}) {
			t.Fatalf("invalid probability: %v %v", plan, err)
		}
	}
}

func TestCandidateBoundsAndChainIsolation(t *testing.T) {
	s := ruleStore(t)
	var items []state.Ask
	for i := 1; i <= 25; i++ {
		items = append(items, state.Ask{RaisedAt: int64(i), FirstQuote: fmt.Sprintf("open %d", i), Refs: []string{"#7"}})
	}
	for i := 26; i <= 35; i++ {
		items = append(items, state.Ask{RaisedAt: int64(i), FirstQuote: fmt.Sprintf("closed %d", i), State: state.AskClosed, Refs: []string{"#7"}})
	}
	items = append(items, state.Ask{Chain: "other-chain", FirstQuote: "do not disclose other-chain", Refs: []string{"#7"}})
	seedAsks(t, s, items...)
	seen := map[string]bool{}
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "same" {
			if strings.Contains(text, "other-chain") || seen[text] {
				t.Error("cross-chain or duplicate comparison")
			}
			seen[text] = true
			return 0, nil
		}
		return 1, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("please decide #7"))
	if err != nil || len(seen) != 30 || len(plan.Inserts) != 1 {
		t.Fatalf("comparisons %d, plan %v, error %v", len(seen), plan, err)
	}
	for _, tc := range []struct {
		text  string
		count int
	}{{strings.Repeat("雪", 30), 1}, {strings.Repeat("雪", 31), 12}, {strings.Repeat("雪", 31) + " #7", 24}} {
		calls := 0
		e.Model = ruleModel(func(name, text string) (float64, error) {
			if name == "settles" {
				calls++
			}
			if name != "settles" && name != "responds" || strings.Contains(text, "closed ") || strings.Contains(text, "other-chain") {
				t.Error("invalid reply candidate")
			}
			return 0, nil
		})
		entry := answerEntry(tc.text)
		entry.Kind = EntryReply
		if _, err := e.Score(context.Background(), entry); err != nil || calls != tc.count {
			t.Fatalf("reply comparisons %d want %d, error %v", calls, tc.count, err)
		}
	}
}

func TestReplyCanTargetProbablyDoneButNotClosedOrDismissed(t *testing.T) {
	s := ruleStore(t)
	seedAsks(t, s, state.Ask{State: state.AskProbablyDone, RaisedAt: 10, FirstQuote: "pending"}, state.Ask{State: state.AskClosed, RaisedAt: 20, FirstQuote: "closed"}, state.Ask{State: state.AskDismissed, RaisedAt: 30, FirstQuote: "dismissed"})
	e := Engine{St: s, Model: ruleModel(func(_, text string) (float64, error) {
		if !strings.Contains(text, "pending") || strings.Contains(text, "dismissed") || strings.Contains(text, "closed") {
			t.Error("wrong candidate")
		}
		return 1, nil
	})}
	entry := answerEntry("yes")
	entry.Kind = EntryReply
	plan, err := e.Score(context.Background(), entry)
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].From != state.AskProbablyDone {
		t.Fatalf("plan %v, error %v", plan, err)
	}
}

func TestParagraphLimitAndDefensiveCleaning(t *testing.T) {
	s := ruleStore(t)
	paragraphs := []string{}
	for i := 0; i < 25; i++ {
		paragraphs = append(paragraphs, fmt.Sprintf("paragraph %02d", i))
	}
	holds := 0
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if len(text) > 8192 || !utf8.ValidString(text) {
			t.Error("invalid state cap")
		}
		if name == "holds_item" {
			holds++
		}
		return 1, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry(strings.Join(paragraphs, "\n\n")))
	if err != nil || holds != 20 || plan.Inserts[0].FirstQuote != strings.Join(paragraphs[:20], "\n\n") {
		t.Fatalf("holds %d, plan %v, error %v", holds, plan, err)
	}
	const secret = "fictional-long-secret-value"
	e.Model = ruleModel(func(_, text string) (float64, error) {
		if strings.Contains(text, secret) || strings.Contains(text, "\x1b") || len(text) > 8192 || !utf8.ValidString(text) {
			t.Error("unclean model text")
		}
		return 1, nil
	})
	plan, err = e.Score(context.Background(), answerEntry("api_\x1b[31mkey\x1b[0m=\""+secret+"\" "+strings.Repeat("雪", 5000)))
	if err != nil {
		t.Fatal(err)
	}
	a := plan.Inserts[0]
	if len(a.FirstQuote) > 4096 || len(a.LastQuote) > 4096 || !utf8.ValidString(a.FirstQuote) || strings.Contains(a.FirstQuote, secret) {
		t.Fatal("unclean or oversized stored quote")
	}
}

func TestModelPayloadExcludesSessionMetadata(t *testing.T) {
	s := ruleStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, private := range []string{"metadata-chain-canary", "/invented/workspace-canary", "metadata-session-canary"} {
			if strings.Contains(string(b), private) {
				t.Error("session metadata in request")
			}
		}
		var request struct{ Questions map[string]json.RawMessage }
		if json.Unmarshal(b, &request) != nil {
			t.Error("invalid JSON")
			w.WriteHeader(500)
			return
		}
		answers := map[string]any{}
		for name := range request.Questions {
			answers[name] = map[string]any{"noul": 1}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": answers})
	}))
	defer srv.Close()
	c := NewClient("fixture-key")
	c.Endpoint = srv.URL
	e := Engine{St: s, Model: c}
	entry := answerEntry("Please decide the release.")
	entry.Chain, entry.Workspace, entry.SessionID = "metadata-chain-canary", "/invented/workspace-canary", "metadata-session-canary"
	plan, err := e.Score(context.Background(), entry)
	if err != nil || len(plan.Inserts) != 1 {
		t.Fatalf("plan %v, error %v", plan, err)
	}
}

func TestMayCloseAndReferences(t *testing.T) {
	for _, text := range []string{"", " /check", "[Example watcher] CI is red", "[Example CI watcher] CI is red", "[Example Release CI watcher] notice", "<task-notification>done", "[Image #1]", "Base directory for this skill", "Stop hook feedback"} {
		if MayClose(text) {
			t.Errorf("allowed notice %q", text)
		}
	}
	for _, text := range []string{"yes", "[Stop the watcher] and roll back the deploy", "[Do not restart the CI watcher] deploy now", "[Kill the watcher] then merge", "[Example watcher] CI is red\n\nStop the deploy and roll it back, I decided.", "<pasted_content id=\"fixture\">log</pasted_content>\nUse option B.", "[Four Capital Words Here watcher] yes"} {
		if !MayClose(text) {
			t.Errorf("withheld reply %q", text)
		}
	}
	refs := References("PR 123 #123 pr 456 ABC12DEF345 #456 abcdefa abc123 1234567 " + strings.Repeat("a", 41))
	if !reflect.DeepEqual(refs, []string{"#123", "#456", "abc12de", "1234567"}) {
		t.Fatalf("refs %v", refs)
	}
	if refs := References("api_key=\"abc1234def5678\""); len(refs) != 0 {
		t.Fatalf("secret became ref %v", refs)
	}
}

func laterEntry(text string, at int64) Entry {
	return Entry{Meta: Meta{Chain: "c", Workspace: "/w", Provider: "claude", SessionID: "s"}, Kind: EntryAnswer, Text: text, At: at}
}

func seedAsk(t *testing.T, st *state.Store, at int64, quote string, refs ...string) state.Ask {
	t.Helper()
	st.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "seed", Chain: "c"})
	row, _ := st.GetAskSession("claude", "seed")
	to := row.Position
	to.ItemsDone++
	if err := st.ApplyAskPlan("claude", "seed", row.Position, to, 0, state.AskPlan{Inserts: []state.Ask{{Chain: "c", RaisedAt: at, FirstQuote: quote, Refs: refs}}}); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListAsks("c")
	return items[0]
}

func TestLaterMessageRule(t *testing.T) {
	score := func(finished float64) Model {
		return ruleModel(func(name, _ string) (float64, error) {
			if name == "finished" {
				return finished, nil
			}
			return 0, nil
		})
	}
	st := ruleStore(t)
	a := seedAsk(t, st, 100, "Please merge #33", "#33")
	seedAsk(t, st, 101, "Please merge #40", "#40")
	seedAsk(t, st, 102, "No reference here")
	e := &Engine{St: st, Model: score(.5)}
	plan, err := e.Score(context.Background(), laterEntry("Merged #33, all done.", 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].ID != a.ID || plan.Moves[0].To != state.AskProbablyDone || plan.Moves[0].Reason != "later-message" {
		t.Fatalf("only the item sharing a reference moves, and only to probably-done: %+v", plan.Moves)
	}
	e.Model = score(.49)
	if plan, _ := e.Score(context.Background(), laterEntry("Merged #33.", 200)); len(plan.Moves) != 0 {
		t.Fatalf("below the threshold: %+v", plan.Moves)
	}
	e.Model = score(1)
	if plan, _ := e.Score(context.Background(), laterEntry("Older note on #33.", 50)); len(plan.Moves) != 0 {
		t.Fatalf("a message older than the item: %+v", plan.Moves)
	}
}

func TestLaterMessageLeavesARestoredItem(t *testing.T) {
	st := ruleStore(t)
	a := seedAsk(t, st, 100, "Please merge #33", "#33")
	st.SetAskByPerson(a.ID, state.AskDismissed, 110)
	st.SetAskByPerson(a.ID, state.AskOpen, 120)
	e := &Engine{St: st, Model: ruleModel(func(string, string) (float64, error) { return 1, nil })}
	moves, err := e.Later(context.Background(), laterEntry("Merged #33.", 200), nil)
	if err != nil || len(moves) != 0 {
		t.Fatalf("%v %+v", err, moves)
	}
}

func TestLaterMessageSkipsWhatItRepeats(t *testing.T) {
	st := ruleStore(t)
	seedAsk(t, st, 100, "Please merge #33", "#33")
	e := &Engine{St: st, Model: ruleModel(func(name, _ string) (float64, error) { return 1, nil })}
	plan, err := e.Score(context.Background(), laterEntry("Still waiting: please merge #33.", 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repeats) != 1 || len(plan.Moves) != 0 {
		t.Fatalf("%+v", plan)
	}
}

// A numbered list of asks becomes one item per point; a list with one ask stays one item.
func TestNumberedListIsOneItemPerPoint(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch name {
		case "holds_item":
			if strings.Contains(text, "report") || text == "That is all." {
				return .1, nil
			}
			return .9, nil
		case "same":
			return 0, nil
		}
		return 1, nil
	})}
	text := "Needs you:\n1. **Send** the note to the example team.\n   It is drafted.\n2. Pick a clock owner, see #12.\n3. A report of done work.\n\nThat is all."
	plan, err := e.Score(context.Background(), answerEntry(text))
	if err != nil || len(plan.Inserts) != 2 {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	if !strings.HasPrefix(plan.Inserts[0].FirstQuote, "Needs you:\n\n1. **Send**") || !strings.Contains(plan.Inserts[0].FirstQuote, "It is drafted.") ||
		!strings.HasPrefix(plan.Inserts[1].FirstQuote, "Needs you:\n\n2. Pick") || !reflect.DeepEqual(plan.Inserts[1].Refs, []string{"#12"}) || len(plan.Inserts[0].Refs) != 0 {
		t.Fatalf("points %+v", plan.Inserts)
	}
	plan, err = e.Score(context.Background(), answerEntry("Options:\n1. A report of one.\n2. Pick a clock owner."))
	if err != nil || len(plan.Inserts) != 1 {
		t.Fatalf("one ask in a list: %+v, %v", plan, err)
	}
}

// A reply that only takes up items moves the one it takes up most, to probably done.
func TestReplyThatTakesUpAnItemMovesTheBestOne(t *testing.T) {
	s := ruleStore(t)
	seedAsks(t, s, state.Ask{RaisedAt: 1, FirstQuote: "send the draft"}, state.Ask{RaisedAt: 2, FirstQuote: "pick an owner"})
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch {
		case name == "settles":
			return .2, nil
		case strings.Contains(text, "send the draft"):
			return .9, nil
		}
		return .84, nil
	})}
	entry := answerEntry("OK, I think this is a good draft. Sent it.")
	entry.Kind = EntryReply
	plan, err := e.Score(context.Background(), entry)
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].To != state.AskProbablyDone {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	items, _ := s.ListAsks("chain")
	for _, a := range items {
		if a.ID == plan.Moves[0].ID && a.FirstQuote != "send the draft" {
			t.Fatalf("moved %q", a.FirstQuote)
		}
	}
}

// A numbered list of options for one choice stays one item.
func TestNumberedOptionsStayOneItem(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, _ string) (float64, error) {
		switch name {
		case "separate", "same":
			return .1, nil
		}
		return .9, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("Two options:\n1. Turn it off.\n2. Keep it, forward-only."))
	if err != nil || len(plan.Inserts) != 1 || !strings.Contains(plan.Inserts[0].FirstQuote, "Turn it off") {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
}

// A numbered list with blank lines between its points splits like one without.
func TestLooseNumberedListIsOneItemPerPoint(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch name {
		case "holds_item":
			if strings.HasPrefix(text, "Needs you") {
				return .1, nil
			}
			return .9, nil
		case "same":
			return 0, nil
		}
		return 1, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("Needs you:\n1. Send the example draft.\n\n2. Pick an owner.\n\n3. Approve the example release."))
	if err != nil || len(plan.Inserts) != 3 {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	for i, want := range []string{"1. Send", "2. Pick", "3. Approve"} {
		if !strings.HasPrefix(plan.Inserts[i].FirstQuote, "Needs you:\n\n"+want) {
			t.Fatalf("point %d: %q", i, plan.Inserts[i].FirstQuote)
		}
	}
	if got := splitParagraphs("One line with 1. inside.\n\nNo list here."); got[0].point || got[1].point {
		t.Fatalf("a lone number made a point: %+v", got)
	}
}

// Asks in plain paragraphs all go in the item's quote, in the message's order.
func TestEveryParagraphThatAsksIsQuoted(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch name {
		case "holds_item":
			if strings.HasPrefix(text, "Done") {
				return .1, nil
			}
			return .9, nil
		case "same":
			return 0, nil
		}
		return 1, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("Send the example draft.\n\nDone with the build.\n\nPick an owner.\n\nApprove the example release."))
	if err != nil || len(plan.Inserts) != 1 {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	if q := plan.Inserts[0].FirstQuote; q != "Send the example draft.\n\nPick an owner.\n\nApprove the example release." {
		t.Fatalf("quote %q", q)
	}
}

// A reply that settles one item does not also move another it only takes up.
func TestReplyThatSettlesOneMovesNoOther(t *testing.T) {
	s := ruleStore(t)
	seedAsks(t, s, state.Ask{RaisedAt: 1, FirstQuote: "send the example draft"}, state.Ask{RaisedAt: 2, FirstQuote: "choose the example owner"})
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		draft := strings.HasPrefix(text, "ITEM:\nsend the example draft")
		switch {
		case name == "settles" && draft:
			return .9, nil
		case name == "settles":
			return .1, nil
		case draft:
			return .1, nil
		}
		return .95, nil
	})}
	entry := answerEntry("The example draft went out this morning, all done.")
	entry.Kind = EntryReply
	plan, err := e.Score(context.Background(), entry)
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].To != state.AskClosed {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
}

// An ask in a paragraph outside a numbered list is kept as its own item.
func TestAskOutsideTheListIsKept(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "same" {
			return 0, nil
		}
		return .9, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("1. Send the example draft.\n2. Pick an owner.\n\nPlease also approve the example release."))
	if err != nil || len(plan.Inserts) != 3 || plan.Inserts[2].FirstQuote != "Please also approve the example release." {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
}

// A long paragraph does not push a later ask out of the quote.
func TestLongParagraphKeepsTheAskAfterIt(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "same" {
			return 0, nil
		}
		return .9, nil
	})}
	long := "Please review the example change. " + strings.Repeat("It touches the example module in several places. ", 90)
	plan, err := e.Score(context.Background(), answerEntry(long+"\n\nPlease also approve the example release."))
	if err != nil || len(plan.Inserts) != 1 {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	q := plan.Inserts[0].FirstQuote
	if len(q) > maxQuoteBytes || !strings.HasPrefix(q, "Please review the example change.") || !strings.HasSuffix(q, "Please also approve the example release.") {
		t.Fatalf("%d bytes: ...%q", len(q), q[max(len(q)-80, 0):])
	}
}

// An ask just before a numbered list is kept, unless it introduces the list with a colon.
func TestAskBeforeTheListIsKept(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "same" {
			return 0, nil
		}
		return .9, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("Please approve the example release.\n\n1. Send the example draft.\n2. Pick an owner."))
	if err != nil || len(plan.Inserts) != 3 || plan.Inserts[2].FirstQuote != "Please approve the example release." {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	plan, err = e.Score(context.Background(), answerEntry("Two things for you:\n\n1. Send the example draft.\n2. Pick an owner."))
	if err != nil || len(plan.Inserts) != 2 {
		t.Fatalf("a lead-in became an item: %+v, error %v", plan, err)
	}
}

// A paired input never cuts the later text; the quote gives way, and a later text too long
// to pair is not compared.
func TestPairKeepsTheWholeReply(t *testing.T) {
	quote := strings.Repeat("q", 4<<10)
	reply := strings.Repeat("r", 5<<10) + " but production still needs review"
	in, ok, whole := pair("ITEM", quote, "REPLY", reply)
	if !ok || whole || len(in) > maxTextBytes || !strings.HasSuffix(in, "production still needs review") || !strings.HasPrefix(in, "ITEM:\nqqq") {
		t.Fatalf("ok %v, whole %v, %d bytes", ok, whole, len(in))
	}
	if _, ok, _ := pair("ITEM", quote, "REPLY", strings.Repeat("r", maxTextBytes)); ok {
		t.Fatal("a reply with no room for the item was paired")
	}
	if _, ok, whole := pair("ITEM", "Approve staging.", "REPLY", "Approved."); !ok || !whole {
		t.Fatalf("a short pair: ok %v, whole %v", ok, whole)
	}
}

// A reply compared with only part of an item cannot close it: the part left out may ask
// for more than the reply gives.
func TestReplyAgainstACutItemLeavesItPending(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, _ string) (float64, error) {
		if name == "settles" {
			return .95, nil
		}
		return 0, nil
	})}
	seedAsk(t, s, 100, "Approve staging. "+strings.Repeat("q", 4<<10)+" Then approve production.")
	reply := "Staging is approved. " + strings.Repeat("r", 5<<10)
	plan, err := e.Score(context.Background(), Entry{Meta: Meta{Chain: "c"}, Kind: EntryReply, Text: reply, At: 1000})
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].To != state.AskProbablyDone {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
}

// A short reply goes to the item the agent raised last, and a repeat raises it again.
func TestShortReplyGoesToTheItemRepeatedLast(t *testing.T) {
	s := ruleStore(t)
	var asked []string
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "settles" {
			asked = append(asked, text)
			if strings.Contains(text, "Approve the release") {
				return .9, nil
			}
		}
		return 0, nil
	})}
	release := seedAsk(t, s, 100, "Approve the release?")
	seedAsk(t, s, 200, "Approve removing the dependency?")
	row, _ := s.GetAskSession("claude", "seed")
	to := row.Position
	to.ItemsDone++
	if err := s.ApplyAskPlan("claude", "seed", row.Position, to, 0, state.AskPlan{Repeats: []state.AskRepeat{{ID: release.ID, Quote: "Approve the release?", At: 300}}}); err != nil {
		t.Fatal(err)
	}
	plan, err := e.Score(context.Background(), Entry{Meta: Meta{Chain: "c"}, Kind: EntryReply, Text: "Approved", At: 1000})
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].ID != release.ID {
		t.Fatalf("plan %+v, error %v, asked %q", plan, err, asked)
	}
}

// A point keeps the line that introduces its list, with the references in it.
func TestListIntroGivesEachPointItsSubject(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch name {
		case "holds_item":
			if strings.HasPrefix(text, "For PR") {
				return .1, nil
			}
			return .9, nil
		case "same":
			return 0, nil
		}
		return 1, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("For PR #33, please:\n1. Obtain security approval.\n2. Run the production smoke test."))
	if err != nil || len(plan.Inserts) != 2 {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	for _, a := range plan.Inserts {
		if !strings.HasPrefix(a.FirstQuote, "For PR #33, please:") || !reflect.DeepEqual(a.Refs, []string{"#33"}) {
			t.Fatalf("point %q, refs %v", a.FirstQuote, a.Refs)
		}
	}
}

// An item whose quote was cut when it was stored is not closed by a reply: the cut part
// may ask for more.
func TestReplyToAnItemStoredInPartLeavesItPending(t *testing.T) {
	quote := quoteAll([]string{"Approve staging. " + strings.Repeat("q", 5<<10) + " Then approve production."}, maxQuoteBytes)
	if !partial(quote) || len(quote) > maxQuoteBytes {
		t.Fatalf("cut quote: partial %v, %d bytes", partial(quote), len(quote))
	}
	if two := quoteAll([]string{strings.Repeat("a", 3<<10), strings.Repeat("b", 3<<10), "Short."}, maxQuoteBytes); !partial(two) || len(two) > maxQuoteBytes {
		t.Fatalf("a quote cut in its middle paragraphs: partial %v, %d bytes", partial(two), len(two))
	}
	if partial(quoteAll([]string{"Approve staging.", "Approve production."}, maxQuoteBytes)) {
		t.Fatal("a whole quote reads as cut")
	}
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, _ string) (float64, error) {
		if name == "settles" {
			return .95, nil
		}
		return 0, nil
	})}
	seedAsk(t, s, 100, quote)
	plan, err := e.Score(context.Background(), Entry{Meta: Meta{Chain: "c"}, Kind: EntryReply, Text: "Staging is approved, go ahead.", At: 1000})
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].To != state.AskProbablyDone {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
}

// A message cut to the length the rules read ends in an ellipsis, so its last paragraph's
// quote reads as cut.
func TestLongMessageIsMarkedCut(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch {
		case name == "same", name == "holds_item" && strings.HasPrefix(text, "Background"):
			return 0, nil
		}
		return .9, nil
	})}
	text := "Background " + strings.Repeat("b", 7900) + "\n\nPlease approve staging and then " + strings.Repeat("production ", 100)
	plan, err := e.Score(context.Background(), answerEntry(text))
	if err != nil || len(plan.Inserts) != 1 || !strings.HasPrefix(plan.Inserts[0].FirstQuote, "Please") || !partial(plan.Inserts[0].FirstQuote) {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
}

// A question with numbered choices keeps the choices, so a reply naming one can be read
// against them.
func TestChoiceListStaysWithItsQuestion(t *testing.T) {
	s := ruleStore(t)
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		switch name {
		case "holds_item":
			if strings.HasPrefix(text, "Which") {
				return .9, nil
			}
			return .1, nil
		case "same":
			return 0, nil
		}
		return 1, nil
	})}
	plan, err := e.Score(context.Background(), answerEntry("Which deployment target should I use?\n1. Staging\n2. Production"))
	if err != nil || len(plan.Inserts) != 1 {
		t.Fatalf("plan %+v, error %v", plan, err)
	}
	if q := plan.Inserts[0].FirstQuote; !strings.Contains(q, "1. Staging") || !strings.Contains(q, "2. Production") {
		t.Fatalf("quote %q", q)
	}
}

// An item raised after the reply does not take the short reply's one place.
func TestShortReplySkipsItemsRaisedAfterIt(t *testing.T) {
	s := ruleStore(t)
	var asked []string
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "settles" {
			asked = append(asked, text)
			if strings.Contains(text, "Approve the release") {
				return .9, nil
			}
		}
		return 0, nil
	})}
	a := seedAsk(t, s, 100, "Approve the release?")
	seedAsk(t, s, 300, "Approve removing the dependency?")
	plan, err := e.Score(context.Background(), Entry{Meta: Meta{Chain: "c"}, Kind: EntryReply, Text: "Approved", At: 200})
	if err != nil || len(plan.Moves) != 1 || plan.Moves[0].ID != a.ID {
		t.Fatalf("plan %+v, error %v, asked %q", plan, err, asked)
	}
}

// Codex records the person's answer to its question inside a tag; the answer is a reply.
func TestQuestionReplyIsThePersonsReply(t *testing.T) {
	for in, want := range map[string]string{
		"<send_user_message_question_reply> yes":                                       "yes",
		"<send_user_message_question_reply>Ship it</send_user_message_question_reply>": "Ship it",
	} {
		if got, ok := replyText(in); !ok || got != want {
			t.Errorf("replyText(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"<send_user_message_question_reply>", "<other_tag> yes"} {
		if got, ok := replyText(in); ok {
			t.Errorf("replyText(%q) = %q, kept", in, got)
		}
	}
}

// A short reply that names an older item by reference is checked against it.
func TestShortReplyReachesTheItemItNames(t *testing.T) {
	s := ruleStore(t)
	asked := map[string]bool{}
	e := Engine{St: s, Model: ruleModel(func(name, text string) (float64, error) {
		if name == "settles" {
			asked[text] = true
			if strings.Contains(text, "#12") && strings.Contains(text, "Merge #12") {
				return .9, nil
			}
		}
		return 0, nil
	})}
	seedAsk(t, s, 100, "Merge #12 when CI passes.", "#12")
	seedAsk(t, s, 200, "Approve #13.", "#13")
	plan, err := e.Score(context.Background(), Entry{Meta: Meta{Chain: "c"}, Kind: EntryReply, Text: "Merged #12", At: 1000})
	if err != nil || len(plan.Moves) != 1 {
		t.Fatalf("plan %+v, error %v, asked %d", plan, err, len(asked))
	}
}
