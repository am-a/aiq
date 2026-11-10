package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/tmux"
)

// convoPalette is every colour aiq convo uses, chosen once for the
// terminal's background.
type convoPalette struct {
	band   string // background behind what the person typed
	you    string // header of what the person typed, on the band
	agent  string // header of an answer
	dim    string // earlier answers, notices, table borders, URLs, the status line
	accent string // the working line and the "moved to" divider
	code   string // inline code and code blocks
}

var (
	// darkPalette is for a dark background, and the default.
	darkPalette = convoPalette{band: sgrBand, you: sgrBoldCyan, agent: sgrBoldGreen, dim: sgrDim, accent: sgrYellow, code: sgrCode}
	// lightPalette keeps text at 4.5:1 or better against white (the you
	// header against its grey band: 5.1:1), where bright cyan, yellow
	// and green wash out.
	lightPalette = convoPalette{
		band:   "\x1b[48;5;254m",
		you:    "\x1b[1;38;5;25m",
		agent:  "\x1b[1;38;5;22m",
		dim:    "\x1b[38;5;242m",
		accent: "\x1b[38;5;94m",
		code:   "\x1b[38;5;130m",
	}
	// pal is the palette in use; cmdConvo picks it before anything prints.
	pal = darkPalette
)

// themeChoice is --theme (already checked), else AIQ_THEME, else auto. A
// mistyped AIQ_THEME is reported once on warn and read as auto.
func themeChoice(flag, env string, warn io.Writer) string {
	if flag != "" {
		return flag
	}
	switch v := strings.ToLower(strings.TrimSpace(env)); v {
	case "", "auto", "light", "dark":
		return v
	}
	fmt.Fprintf(warn, "aiq: ignoring AIQ_THEME=%s; use auto, light or dark\n", env)
	return ""
}

// choosePalette settles the theme: light or dark as asked, else what the
// terminal says its background is (OSC 11), else the theme tmux learned
// from the terminals showing pane, else $COLORFGBG, else dark.
func choosePalette(choice, pane string, stop <-chan struct{}) convoPalette {
	light := false
	switch choice {
	case "light":
		light = true
	case "dark":
	default:
		light = detectLight(pane, stop)
	}
	if light {
		return lightPalette
	}
	return darkPalette
}

func detectLight(pane string, stop <-chan struct{}) bool {
	// The query talks to /dev/tty, so where stdin points does not matter;
	// it gives up when there is no controlling terminal.
	if term.IsTerminal(int(os.Stdout.Fd())) {
		if light, ok := queryBackground(stop); ok {
			return light
		}
	}
	// tmux does not answer OSC 11 in a popup, but knows the theme its
	// clients' terminals reported. Only the clients of the pane's own
	// session count; without a pane there is no telling which they are.
	if pane == "" {
		pane = os.Getenv("TMUX_PANE")
	}
	if pane != "" {
		if light, ok := clientTheme(tmux.ClientThemes, pane, os.Getenv("AIQ_TMUX_CLIENT")); ok {
			return light
		}
	}
	light, _ := colorFGBG(os.Getenv("COLORFGBG"))
	return light
}

// clientTheme is the theme of the clients showing pane: the one named
// prefer when it reported one, else the first that did.
func clientTheme(list func(target string) ([]tmux.ClientTheme, error), pane, prefer string) (light, ok bool) {
	clients, err := list(pane)
	if err != nil {
		return false, false
	}
	theme := ""
	for _, c := range clients {
		if c.Theme == "" {
			continue
		}
		if theme == "" {
			theme = c.Theme
		}
		if prefer != "" && c.Client == prefer {
			theme = c.Theme
			break
		}
	}
	switch theme {
	case "light":
		return true, true
	case "dark":
		return false, true
	}
	return false, false
}

// colorFGBG reads $COLORFGBG ("fg;bg", some terminals "fg;default;bg"): a
// background of 7 or 15 is light, 0 to 6 or 8 dark; anything else says
// nothing.
func colorFGBG(v string) (light, ok bool) {
	f := strings.Split(v, ";")
	n, err := strconv.Atoi(f[len(f)-1])
	switch {
	case err != nil:
		return false, false
	case n == 7 || n == 15:
		return true, true
	case n >= 0 && n <= 6 || n == 8:
		return false, true
	}
	return false, false
}

// da1Reply is a Primary Device Attributes answer: ESC [ ? params c.
var da1Reply = regexp.MustCompile("\x1b\\[\\?[0-9;]*c")

// readUntilDA1 collects input until the DA1 reply is in, stop closes, or
// limit passes; then it takes whatever else is already waiting. It returns
// what came before the DA1 reply (or all of it, without one).
func readUntilDA1(read func(wait time.Duration) []byte, stop <-chan struct{}, limit time.Duration) []byte {
	var got []byte
	deadline := time.Now().Add(limit)
	for {
		if loc := da1Reply.FindIndex(got); loc != nil {
			return got[:loc[0]]
		}
		select {
		case <-stop:
			return got
		default:
		}
		left := time.Until(deadline)
		if left <= 0 {
			for b := read(0); len(b) > 0; b = read(0) {
				got = append(got, b...) // drain what turned up
			}
			return got
		}
		got = append(got, read(min(left, 50*time.Millisecond))...)
	}
}

// parseOSC11 reads "ESC ] 11 ; rgb:RRRR/GGGG/BBBB" ended by BEL or ST, with
// one to four hex digits a component (rgba: carries an alpha, ignored).
func parseOSC11(b []byte) (r, g, bl float64, ok bool) {
	s := string(b)
	i := strings.Index(s, "\x1b]11;")
	if i < 0 {
		return 0, 0, 0, false
	}
	s = s[i+5:]
	end := strings.IndexAny(s, "\a\x1b")
	if end < 0 || s[end] == '\x1b' && !strings.HasPrefix(s[end:], "\x1b\\") {
		return 0, 0, 0, false
	}
	s = s[:end]
	var body string
	switch {
	case strings.HasPrefix(s, "rgb:"):
		body = s[4:]
	case strings.HasPrefix(s, "rgba:"):
		body = s[5:]
	default:
		return 0, 0, 0, false
	}
	parts := strings.Split(body, "/")
	if len(parts) < 3 {
		return 0, 0, 0, false
	}
	var c [3]float64
	for k := 0; k < 3; k++ {
		p := parts[k]
		v, err := strconv.ParseUint(p, 16, 16)
		if err != nil || len(p) < 1 || len(p) > 4 {
			return 0, 0, 0, false
		}
		c[k] = float64(v) / float64(uint64(1)<<(4*len(p))-1)
	}
	return c[0], c[1], c[2], true
}

// luminance weighs the (gamma-encoded) components the way the eye does;
// a middle grey comes out at 0.5.
func luminance(r, g, b float64) float64 {
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// withMutedInput runs body and restores the terminal however body ends: a
// return, an error, or a panic (deferred calls run while a panic unwinds,
// so the panic keeps its stack). SIGINT, SIGTERM, SIGHUP and SIGQUIT end
// body through its stop channel, so they come here too.
func withMutedInput(m *mutedInput, body func() error) error {
	if m != nil {
		defer m.restore()
	}
	return body()
}
