package main

import (
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/transcript"
)

// The conversation on a terminal: a full screen in the style of aiq asks, with one message
// selected. The printer draws each message as the stream and the pager showed it; the screen
// only cuts its output into messages, so how a message looks is the printer's alone.

// convoGutter is the cells left of each row, where the selection mark goes.
const convoGutter = 2

// convoStep is how many rows j/k, Up/Down and the wheel scroll through a message taller than
// the screen, before moving to the next one.
const convoStep = 3

// convoPart is one conversation the screen shows: the first, or one the pane moved to.
type convoPart struct {
	s     *transcript.Session
	t     convoTarget
	from  int       // the first turn shown (--last)
	moved time.Time // when the pane moved to it; zero for the first
	hold  bool      // the last reply is held back
}

// convoRow is one row of the conversation: rendered text, and the message it belongs to.
type convoRow struct {
	text string
	msg  int // -1: the space between messages
}

// convoMsg is one message: its rows, from inclusive to to exclusive, and the Markdown y copies.
type convoMsg struct {
	id       string
	kind     string // as convoCut, with more and none folded into the message before
	raw      string
	from, to int
}

type convoScreen struct {
	parts []convoPart
	live  bool // --follow: an open turn may still answer
	head  []string
	rows  []convoRow
	msgs  []convoMsg
	sel   int
	top   int
	w, h  int
	// known is how many messages there were when the newest was last selected; the rest are
	// new below.
	known   int
	status  string // what --follow says: the turn working, or what went wrong
	message string // what the last key did, such as a copy
}

func newConvoScreen(live bool) *convoScreen {
	return &convoScreen{live: live, w: 80, h: 24}
}

// layout renders every part at the screen's width and cuts it into rows and messages.
func (c *convoScreen) layout() {
	c.rows, c.msgs, c.head = nil, nil, nil
	if len(c.parts) == 0 {
		return
	}
	width := max(c.w-convoGutter, 10)
	for i, pt := range c.parts {
		p := newConvoPrinter(pt.s.Provider, true, false)
		p.width, p.live, p.hold, p.track = width, c.live, pt.hold, true
		if !pt.moved.IsZero() {
			c.rows = append(c.rows, convoRow{msg: -1})
			for _, l := range strings.Split(convoMovedLine(p, pt.t, pt.s, pt.moved, pt.from, width), "\n") {
				c.rows = append(c.rows, convoRow{text: l, msg: -1})
			}
		}
		p.skip(pt.s.Turns[:pt.from])
		c.cut(i, p.emit(pt.s), p.cuts, width)
	}
	// The header names the conversation shown now.
	last := c.parts[len(c.parts)-1]
	c.head = strings.Split(strings.TrimSuffix(convoHeader(last.s, last.t, last.from, true), "\n"), "\n")
}

// convoWrap breaks a rendered line into rows of at most w cells, where the stream left the
// wrapping to the terminal. It breaks at the last space that fits, else between glyphs. Each
// row ends its styles, and the next row opens again with those still in force.
func convoWrap(line string, w int) []string {
	if w < 1 || cells(kitPlain(line)) <= w {
		return []string{line}
	}
	type token struct {
		text  string
		cells int
		sgr   bool
	}
	var toks []token
	glyphs := func(s string) {
		eachGlyph(s, func(g string, c int) bool {
			toks = append(toks, token{text: g, cells: c})
			return true
		})
	}
	at := 0
	for _, m := range kitSGR.FindAllStringIndex(line, -1) {
		glyphs(line[at:m[0]])
		toks = append(toks, token{text: line[m[0]:m[1]], sgr: true})
		at = m[1]
	}
	glyphs(line[at:])
	var rows []string
	open := "" // the styles in force where the row starts
	for start := 0; start < len(toks); {
		used, end, space := 0, start, -1
		for ; end < len(toks); end++ {
			t := toks[end]
			if t.sgr {
				continue
			}
			if used > 0 && used+t.cells > w {
				break
			}
			if t.text == " " {
				space = end
			}
			used += t.cells
		}
		cut, next := end, end
		if end < len(toks) && space > start {
			cut, next = space, space+1 // the space the row breaks at goes
		}
		var b strings.Builder
		b.WriteString(open)
		for _, t := range toks[start:cut] {
			b.WriteString(t.text)
			if t.sgr {
				if t.text == sgrReset || t.text == "\x1b[m" {
					open = ""
				} else {
					open += t.text
				}
			}
		}
		rows = append(rows, b.String()+sgrReset)
		start = next
	}
	return rows
}

// cut splits what a printer emitted into messages, in rows width cells wide at most. The
// blank lines before a message are the space between messages; a later part of an answer and
// a "(no reply)" go on the message before them.
func (c *convoScreen) cut(part int, text string, cuts []convoCut, width int) {
	first := len(c.msgs)
	seen := map[string]int{}
	prev := 0
	for _, ct := range cuts {
		var lines []string
		for _, l := range strings.Split(strings.TrimSuffix(text[prev:ct.end], "\n"), "\n") {
			lines = append(lines, convoWrap(l, width)...)
		}
		prev = ct.end
		raw := strings.TrimRight(sanitize(ct.text), "\n")
		if (ct.kind == "more" || ct.kind == "none") && len(c.msgs) > first {
			n := len(c.msgs) - 1
			for _, l := range lines {
				c.rows = append(c.rows, convoRow{text: l, msg: n})
			}
			c.msgs[n].to = len(c.rows)
			if ct.kind == "more" {
				c.msgs[n].raw += "\n\n" + raw
			}
			continue
		}
		i := 0
		for i < len(lines)-1 && lines[i] == "" {
			c.rows = append(c.rows, convoRow{msg: -1})
			i++
		}
		h := fnv.New64a()
		h.Write([]byte(raw))
		id := fmt.Sprintf("%d\x00%s\x00%s\x00%x", part, ct.key, ct.kind, h.Sum64())
		seen[id]++
		m := convoMsg{id: fmt.Sprintf("%s\x00%d", id, seen[id]), kind: ct.kind, raw: raw, from: len(c.rows)}
		for _, l := range lines[i:] {
			c.rows = append(c.rows, convoRow{text: l, msg: len(c.msgs)})
		}
		m.to = len(c.rows)
		c.msgs = append(c.msgs, m)
	}
}

// height is how many rows of the conversation fit between the header and the two lines at
// the bottom: the status and the keys.
func (c *convoScreen) height() int { return max(c.h-len(c.head)-2, 1) }

func (c *convoScreen) clampTop() { c.top = clamp(c.top, 0, max(len(c.rows)-c.height(), 0)) }

// following says the newest message is selected, so the next one will be.
func (c *convoScreen) following() bool { return len(c.msgs) == 0 || c.sel == len(c.msgs)-1 }

// visible says some row of message i is on screen.
func (c *convoScreen) visible(i int) bool {
	m := c.msgs[i]
	return m.to > c.top && m.from < c.top+c.height()
}

// show scrolls the selected message into view: all of it when it fits, else its start.
func (c *convoScreen) show() {
	if len(c.msgs) == 0 {
		return
	}
	m, h := c.msgs[c.sel], c.height()
	switch {
	case m.to-m.from > h || m.from < c.top:
		c.top = m.from
	case m.to > c.top+h:
		c.top = m.to - h
	}
	c.clampTop()
}

// apply takes what --follow found. A newest message selected moves on to the next one;
// otherwise the selection and its place on screen stay.
func (c *convoScreen) apply(u convoUpdate) {
	if u.s == nil {
		c.status = u.status
		return
	}
	pt := convoPart{s: u.s, t: u.t, from: u.from, moved: u.moved, hold: u.hold}
	if u.begin || len(c.parts) == 0 {
		c.parts = append(c.parts, pt)
	} else {
		last := &c.parts[len(c.parts)-1]
		last.s, last.hold = u.s, u.hold
	}
	c.relayout()
}

// resize re-wraps for a new size and keeps the selection.
func (c *convoScreen) resize(w, h int) {
	c.w, c.h = w, h
	c.relayout()
}

// relayout lays the screen out again, keeping the selected message and where it sits on
// screen.
func (c *convoScreen) relayout() {
	following := c.following()
	id, off, old := "", 0, c.sel
	if len(c.msgs) > 0 {
		id, off = c.msgs[c.sel].id, c.msgs[c.sel].from-c.top
	}
	c.layout()
	if len(c.msgs) == 0 {
		c.sel, c.top = 0, 0
		return
	}
	idx := -1
	for i, m := range c.msgs {
		if m.id == id {
			idx = i
			break
		}
	}
	if idx < 0 || following && idx != len(c.msgs)-1 {
		// A new message to follow, or the one selected is gone.
		c.sel = min(old, len(c.msgs)-1)
		if following {
			c.sel = len(c.msgs) - 1
		}
		c.show()
		c.settle()
		return
	}
	c.sel = idx
	c.top = c.msgs[idx].from - off
	c.clampTop()
	m := c.msgs[idx]
	if !c.visible(idx) || following && m.to-m.from <= c.height() {
		c.show()
	}
	c.settle()
}

// settle notes that the newest message is selected: nothing is new below it.
func (c *convoScreen) settle() {
	if c.following() {
		c.known = len(c.msgs)
	}
}

// down moves on through the selected message while more of it is below the screen, then to
// the next message.
func (c *convoScreen) down() {
	if len(c.msgs) == 0 {
		return
	}
	m, bottom := c.msgs[c.sel], c.top+c.height()
	if m.to > bottom && m.from < bottom {
		c.top = min(c.top+convoStep, m.to-c.height())
		c.clampTop()
		return
	}
	if c.sel < len(c.msgs)-1 {
		c.sel++
	}
	c.show()
}

// up moves back through the selected message while its start is above the screen, then to
// the message before.
func (c *convoScreen) up() {
	if len(c.msgs) == 0 {
		return
	}
	m := c.msgs[c.sel]
	if m.from < c.top && m.to > c.top {
		c.top = max(c.top-convoStep, m.from)
		c.clampTop()
		return
	}
	if c.sel > 0 {
		c.sel--
	}
	c.show()
}

// prompt says message i is something the person typed.
func (c *convoScreen) prompt(i int) bool { return c.msgs[i].kind == "you" || c.msgs[i].kind == "steer" }

// jump selects the next prompt the person typed (d 1) or the one before (d -1).
func (c *convoScreen) jump(d int) {
	for i := c.sel + d; i >= 0 && i < len(c.msgs); i += d {
		if c.prompt(i) {
			c.sel = i
			c.show()
			return
		}
	}
}

// scroll moves the view by d rows. The selection stays on screen, so y copies what can be
// seen: scrolled past it, the nearest message on screen is selected.
func (c *convoScreen) scroll(d int) {
	c.top += d
	c.clampTop()
	if len(c.msgs) == 0 || c.visible(c.sel) {
		return
	}
	if c.msgs[c.sel].to <= c.top {
		for i := range c.msgs {
			if c.visible(i) {
				c.sel = i
				return
			}
		}
		return
	}
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if c.visible(i) {
			c.sel = i
			return
		}
	}
}

// copy puts the selected message's Markdown on the clipboard and says how that went.
func (c *convoScreen) copy() {
	if len(c.msgs) == 0 {
		return
	}
	where, err := asksCopier(c.msgs[c.sel].raw)
	if err != nil {
		c.message = fmt.Sprintf("Could not copy: %v", err)
		return
	}
	c.message = "Copied the message to the " + where
}

// input handles one event and says whether to quit.
func (c *convoScreen) input(e asksInput) bool {
	c.message = ""
	switch {
	case e.paste:
		c.message = "Pasted text does nothing here"
	case e.mouse:
		c.click(e)
	default:
		k := e.key
		switch {
		case k.code == keyQuit || k.code == keyBack || k.r == 'q':
			return true
		case k.code == keyDown:
			c.down()
		case k.code == keyUp:
			c.up()
		case k.r == '\t':
			c.jump(1)
		case k.code == keyBackTab:
			c.jump(-1)
		case k.code == keyHome || k.r == 'g':
			c.sel, c.top = 0, 0
		case k.code == keyEnd:
			c.sel = max(len(c.msgs)-1, 0)
			c.show()
		case k.code == keyPgDn:
			c.scroll(max(c.height()-1, 1))
		case k.code == keyPgUp:
			c.scroll(-max(c.height()-1, 1))
		case k.r == 'y' || k.r == 'c':
			c.copy()
		}
	}
	c.settle()
	return false
}

// click selects the message under a left click; the wheel scrolls.
func (c *convoScreen) click(e asksInput) {
	switch e.button {
	case 64:
		c.scroll(-convoStep)
	case 65:
		c.scroll(convoStep)
	case 0:
		y := e.y - 1 - len(c.head)
		if y < 0 || y >= c.height() || c.top+y >= len(c.rows) {
			return
		}
		if r := c.rows[c.top+y]; r.msg >= 0 {
			c.sel = r.msg
		}
	}
}

// newBelow is how many messages came since the newest was last selected.
func (c *convoScreen) newBelow() int {
	if !c.live || c.following() {
		return 0
	}
	return max(len(c.msgs)-c.known, 0)
}

// footer is one dim line of keys, cut down by width, as in aiq asks.
func (c *convoScreen) footer(th kitTheme) []kitSeg {
	keys := []kitKey{{"j/k", "move", 0}}
	switch {
	case c.w < 50:
		keys = append(keys, kitKey{"y", "copy", 0}, kitKey{"q", "quit", 0})
	case c.w < 80:
		keys = append(keys, kitKey{"Tab", "prompt", 0}, kitKey{"y", "copy", 0}, kitKey{"q", "quit", 0})
	default:
		keys = append(keys, kitKey{"Tab", "prompt", 0}, kitKey{"g/G", "first/last", 0}, kitKey{"PgUp/PgDn", "page", 0},
			kitKey{"y", "copy", 0}, kitKey{"q", "quit", 0})
	}
	segs, _ := kitKeys(keys, th.dim, 1)
	return append([]kitSeg{{text: " "}}, segs...)
}

// statusLine says what the last key did, what is new below, and what --follow reports.
func (c *convoScreen) statusLine(th kitTheme) []kitSeg {
	var segs []kitSeg
	if c.message != "" {
		segs = append(segs, kitSeg{style: sgrBold + th.accent, text: " " + c.message})
	}
	if n := c.newBelow(); n > 0 {
		segs = append(segs, kitSeg{style: sgrBold + th.accent, text: fmt.Sprintf(" %d new below, G follows", n)})
	}
	if c.status != "" {
		segs = append(segs, kitSeg{style: th.dim, text: " " + c.status})
	}
	return segs
}

// frame is the screen, one string a row, each exactly w cells.
func (c *convoScreen) frame() []string {
	th, mk := kitThemeFor(false), kitMarksFor(os.Getenv)
	rail := "│"
	if mk.selected != kitUnicode.selected {
		rail = "|"
	}
	c.clampTop()
	var out []string
	for _, l := range c.head {
		out = append(out, kitLine([]kitSeg{{text: l, raw: true}}, "", c.w))
	}
	for i := c.top; i < c.top+c.height(); i++ {
		if i >= len(c.rows) {
			out = append(out, kitLine(nil, "", c.w))
			continue
		}
		r := c.rows[i]
		gutter := kitSeg{text: strings.Repeat(" ", convoGutter)}
		if r.msg >= 0 && r.msg == c.sel {
			gutter = kitSeg{style: th.ink, text: rail + " "}
			if i == c.msgs[r.msg].from {
				gutter = kitSeg{style: sgrBold + th.ink, text: mk.selected + " "}
			}
		}
		out = append(out, kitLine([]kitSeg{gutter, {text: r.text, raw: true}}, "", c.w))
	}
	out = append(out, kitLine(c.statusLine(th), "", c.w), kitLine(c.footer(th), "", c.w))
	return out[:min(len(out), c.h)]
}

func (c *convoScreen) draw(out io.Writer) error {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, row := range c.frame() {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(sgrReset + "\x1b[2K" + row)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// showConvo shows the conversation as it is now.
func showConvo(in *devTTY, tgt convoTarget, last int, stop <-chan struct{}) error {
	_, mod := statFile(tgt.path)
	s, err := loadConvo(tgt)
	if err != nil {
		return err
	}
	if !tgt.stale && s.Unfinished && time.Since(mod) < convoQuiet {
		holdLastTurn(s) // see convoFollow.quiet
	}
	c := newConvoScreen(false)
	c.apply(convoUpdate{s: s, t: tgt, begin: true, from: convoStart(s.Turns, last)})
	c.status = workingLabel(s, time.Now())
	return runConvoScreen(c, in, os.Stdout, nil, nil, stop)
}

// screen runs --follow on the screen: the loop finds what changed and the screen shows it.
func (f *convoFollow) screen(in *devTTY, stop <-chan struct{}) error {
	updates := make(chan convoUpdate)
	quit := make(chan struct{})
	defer close(quit)
	f.send = func(u convoUpdate) {
		select {
		case updates <- u:
		case <-quit:
		}
	}
	failed := make(chan error, 1)
	go func() { failed <- f.loop(quit) }()
	// The conversation is read before the screen takes the terminal, so an error shows as one.
	c := newConvoScreen(true)
	select {
	case u := <-updates:
		c.apply(u)
	case err := <-failed:
		return err
	case <-stop:
		return nil
	}
	return runConvoScreen(c, in, os.Stdout, updates, failed, stop)
}

// runConvoScreen shows c until q, Esc or Ctrl-C, a signal (stop), or the end of what feeds
// it (failed). updates brings what --follow finds; both are nil for a conversation as it is.
func runConvoScreen(c *convoScreen, in *devTTY, out *os.File, updates <-chan convoUpdate, failed <-chan error, stop <-chan struct{}) error {
	old, err := term.MakeRaw(in.fd)
	if err != nil {
		return err
	}
	defer func() {
		// Input mode first, even if the output no longer takes escapes.
		term.Restore(in.fd, old)
		out.WriteString(asksTerminalLeave)
	}()
	if _, err := out.WriteString(asksTerminalEnter + asksMouseEnter); err != nil {
		return err
	}
	size := func() {
		w, h, err := term.GetSize(int(out.Fd()))
		if err != nil || w < 1 || h < 1 {
			w, h = 80, 24
		}
		c.resize(w, h)
	}
	size()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	keys, stopKeys := readKeys(in.fd)
	defer stopKeys()
	var tokens asksTokenizer
	var escape, orphan <-chan time.Time
	handle := func(events []asksInput) bool {
		for _, e := range events {
			if c.input(e) {
				return true
			}
		}
		return false
	}
	for {
		if err := c.draw(out); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case err := <-failed:
			return err
		case <-winch:
			size()
		case u := <-updates:
			c.apply(u)
		case <-escape:
			escape = nil
			if handle(tokens.flush()) {
				return nil
			}
			orphan = time.After(orphanWait)
		case <-orphan:
			orphan = nil
			tokens.forget()
		case data, ok := <-keys:
			if !ok {
				return nil
			}
			if handle(tokens.feed(data)) {
				return nil
			}
			escape, orphan = nil, nil
			if tokens.waiting() {
				escape = time.After(escapeWait)
			}
		}
	}
}
