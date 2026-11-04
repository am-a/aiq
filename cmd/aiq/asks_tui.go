package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

var asksSections = []string{"Asks", "Warnings", "Other open", "Probably done", "Closed and dismissed"}

type asksSnapshot struct {
	items, moved []state.Ask
	status       state.AsksStatus
	leases       []state.Lease
	workspaces   []string
	off          map[string]bool
	keyOK        bool
}

type asksRequest struct {
	action string
	id     int64
	to     state.AskState
	ws     string
	off    bool
	closed bool
}

type asksResult struct {
	snapshot  asksSnapshot
	err       error
	message   string
	undo      int64
	clearUndo bool
	goTo      *state.Lease
}

func asksWorkspace(l state.Lease) string {
	if l.Workspace != "" {
		return l.Workspace
	}
	return l.Cwd
}

// No transcript path or quoted text participates in navigation or session switches.
func asksLiveLeases(all []state.Lease) []state.Lease {
	var out []state.Lease
	for _, l := range all {
		if l.Mode == state.ModeLong && l.Hostname == hostname() && proc.Alive(l.PID) {
			out = append(out, l)
		}
	}
	return out
}

func asksLoad(reg, leases *state.Store, closed bool, now time.Time) (s asksSnapshot, err error) {
	states := []state.AskState{state.AskOpen, state.AskProbablyDone}
	if closed {
		states = append(states, state.AskClosed, state.AskDismissed)
	}
	if s.items, err = reg.ListAsks("", states...); err != nil {
		return s, err
	}
	if s.moved, err = reg.ListAsksMoved(asksDayStart(now)); err != nil {
		return s, err
	}
	if s.status, err = reg.GetAsksStatus(now); err != nil {
		return s, err
	}
	if s.off, err = reg.AskWorkspacesOff(); err != nil {
		return s, err
	}
	all, err := leases.ListLeases()
	if err != nil {
		return s, err
	}
	s.leases, s.keyOK = asksLiveLeases(all), asksKey() != ""
	s.workspaces, err = asksWorkspaceNames(leases)
	if err != nil {
		return s, err
	}
	return s, nil
}

func asksApply(reg, leases *state.Store, r asksRequest, now time.Time) asksResult {
	var out asksResult
	switch r.action {
	case "state":
		out.err = reg.SetAskByPerson(r.id, r.to, now.Unix())
		if out.err == nil {
			verb := "Dismissed"
			if r.to == state.AskOpen {
				verb = "Restored"
			}
			out.message, out.undo = fmt.Sprintf("%s #%d. u undo", verb, r.id), r.id
		}
	case "undo":
		_, out.err = reg.UndoAsk(r.id, now.Unix())
		if out.err == nil {
			out.message = fmt.Sprintf("Undid #%d", r.id)
			out.clearUndo = true
		}
	case "workspace":
		out.err = reg.SetAskWorkspaceOff(r.ws, r.off, now.Unix())
	case "go":
		var all []state.Lease
		all, out.err = leases.ListLeases()
		for _, l := range asksLiveLeases(all) {
			if asksWorkspace(l) == r.ws && l.Pane != "" {
				pick := l
				out.goTo = &pick
				break
			}
		}
		if out.err == nil && out.goTo == nil {
			out.message = "Session ended: no live local pane for this workspace"
		}
	}
	if out.err == nil {
		out.snapshot, out.err = asksLoad(reg, leases, r.closed, now)
	}
	return out
}

// asksWorker owns all store calls while the screen is up. A slow read cannot hold
// terminal input or signal restoration. Requests are serialized, including writes.
func asksWorker(reg, leases *state.Store, requests <-chan asksRequest, results chan<- asksResult, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case r := <-requests:
			result := asksApply(reg, leases, r, time.Now())
			select {
			case results <- result:
			case <-stop:
				return
			}
		}
	}
}

type asksRow struct {
	key, text, section, workspace string
	id                            int64
	action                        string
}

type asksScreen struct {
	snapshot   asksSnapshot
	rows       []asksRow
	expanded   map[int64]bool
	sections   map[string]bool
	selected   string
	top        int
	freeScroll bool
	w, h       int
	sessions   bool
	moved      bool
	loaded     bool
	busy       bool
	stale      bool
	message    string
	undo       int64
	noColor    bool
}

func newAsksScreen() *asksScreen {
	return &asksScreen{expanded: map[int64]bool{}, sections: map[string]bool{
		"Asks": true, "Warnings": true,
	}, w: 80, h: 24, noColor: os.Getenv("NO_COLOR") != ""}
}

func asksText(s string) string { return transcript.Clean(sanitize(s)) }

func asksGroups(items []state.Ask, now time.Time) map[string][]state.Ask {
	groups := map[string][]state.Ask{}
	for _, a := range items {
		name := "Closed and dismissed"
		switch a.State {
		case state.AskOpen:
			name = asksGroup(a, now.Unix())
		case state.AskProbablyDone:
			name = "Probably done"
		}
		groups[name] = append(groups[name], a)
	}
	return groups
}

func (b *asksScreen) current() asksRow {
	if b.selected == "moved-header" {
		return asksRow{key: "moved-header"}
	}
	for _, r := range b.rows {
		if r.key == b.selected {
			return r
		}
	}
	return asksRow{}
}

func (b *asksScreen) item(id int64) (state.Ask, bool) {
	for _, list := range [][]state.Ask{b.snapshot.items, b.snapshot.moved} {
		for _, a := range list {
			if a.ID == id {
				return a, true
			}
		}
	}
	return state.Ask{}, false
}

func (b *asksScreen) workspaceName(ws string) string {
	base := filepath.Base(ws)
	for _, list := range [][]state.Ask{b.snapshot.items, b.snapshot.moved} {
		for _, a := range list {
			if a.Workspace != ws && filepath.Base(a.Workspace) == base {
				return asksText(filepath.Join(filepath.Base(filepath.Dir(ws)), base))
			}
		}
	}
	for _, l := range b.snapshot.leases {
		other := asksWorkspace(l)
		if other != ws && filepath.Base(other) == base {
			return asksText(filepath.Join(filepath.Base(filepath.Dir(ws)), base))
		}
	}
	return asksText(base)
}

func (b *asksScreen) itemRows(a state.Ask, prefix string, now time.Time) []asksRow {
	key := fmt.Sprintf("%s:%d", prefix, a.ID)
	appendRow := func(out []asksRow, text, action string) []asksRow {
		return append(out, asksRow{key: key, text: text, id: a.ID, action: action})
	}
	live := "ended"
	for _, l := range b.snapshot.leases {
		if asksWorkspace(l) == a.Workspace {
			live = "live"
			break
		}
	}
	meta := fmt.Sprintf("#%d %s · %s · %s · %s · %s", a.ID, b.workspaceName(a.Workspace), asksText(a.Provider),
		fmtAge(max(now.Sub(time.Unix(a.RaisedAt, 0)), 0)), live, a.State)
	if a.Repeats > 0 {
		meta += fmt.Sprintf(" ×%d", a.Repeats+1)
	}
	var out []asksRow
	for _, text := range wrap(meta, max(b.w-2, 10)) {
		out = appendRow(out, " "+text, "")
	}
	quote := asksText(a.FirstQuote)
	if b.expanded[a.ID] {
		quote = asksText(a.LastQuote)
	} else {
		quote = asksCut(quote, max(b.w*2, 1))
	}
	quoteRows := wrap(quote, max(b.w-4, 10))
	if !b.expanded[a.ID] && len(quoteRows) > 2 {
		quoteRows = quoteRows[:2]
		quoteRows[1] = truncate(quoteRows[1], max(b.w-5, 1)) + "…"
	}
	for _, text := range quoteRows {
		out = appendRow(out, "   "+text, "")
	}
	if a.State != state.AskOpen {
		for _, text := range wrap("By "+asksText(a.StateReason)+": "+asksText(a.StateText), max(b.w-4, 10)) {
			out = appendRow(out, "   "+text, "")
		}
	}
	if b.expanded[a.ID] {
		for _, text := range wrap(asksText(a.Workspace)+" · "+asksText(a.SessionID), max(b.w-4, 10)) {
			out = appendRow(out, "   "+text, "")
		}
	}
	if key == b.selected {
		dismiss, restore := state.AskAction(a.State)
		if dismiss {
			out = appendRow(out, "   [dismiss] d", "dismiss")
		}
		if restore {
			out = appendRow(out, "   [restore] r", "restore")
		}
	}
	return out
}

func (b *asksScreen) layout(now time.Time) {
	oldIndex := 0
	for i, r := range b.rows {
		if r.key == b.selected {
			oldIndex = i
			break
		}
	}
	b.rows = nil
	if b.sessions {
		b.rows = append(b.rows, asksRow{key: "sessions", text: "Sessions · Enter toggles · s returns", section: "Sessions"})
		workspaces := append([]string(nil), b.snapshot.workspaces...)
		sort.Strings(workspaces)
		for _, ws := range workspaces {
			mode := "on"
			if b.snapshot.off[ws] {
				mode = "off"
			}
			for _, text := range wrap(mode+" · "+asksText(ws), max(b.w-2, 10)) {
				b.rows = append(b.rows, asksRow{key: "ws:" + ws, workspace: ws, text: " " + text, action: "workspace"})
			}
		}
		for i, why := range b.snapshot.status.NotWatched {
			for _, text := range wrap("Not watched: "+asksText(why), max(b.w-2, 10)) {
				b.rows = append(b.rows, asksRow{key: fmt.Sprintf("nw:%d", i), text: " " + text})
			}
		}
		if b.snapshot.status.LastError != "" {
			for _, text := range wrap("Last error: "+asksText(b.snapshot.status.LastError), max(b.w-2, 10)) {
				b.rows = append(b.rows, asksRow{key: "last-error", text: " " + text})
			}
		}
	} else {
		if b.moved {
			for _, a := range b.snapshot.moved {
				b.rows = append(b.rows, b.itemRows(a, "moved", now)...)
			}
		}
		groups := asksGroups(b.snapshot.items, now)
		for _, name := range asksSections {
			mark := "+"
			if b.sections[name] {
				mark = "-"
			}
			label := fmt.Sprintf("[%s] %s (%d)", mark, name, len(groups[name]))
			if name == "Closed and dismissed" && !b.sections[name] {
				label = "[+] Closed and dismissed (load on open)"
			}
			b.rows = append(b.rows, asksRow{key: "section:" + name, section: name, text: label})
			if b.sections[name] {
				for _, a := range groups[name] {
					b.rows = append(b.rows, b.itemRows(a, "ask", now)...)
				}
			}
		}
	}
	for i, r := range b.rows {
		if r.key == b.selected {
			b.top += i - oldIndex
			return
		}
	}
	if b.selected == "moved-header" {
		return
	}
	if len(b.rows) > 0 {
		b.selected = b.rows[min(oldIndex, len(b.rows)-1)].key
	}
}

// Header rows are fixed, so status and rule-driven moves remain visible when scrolled.
func (b *asksScreen) header(now time.Time) []line {
	open := 0
	for _, a := range b.snapshot.items {
		if a.State == state.AskOpen {
			open++
		}
	}
	status := asksStatusLine(b.snapshot.status, b.snapshot.keyOK, open, now)
	if !b.loaded {
		status = "Loading asks…"
	} else if b.stale {
		status = "Stale: " + status
	}
	closed, probably := 0, 0
	for _, a := range b.snapshot.moved {
		if a.State == state.AskClosed {
			closed++
		} else if a.State == state.AskProbablyDone {
			probably++
		}
	}
	mark := ""
	if b.selected == "moved-header" {
		mark = "> "
	}
	head := []line{{asksText(status), sgrBold}, {fmt.Sprintf("%sMoved today: %d closed, %d probably done", mark, closed, probably), sgrDim}}
	if b.message != "" {
		head = append(head, line{asksText(b.message), sgrBold})
	}
	if len(b.snapshot.status.NotWatched) > 0 || b.snapshot.status.Skipped > 0 {
		why := ""
		if len(b.snapshot.status.NotWatched) > 0 {
			why = ": " + asksText(b.snapshot.status.NotWatched[0])
		}
		head = append(head, line{fmt.Sprintf("%d long sessions not watched%s · %d texts skipped · s sessions", len(b.snapshot.status.NotWatched), why, b.snapshot.status.Skipped), sgrDim})
	}
	return head
}

func (b *asksScreen) draw(out *os.File, now time.Time) error {
	b.layout(now)
	head := b.header(now)
	height := max(b.h-len(head)-1, 1)
	sel := 0
	for i, r := range b.rows {
		if r.key == b.selected && b.selected != "moved-header" {
			sel = i
			break
		}
	}
	b.top = clamp(b.top, 0, max(len(b.rows)-height, 0))
	if !b.freeScroll && (sel < b.top || sel >= b.top+height) {
		b.top = clamp(sel, 0, max(len(b.rows)-height, 0))
	}
	rows := append([]line{}, head...)
	for i := b.top; i < len(b.rows) && len(rows) < b.h-1; i++ {
		r := b.rows[i]
		text, style := "  "+r.text, ""
		if r.key == b.selected {
			text, style = "> "+r.text, sgrReverse
		}
		rows = append(rows, line{text, style})
	}
	for len(rows) < b.h-1 {
		rows = append(rows, line{})
	}
	rows = append(rows, line{"j/k Tab d/r u g s m q · PgUp/PgDn", sgrDim})
	var text strings.Builder
	text.WriteString("\x1b[H")
	for i, r := range rows[:min(len(rows), b.h)] {
		if i > 0 {
			text.WriteString("\r\n")
		}
		style := r.style
		if b.noColor {
			style = ""
		}
		text.WriteString(style + truncate(asksText(r.text), b.w))
		if style != "" {
			text.WriteString(sgrReset)
		}
		text.WriteString("\x1b[K")
	}
	text.WriteString("\x1b[J")
	_, err := out.WriteString(text.String())
	return err
}

func (b *asksScreen) move(delta int) {
	b.freeScroll = false
	keys := []string{"moved-header"}
	if b.sessions {
		keys = nil
	}
	for _, r := range b.rows {
		if len(keys) == 0 || keys[len(keys)-1] != r.key {
			keys = append(keys, r.key)
		}
	}
	for i, key := range keys {
		if key == b.selected {
			b.selected = keys[clamp(i+delta, 0, len(keys)-1)]
			return
		}
	}
}

func (b *asksScreen) action(k keypress) (asksRequest, bool, bool) {
	r := b.current()
	request := asksRequest{closed: b.sections["Closed and dismissed"]}
	switch {
	case k.code == keyQuit || k.r == 'q':
		return request, false, true
	case k.code == keyUp:
		b.move(-1)
	case k.code == keyDown:
		b.move(1)
	case k.code == keyPgUp:
		b.top -= max(b.h-4, 1)
		b.freeScroll = true
	case k.code == keyPgDn:
		b.top += max(b.h-4, 1)
		b.freeScroll = true
	case k.r == '\t':
		b.freeScroll = false
		section := r.section
		if a, ok := b.item(r.id); ok {
			for name, list := range asksGroups([]state.Ask{a}, time.Now()) {
				if len(list) > 0 {
					section = name
				}
			}
		}
		for i, name := range asksSections {
			if section == name {
				b.selected = "section:" + asksSections[(i+1)%len(asksSections)]
				return request, false, false
			}
		}
		for _, row := range b.rows {
			if row.section != "" && row.key != b.selected {
				b.selected = row.key
				break
			}
		}
	case k.r == 's':
		b.sessions, b.selected, b.top = !b.sessions, "", 0
		b.freeScroll = false
	case k.code == keyEnter:
		if r.key == "moved-header" {
			b.moved = !b.moved
			return request, false, false
		}
		if r.workspace != "" {
			request.action, request.ws, request.off = "workspace", r.workspace, !b.snapshot.off[r.workspace]
			return request, true, false
		}
		if r.section != "" {
			b.sections[r.section] = !b.sections[r.section]
			request.closed = b.sections["Closed and dismissed"]
			return request, true, false
		}
		if r.id != 0 {
			b.expanded[r.id] = !b.expanded[r.id]
		}
	case k.r == 'u' && b.undo != 0:
		request.action, request.id = "undo", b.undo
		return request, true, false
	case k.r == 'd' || k.r == 'r' || k.r == 'g':
		if a, ok := b.item(r.id); ok {
			request.id = a.ID
			dismiss, restore := state.AskAction(a.State)
			if k.r == 'g' {
				request.action, request.ws = "go", a.Workspace
			} else if k.r == 'd' && dismiss {
				request.action, request.to = "state", state.AskDismissed
			} else if k.r == 'r' && restore {
				request.action, request.to = "state", state.AskOpen
			}
			return request, request.action != "", false
		}
	}
	return request, false, false
}

type asksInput struct {
	key          keypress
	x, y, button int
	mouse        bool
}

type asksTokenizer struct{ pending []byte }

// Buffer partial CSI/SGR sequences. Legacy mouse reports are consumed and ignored,
// including their payload bytes, which otherwise could become dismiss/quit keys.
func (p *asksTokenizer) feed(buf []byte) []asksInput {
	p.pending = append(p.pending, buf...)
	var out []asksInput
	for len(p.pending) > 0 {
		b := p.pending
		if b[0] == 0x1b {
			if len(b) < 2 {
				break
			}
			if b[1] == '[' || b[1] == 'O' {
				if len(b) >= 3 && b[1] == '[' && b[2] == 'M' {
					if len(b) < 6 {
						break
					}
					p.pending = b[6:]
					continue
				}
				i := 2
				sgrMouse := len(b) >= 3 && b[1] == '[' && b[2] == '<'
				if sgrMouse {
					for i < len(b) && b[i] != 'M' && b[i] != 'm' {
						if b[i] == 3 || b[i] == 4 {
							p.pending = nil
							return append(out, asksInput{key: keypress{code: keyQuit}})
						}
						i++
					}
				} else {
					for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
						i++
					}
				}
				if i == len(b) {
					if len(b) > 128 {
						p.pending = nil
					}
					break
				}
				seq := b[:i+1]
				if sgrMouse {
					parts := strings.Split(string(b[3:i]), ";")
					if len(parts) == 3 && b[i] == 'M' {
						button, e1 := strconv.Atoi(parts[0])
						x, e2 := strconv.Atoi(parts[1])
						y, e3 := strconv.Atoi(parts[2])
						if e1 == nil && e2 == nil && e3 == nil && x > 0 && y > 0 && (button == 0 || button == 64 || button == 65) {
							out = append(out, asksInput{mouse: true, button: button, x: x, y: y})
						}
					}
				} else {
					for _, k := range parseKeys(seq) {
						out = append(out, asksInput{key: k})
					}
				}
				p.pending = b[i+1:]
				continue
			}
			p.pending = b[2:]
			continue
		}
		if !utf8.FullRune(b) {
			break
		}
		_, n := utf8.DecodeRune(b)
		for _, k := range parseKeys(b[:n]) {
			// The resume browser uses g for Home. Here it is go-to-session.
			if b[0] == 'g' {
				k = keypress{r: 'g'}
			}
			out = append(out, asksInput{key: k})
		}
		p.pending = b[n:]
	}
	return out
}

func (b *asksScreen) click(e asksInput, now time.Time) (asksRequest, bool) {
	if e.button == 64 || e.button == 65 {
		b.top += map[bool]int{true: -3, false: 3}[e.button == 64]
		b.freeScroll = true
		return asksRequest{}, false
	}
	if e.x > b.w || e.y > b.h || e.y < 1 {
		return asksRequest{}, false
	}
	if e.y == 2 {
		b.moved = !b.moved
		return asksRequest{}, false
	}
	idx := b.top + e.y - len(b.header(now)) - 1
	if idx < 0 || idx >= len(b.rows) || e.y == b.h {
		return asksRequest{}, false
	}
	r := b.rows[idx]
	if b.selected != r.key {
		b.selected = r.key
		b.freeScroll = false
		return asksRequest{}, false
	}
	k := keypress{code: keyEnter}
	if r.action == "dismiss" && e.x >= 6 && e.x <= 14 {
		k = keypress{r: 'd'}
	} else if r.action == "restore" && e.x >= 6 && e.x <= 14 {
		k = keypress{r: 'r'}
	}
	request, submit, _ := b.action(k)
	return request, submit
}

const asksTerminalEnter = "\x1b[?1049h\x1b[?25l"
const asksMouseEnter = "\x1b[?1000h\x1b[?1006h"
const asksTerminalLeave = "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l"

func runAsksTUI(reg, leases *state.Store) error {
	pick, err := runAsksScreen(reg, leases, os.Stdin, os.Stdout, !hasFlag(os.Args[1:], "--no-mouse"))
	if err != nil || pick == nil {
		return err
	}
	info, err := tmux.PaneInfo(pick.Pane)
	if err != nil || info.Dead {
		fmt.Fprintln(os.Stdout, "Session ended: the pane is no longer available")
		return nil
	}
	if os.Getenv("TMUX") == "" {
		fmt.Fprintln(os.Stdout, tmux.Quote([]string{"tmux", "attach-session", "-t", "=" + info.Session, ";", "select-pane", "-t", pick.Pane}))
		return nil
	}
	if err := tmux.SelectPane(pick.Pane); err != nil {
		return err
	}
	return tmux.Attach(info.Session)
}

func runAsksScreen(reg, leases *state.Store, in, out *os.File, mouse bool) (*state.Lease, error) {
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	var once sync.Once
	restored := make(chan struct{})
	restore := func() {
		once.Do(func() {
			go func() {
				defer close(restored)
				// Restore input mode first, even if a blocked output cannot accept escapes.
				term.Restore(int(in.Fd()), old)
				out.WriteString(asksTerminalLeave)
			}()
		})
		select {
		case <-restored:
		case <-time.After(time.Second):
		}
	}
	defer restore()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	shutdown, signalDone := make(chan struct{}), make(chan struct{})
	defer close(signalDone)
	go func() {
		select {
		case <-signals:
			close(shutdown)
			restore()
		case <-signalDone:
		}
	}()
	if _, err := out.WriteString(asksTerminalEnter); err != nil {
		return nil, err
	}
	if mouse {
		if _, err := out.WriteString(asksMouseEnter); err != nil {
			return nil, err
		}
	}
	b := newAsksScreen()
	resize := func() {
		b.w, b.h, err = term.GetSize(int(out.Fd()))
		if err != nil || b.w < 1 || b.h < 1 {
			b.w, b.h = 80, 24
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	keys, stopKeys := readKeys(int(in.Fd()))
	defer stopKeys()
	requests, results := make(chan asksRequest, 1), make(chan asksResult, 1)
	workerStop := make(chan struct{})
	defer close(workerStop)
	go asksWorker(reg, leases, requests, results, workerStop)
	submit := func(r asksRequest) {
		if b.busy {
			b.message = "Refreshing; try again"
			return
		}
		if b.stale && r.action != "" && r.action != "go" {
			b.message = "Store unavailable; waiting for refresh"
			return
		}
		b.busy = true
		requests <- r
	}
	submit(asksRequest{})
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var tokenizer asksTokenizer
	dirty := true
	ageStamp := ""
	for {
		if dirty {
			if err := b.draw(out, time.Now()); err != nil {
				return nil, err
			}
			dirty = false
		}
		select {
		case <-shutdown:
			return nil, nil
		case <-winch:
			resize()
			dirty = true
		case <-tick.C:
			now := time.Now()
			stamp := asksStatusLine(b.snapshot.status, b.snapshot.keyOK, len(b.snapshot.items), now)
			for _, a := range b.snapshot.items {
				stamp += fmtAge(max(now.Sub(time.Unix(a.RaisedAt, 0)), 0)) + ";"
			}
			if stamp != ageStamp {
				ageStamp, dirty = stamp, true
			}
			if !b.busy {
				submit(asksRequest{closed: b.sections["Closed and dismissed"]})
			}
		case result := <-results:
			wasLoaded := b.loaded
			b.busy, b.loaded = false, true
			if !wasLoaded {
				dirty = true
			}
			if result.message != "" {
				b.message, dirty = result.message, true
			}
			if result.undo != 0 || result.clearUndo {
				b.undo = result.undo
			}
			if result.err != nil {
				b.message, b.stale, dirty = "Store error: "+asksText(result.err.Error())+" "+result.message, true, true
				continue
			}
			b.stale = false
			if !reflect.DeepEqual(b.snapshot, result.snapshot) {
				b.snapshot = result.snapshot
				dirty = true
			}
			if result.message == "" && (strings.HasPrefix(b.message, "Store error:") || strings.HasPrefix(b.message, "Store unavailable;")) {
				b.message = ""
				dirty = true
			}
			if result.goTo != nil {
				return result.goTo, nil
			}
		case data, ok := <-keys:
			if !ok {
				return nil, nil
			}
			for _, e := range tokenizer.feed(data) {
				dirty = true
				if e.mouse {
					if mouse {
						r, send := b.click(e, time.Now())
						if send {
							submit(r)
						}
					}
				} else {
					if e.key.r == 'm' {
						b.moved = !b.moved
						continue
					}
					r, send, quit := b.action(e.key)
					if quit {
						return nil, nil
					}
					if send {
						submit(r)
					}
				}
				b.layout(time.Now())
			}
		}
	}
}
