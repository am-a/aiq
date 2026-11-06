package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

// screenTurns is a session of n turns, each a prompt and its answer: 2n messages.
func screenTurns(n int, reply func(i int) string) *transcript.Session {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", ID: "abcdef123456", Cwd: "/w"}
	for i := 0; i < n; i++ {
		r := fmt.Sprintf("answer %d", i)
		if reply != nil {
			r = reply(i)
		}
		start := at.Add(time.Duration(i) * time.Minute)
		s.Turns = append(s.Turns, transcript.Turn{Prompt: fmt.Sprintf("prompt %d", i), Reply: r, Started: start, Ended: start.Add(30 * time.Second)})
	}
	return s
}

func screenOf(live bool, w, h int, s *transcript.Session) *convoScreen {
	c := newConvoScreen(live)
	c.w, c.h = w, h
	c.apply(convoUpdate{s: s, begin: true})
	return c
}

func press(c *convoScreen, keys ...keypress) bool {
	quit := false
	for _, k := range keys {
		quit = c.input(asksInput{key: k})
	}
	return quit
}

var (
	kDown, kUp       = keypress{code: keyDown}, keypress{code: keyUp}
	kTab, kBackTab   = keypress{r: '\t'}, keypress{code: keyBackTab}
	kFirst, kLast    = keypress{r: 'g'}, keypress{code: keyEnd}
	kPgDn, kPgUp     = keypress{code: keyPgDn}, keypress{code: keyPgUp}
	kCopy, kQuitChar = keypress{r: 'y'}, keypress{r: 'q'}
)

// plainFrame is the screen as text, one row a line.
func plainFrame(c *convoScreen) []string {
	var out []string
	for _, r := range c.frame() {
		out = append(out, kitPlain(r))
	}
	return out
}

// The screen opens on the newest message; j/k and the arrows move one message, g and G go to
// the ends, and q, Escape and Ctrl-C quit.
func TestConvoScreenMovesByMessage(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	c := screenOf(false, 80, 40, screenTurns(5, nil))
	if len(c.msgs) != 10 || c.sel != 9 {
		t.Fatalf("%d messages, %d selected; want 10, the last", len(c.msgs), c.sel)
	}
	steps := []struct {
		k    keypress
		want int
	}{
		{keypress{r: 'k'}, 8}, {kUp, 7}, {keypress{r: 'j'}, 8}, {kDown, 9}, {kDown, 9},
		{kFirst, 0}, {kUp, 0}, {kLast, 9}, {keypress{code: keyHome}, 0}, {kDown, 1},
	}
	for i, s := range steps {
		// j and k come as Down and Up from the tokenizer, as in aiq asks.
		k := s.k
		if k.r == 'j' || k.r == 'k' {
			var tok asksTokenizer
			k = tok.feed([]byte(string(k.r)))[0].key
		}
		if press(c, k); c.sel != s.want {
			t.Fatalf("step %d (%+v): selected %d, want %d", i, s.k, c.sel, s.want)
		}
	}
	if c.msgs[0].kind != "you" || c.msgs[1].kind != "answer" {
		t.Fatalf("kinds %q, %q", c.msgs[0].kind, c.msgs[1].kind)
	}
	// The selected message carries the mark on its first row, and a rail on the rest.
	frame := plainFrame(c)
	if len(frame) != c.h {
		t.Fatalf("%d rows, want %d", len(frame), c.h)
	}
	marked := 0
	for i, row := range frame {
		if cells(row) != c.w {
			t.Errorf("row %d is %d cells wide, want %d: %q", i, cells(row), c.w, row)
		}
		if strings.HasPrefix(row, "▶ ") {
			marked++
			if !strings.Contains(row, "claude") {
				t.Errorf("the mark is not on the answer's head: %q", row)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("%d rows marked:\n%s", marked, strings.Join(frame, "\n"))
	}
	if !strings.Contains(frame[len(frame)-1], "j/k move") || !strings.Contains(frame[len(frame)-1], "y copy") {
		t.Errorf("footer %q", frame[len(frame)-1])
	}
	for _, k := range []keypress{kQuitChar, {code: keyBack}, {code: keyQuit}} {
		if !press(c, k) {
			t.Errorf("%+v does not quit", k)
		}
	}
}

// Tab and Shift-Tab jump between what the person typed: prompts and what they typed while the
// agent worked, never an answer or a notice.
func TestConvoScreenTabJumpsBetweenPrompts(t *testing.T) {
	c := screenOf(false, 80, 60, convoFixture())
	var prompts, notices []int
	for i, m := range c.msgs {
		switch m.kind {
		case "you", "steer":
			prompts = append(prompts, i)
		case "notice":
			notices = append(notices, i)
		}
	}
	if len(prompts) < 4 || len(notices) == 0 {
		t.Fatalf("prompts %v, notices %v", prompts, notices)
	}
	press(c, kFirst)
	var got []int
	for range prompts {
		press(c, kTab)
		got = append(got, c.sel)
	}
	if fmt.Sprint(got) != fmt.Sprint(append(prompts[1:], prompts[len(prompts)-1])) {
		t.Fatalf("Tab went %v, prompts are %v", got, prompts)
	}
	got = nil
	for range prompts {
		press(c, kBackTab)
		got = append(got, c.sel)
	}
	want := []int{}
	for i := len(prompts) - 2; i >= 0; i-- {
		want = append(want, prompts[i])
	}
	if fmt.Sprint(got) != fmt.Sprint(append(want, prompts[0])) {
		t.Fatalf("Shift-Tab went %v, prompts are %v", got, prompts)
	}
	// Shift-Tab reaches the screen from the terminal as ESC [ Z.
	var tok asksTokenizer
	if ev := tok.feed([]byte("\x1b[Z")); len(ev) != 1 || ev[0].key.code != keyBackTab {
		t.Fatalf("ESC [ Z reads as %+v", ev)
	}
}

// y copies the selected message as the transcript has it, cleaned as the printer cleans it:
// the Markdown, never what the terminal shows.
func TestConvoScreenCopiesTheMarkdown(t *testing.T) {
	var got []string
	fail := error(nil)
	saved := asksCopier
	asksCopier = func(text string) (string, error) {
		got = append(got, text)
		if fail != nil {
			return "", fail
		}
		return "clipboard", nil
	}
	t.Cleanup(func() { asksCopier = saved })
	reply := "**Done.** Run `make test`.\n\n- one\n- two\x1b]52;c;eA==\x07\n"
	s := screenTurns(2, func(i int) string {
		if i == 1 {
			return reply
		}
		return "first answer"
	})
	c := screenOf(false, 80, 40, s)
	press(c, kCopy)
	want := "**Done.** Run `make test`.\n\n- one\n- two]52;c;eA=="
	if len(got) != 1 || got[0] != want {
		t.Fatalf("copied %q, want %q", got, want)
	}
	if c.message != "Copied the message to the clipboard" {
		t.Fatalf("message %q", c.message)
	}
	if frame := plainFrame(c); !strings.Contains(frame[len(frame)-2], "Copied the message to the clipboard") {
		t.Fatalf("status row %q", frame[len(frame)-2])
	}
	press(c, kUp, kCopy)
	if got[1] != "prompt 1" {
		t.Fatalf("copied the prompt as %q", got[1])
	}
	fail = errors.New("no clipboard found")
	press(c, kCopy)
	if c.message != "Could not copy: no clipboard found" {
		t.Fatalf("message %q", c.message)
	}
	press(c, kDown)
	if c.message != "" {
		t.Fatalf("the message outlives the next key: %q", c.message)
	}
}

// Every row the screen shows is the printer's, in its order: the screen only cuts it up, and
// wraps what the stream left the terminal to wrap.
func TestConvoScreenShowsWhatThePrinterPrints(t *testing.T) {
	s := convoFixture()
	s.Turns[6].Open = false // a "(no reply)" goes on its prompt
	s.Turns[6].Reply = "Shipped, with a line long enough that a screen this narrow has to wrap it somewhere."
	s.Turns[0].Reply = "A `code span` and **bold words** that run on past the edge of the screen, wrapped."
	c := screenOf(false, 40, 40, s)
	p := newConvoPrinter(s.Provider, true, false)
	p.width = 40 - convoGutter
	printed := p.emit(s)
	var rows []string
	for _, r := range c.rows {
		rows = append(rows, r.text)
		if n := cells(kitPlain(r.text)); n > p.width {
			t.Errorf("row %d cells wide, more than %d: %q", n, p.width, kitPlain(r.text))
		}
	}
	squeeze := strings.NewReplacer(" ", "", "\n", "")
	if got, want := squeeze.Replace(kitPlain(strings.Join(rows, "\n"))), squeeze.Replace(kitPlain(printed)); got != want {
		t.Fatalf("rows differ from the printer\nrows:\n%s\nprinter:\n%s", strings.Join(rows, "\n"), printed)
	}
	for i, m := range c.msgs {
		if m.from >= m.to || c.rows[m.from].text == "" {
			t.Errorf("message %d (%s) starts with a blank row or is empty", i, m.kind)
		}
		for r := m.from; r < m.to; r++ {
			if c.rows[r].msg != i {
				t.Errorf("row %d of message %d says %d", r, i, c.rows[r].msg)
			}
		}
	}
	for _, m := range c.msgs {
		if m.kind == "none" || m.kind == "more" {
			t.Errorf("a %s stands alone", m.kind)
		}
	}
}

// A line too wide breaks at its spaces, else between glyphs, and each row opens with the
// styles still in force where it broke.
func TestConvoWrapCarriesStyles(t *testing.T) {
	line := "plain " + sgrBold + "bold words" + sgrReset + " and " + sgrCode + "averyveryverylongword" + sgrReset
	got := convoWrap(line, 8)
	want := []string{
		"plain" + sgrReset,
		sgrBold + "bold" + sgrReset,
		sgrBold + "words" + sgrReset + sgrReset,
		"and" + sgrReset,
		sgrCode + "averyver" + sgrReset,
		sgrCode + "yverylon" + sgrReset,
		sgrCode + "gword" + sgrReset + sgrReset,
	}
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := convoWrap(line, 80); len(got) != 1 || got[0] != line {
		t.Fatalf("a line that fits: %q", got)
	}
}

// --follow: with the newest message selected, a new one is selected as it comes. Moved up,
// the selection and the view stay, the status line counts what is new below, and G follows
// again.
func TestConvoScreenFollowsTheNewest(t *testing.T) {
	full := screenTurns(8, nil)
	upTo := func(n int) *transcript.Session {
		s := *full
		s.Turns = full.Turns[:n]
		return &s
	}
	c := newConvoScreen(true)
	c.w, c.h = 80, 14
	c.apply(convoUpdate{s: upTo(3), begin: true})
	if c.sel != 5 || !c.visible(5) {
		t.Fatalf("selected %d of %d", c.sel, len(c.msgs))
	}
	c.apply(convoUpdate{s: upTo(4)})
	if c.sel != 7 || !c.visible(7) || c.newBelow() != 0 {
		t.Fatalf("at the end: selected %d of %d, %d new", c.sel, len(c.msgs), c.newBelow())
	}
	press(c, kUp, kUp, kUp, kUp, kUp)
	id, top := c.msgs[c.sel].id, c.top
	if top == 0 {
		t.Fatal("the test wants a screen scrolled down")
	}
	c.apply(convoUpdate{s: upTo(6)})
	if c.msgs[c.sel].id != id || c.top != top {
		t.Fatalf("moved up: selection %q top %d, want %q top %d", c.msgs[c.sel].id, c.top, id, top)
	}
	if c.newBelow() != 4 {
		t.Fatalf("%d new below, want 4", c.newBelow())
	}
	if status := plainFrame(c)[c.h-2]; !strings.Contains(status, "4 new below, G follows") {
		t.Fatalf("status %q", status)
	}
	press(c, kLast)
	if c.sel != 11 || c.newBelow() != 0 {
		t.Fatalf("G: selected %d, %d new", c.sel, c.newBelow())
	}
	c.apply(convoUpdate{s: upTo(8)})
	if c.sel != 15 || !c.visible(15) {
		t.Fatalf("following again: selected %d of %d", c.sel, len(c.msgs))
	}
	// What --follow says shows on the status line.
	c.apply(convoUpdate{status: "working · 1m · 2 tools"})
	if status := plainFrame(c)[c.h-2]; !strings.Contains(status, "working · 1m · 2 tools") {
		t.Fatalf("status %q", status)
	}
	// The pane moved: a divider, and the new conversation below it, followed.
	moved := screenTurns(1, func(int) string { return "fresh start" })
	moved.ID = "99999999ffff"
	c.apply(convoUpdate{s: moved, t: convoTarget{account: "codex/b"}, begin: true, moved: time.Now()})
	frame := strings.Join(plainFrame(c), "\n")
	if !strings.Contains(frame, "MOVED TO CODEX/B · SESSION 99999999") || !strings.Contains(frame, "fresh start") {
		t.Fatalf("after a move:\n%s", frame)
	}
	if !strings.Contains(plainFrame(c)[0], "session 99999999") {
		t.Errorf("header %q", plainFrame(c)[0])
	}
	if c.sel != len(c.msgs)-1 || c.msgs[c.sel].raw != "fresh start" {
		t.Fatalf("after a move: selected %d of %d", c.sel, len(c.msgs))
	}
}

// A resize re-wraps the conversation and keeps the selected message on screen.
func TestConvoScreenResizeKeepsTheSelection(t *testing.T) {
	long := strings.Repeat("several words that wrap ", 12)
	c := screenOf(true, 100, 30, screenTurns(10, func(i int) string { return fmt.Sprintf("%d %s", i, long) }))
	press(c, kUp, kUp, kUp, kUp, kUp)
	id, rows := c.msgs[c.sel].id, len(c.rows)
	c.resize(40, 30)
	if c.msgs[c.sel].id != id || !c.visible(c.sel) {
		t.Fatalf("after narrowing: selected %q, want %q (visible %v)", c.msgs[c.sel].id, id, c.visible(c.sel))
	}
	if len(c.rows) <= rows {
		t.Fatalf("not re-wrapped: %d rows, was %d", len(c.rows), rows)
	}
	for _, r := range plainFrame(c) {
		if cells(r) != 40 {
			t.Fatalf("row %d cells wide after the resize: %q", cells(r), r)
		}
	}
	c.resize(100, 12)
	if c.msgs[c.sel].id != id || !c.visible(c.sel) {
		t.Fatalf("after shortening: selected %q, want %q", c.msgs[c.sel].id, id)
	}
	// Following stays following.
	press(c, kLast)
	c.resize(60, 20)
	if c.sel != len(c.msgs)-1 || !c.visible(c.sel) {
		t.Fatalf("following: selected %d of %d", c.sel, len(c.msgs))
	}
}

// A message taller than the screen shows its start when selected; j goes on through it a few
// rows at a time and then to the next message, k back through it and then to the one before.
func TestConvoScreenLongMessage(t *testing.T) {
	var paras []string
	for i := 0; i < 60; i++ {
		paras = append(paras, fmt.Sprintf("paragraph %d", i))
	}
	s := screenTurns(3, func(i int) string {
		if i == 1 {
			return strings.Join(paras, "\n\n")
		}
		return fmt.Sprintf("short %d", i)
	})
	c := screenOf(false, 80, 20, s)
	h := c.height()
	press(c, kUp, kUp) // the prompt, then the long answer
	long := c.msgs[c.sel]
	if c.sel != 3 || long.to-long.from <= h {
		t.Fatalf("selected %d, %d rows high on a screen of %d", c.sel, long.to-long.from, h)
	}
	if c.top != long.from {
		t.Fatalf("top %d, want the answer's start %d", c.top, long.from)
	}
	if !strings.Contains(plainFrame(c)[len(c.head)], "claude") {
		t.Fatalf("the first row shown is %q", plainFrame(c)[len(c.head)])
	}
	press(c, kDown)
	if c.sel != 3 || c.top != long.from+convoStep {
		t.Fatalf("j: selected %d top %d, want 3 and %d", c.sel, c.top, long.from+convoStep)
	}
	press(c, kUp)
	if c.sel != 3 || c.top != long.from {
		t.Fatalf("k: selected %d top %d", c.sel, c.top)
	}
	n := 0
	for c.sel == 3 {
		press(c, kDown)
		if n++; n > 100 {
			t.Fatal("j never leaves the long answer")
		}
	}
	if c.sel != 4 {
		t.Fatalf("after the long answer: selected %d", c.sel)
	}
	// Its end was on screen before j moved on.
	if n != (long.to-h-long.from+convoStep-1)/convoStep+1 {
		t.Errorf("%d presses through the answer", n)
	}
	press(c, kUp)
	if c.sel != 3 || c.top != long.from {
		t.Fatalf("back up: selected %d top %d, want its start %d", c.sel, c.top, long.from)
	}
	press(c, kUp)
	if c.sel != 2 {
		t.Fatalf("k at the start: selected %d", c.sel)
	}
	// Page Down scrolls a page and keeps the selection on screen.
	press(c, kFirst, kPgDn)
	if c.top != h-1 || !c.visible(c.sel) {
		t.Fatalf("PgDn: top %d, selected %d visible %v", c.top, c.sel, c.visible(c.sel))
	}
	press(c, kPgUp)
	if c.top != 0 || !c.visible(c.sel) {
		t.Fatalf("PgUp: top %d", c.top)
	}
}

// The wheel scrolls with the selection kept on screen; a click selects the message under it.
func TestConvoScreenMouse(t *testing.T) {
	c := screenOf(false, 80, 14, screenTurns(8, nil))
	top := c.top
	c.input(asksInput{mouse: true, button: 64, x: 5, y: 5})
	if c.top != top-convoStep || !c.visible(c.sel) {
		t.Fatalf("wheel up: top %d, want %d; selection visible %v", c.top, top-convoStep, c.visible(c.sel))
	}
	for i := 0; i < 40; i++ {
		c.input(asksInput{mouse: true, button: 64, x: 5, y: 5})
	}
	if c.top != 0 || !c.visible(c.sel) {
		t.Fatalf("wheel to the top: top %d, selected %d", c.top, c.sel)
	}
	// The row under the click: header rows, then the conversation from top.
	for y := len(c.head) + 1; y <= len(c.head)+c.height(); y++ {
		r := c.rows[c.top+y-1-len(c.head)]
		if r.msg < 0 || r.msg == c.sel {
			continue
		}
		c.input(asksInput{mouse: true, button: 0, x: 3, y: y})
		if c.sel != r.msg {
			t.Fatalf("click on row %d: selected %d, want %d", y, c.sel, r.msg)
		}
		return
	}
	t.Fatal("no other message on screen to click")
}

// Off a terminal aiq convo prints plain text, as it always has.
func TestConvoPrintsPlainTextOffATerminal(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(home, "data"))
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "TMUX_PANE", "AIQ_THEME", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(v, "")
	}
	t.Chdir(dir)
	const id = "5e551011-0000-4000-8000-000000000001"
	project := filepath.Join(home, ".claude", "projects", claudeNonAlnum.ReplaceAllString(dir, "-"))
	os.MkdirAll(project, 0o700)
	c := `"cwd":"` + dir + `","sessionId":"` + id + `"`
	user := func(ts, text string) string {
		return `{"type":"user",` + c + `,"timestamp":"` + ts + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	reply := func(ts, mid, text string) string {
		return `{"type":"assistant",` + c + `,"timestamp":"` + ts + `","message":{"id":"` + mid + `","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(filepath.Join(project, id+".jsonl"), []byte(user("2026-09-15T10:00:00Z", "first")+reply("2026-09-15T10:01:00Z", "m1", "one")+
		user("2026-09-15T10:02:00Z", "second")+reply("2026-09-15T10:03:00Z", "m2", "**two** and `three`")), 0o600)

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := cmdConvo([]string{"5e551011"})
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	when := func(ts string) string {
		at, _ := time.Parse(time.RFC3339, ts)
		return whenLabel(at, time.Now())
	}
	want := "claude opus-5 · " + dir + " · session 5e551011\n" +
		"\n▌ you · " + when("2026-09-15T10:00:00Z") + "\nfirst\n" +
		"\n▌ claude · 1m\none\n" +
		"\n▌ you · " + when("2026-09-15T10:02:00Z") + "\nsecond\n" +
		"\n▌ claude · 1m\n**two** and `three`\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
}

// On a terminal --follow's loop hands what it finds to the screen and prints nothing: the
// first conversation, what the file gains, and the status line.
func TestFollowFeedsTheScreen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	c := `"cwd":"/w","sessionId":"s1"`
	user := func(ts, text string) string {
		return `{"type":"user",` + c + `,"timestamp":"` + ts + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	reply := func(ts, id, text string) string {
		return `{"type":"assistant",` + c + `,"timestamp":"` + ts + `","message":{"id":"` + id + `","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(path, []byte(user("2026-09-15T10:00:00Z", "first")+reply("2026-09-15T10:01:00Z", "m1", "one")), 0o644)
	var stream bytes.Buffer
	updates := make(chan convoUpdate, 64)
	f := &convoFollow{tgt: convoTarget{provider: "claude", path: path}, w: &stream, tty: true,
		statEvery: 10 * time.Millisecond, parseGap: 30 * time.Millisecond, resolveEvery: time.Hour, quiet: 50 * time.Millisecond,
		send: func(u convoUpdate) { updates <- u }}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.loop(stop); close(done) }()
	screen := newConvoScreen(true)
	screen.w, screen.h = 80, 30
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for !ok() {
			select {
			case u := <-updates:
				screen.apply(u)
			case <-deadline:
				t.Fatalf("no %s: %d messages, status %q", what, len(screen.msgs), screen.status)
			}
		}
	}
	wait("first conversation", func() bool { return len(screen.msgs) == 2 })
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(user("2026-09-15T10:02:00Z", "second"))
	fh.Close()
	wait("prompt", func() bool { return len(screen.msgs) == 3 && screen.sel == 2 })
	fh, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(reply("2026-09-15T10:03:00Z", "m2", "two"))
	fh.Close()
	wait("reply", func() bool { return len(screen.msgs) == 4 && screen.sel == 3 && screen.msgs[3].raw == "two" })
	close(stop)
	<-done
	if stream.Len() != 0 {
		t.Fatalf("the stream got %q", stream.String())
	}
}

// --follow off a terminal prints the stream, byte for byte as it always has.
func TestFollowStreamOffATerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	c := `"cwd":"/w","sessionId":"s1"`
	os.WriteFile(path, []byte(`{"type":"user",`+c+`,"timestamp":"2026-09-15T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"first"}}`+"\n"+
		`{"type":"assistant",`+c+`,"timestamp":"2026-09-15T10:01:00Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"one"}]}}`+"\n"), 0o644)
	var b bytes.Buffer
	f := &convoFollow{tgt: convoTarget{provider: "claude", path: path}, w: &b,
		statEvery: 10 * time.Millisecond, parseGap: 30 * time.Millisecond, resolveEvery: time.Hour, quiet: time.Millisecond}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.run(stop); close(done) }()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-done
	at, _ := time.Parse(time.RFC3339, "2026-09-15T10:00:00Z")
	want := "claude opus-5 · /w · session s1\n\n▌ you · " + whenLabel(at, time.Now()) + "\nfirst\n\n▌ claude · 1m\none\n"
	if b.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", b.String(), want)
	}
}
