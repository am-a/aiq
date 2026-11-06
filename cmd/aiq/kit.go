package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The style kit for aiq's screens. Every colour is a role, so a screen says what a thing is
// and the theme says how it looks. Colour is never the only signal: selection has a glyph,
// a warning has a word, and a section has a bar.

// kitTheme is the roles for one background. Without colour only bold and dim remain.
type kitTheme struct {
	ink, agent, accent, warn, dim string // foregrounds
	tab                           string // the one reverse band, for the screen and scope
	itemBand, selBand, warnBand   string // backgrounds
}

var (
	kitLight = kitTheme{
		ink: "\x1b[38;5;25m", agent: "\x1b[38;5;22m", accent: "\x1b[38;5;94m", warn: "\x1b[38;5;160m", dim: "\x1b[38;5;242m",
		tab:      "\x1b[1;7;38;5;25m",
		itemBand: "\x1b[48;5;254m", selBand: "\x1b[48;5;252m", warnBand: "\x1b[48;5;224m",
	}
	kitDark = kitTheme{
		ink: "\x1b[38;5;110m", agent: "\x1b[38;5;114m", accent: "\x1b[38;5;180m", warn: "\x1b[38;5;203m", dim: "\x1b[38;5;246m",
		tab:      "\x1b[1;7;38;5;110m",
		itemBand: "\x1b[48;5;235m", selBand: "\x1b[48;5;238m", warnBand: "\x1b[48;5;52m",
	}
	kitMono = kitTheme{warn: sgrBold, dim: sgrDim, tab: "\x1b[1;7m"}
	// kitMonoPalette is what the Markdown renderer gets without colour.
	kitMonoPalette = convoPalette{you: sgrBold, agent: sgrBold, dim: sgrDim}
)

// termBackground is what the terminal reported as its background, when it did.
var termBackground struct {
	r, g, b float64 // 0 to 1
	ok      bool
}

// kitThemeFor settles the roles from the palette in use. Where the terminal takes 24-bit colour
// and reported its background, the bands are tinted from that background, so they sit in any
// theme. Otherwise they are the 256-colour greys.
func kitThemeFor(noColor bool) kitTheme {
	if noColor {
		return kitMono
	}
	th := kitDark
	if pal == lightPalette {
		th = kitLight
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		if termBackground.ok {
			f1, f2 := 0.08, 0.16 // a dark background gets lighter
			if pal == lightPalette {
				f1, f2 = -0.05, -0.11 // a light one gets darker
			}
			th.itemBand = kitTint(f1)
			th.selBand = kitTint(f2)
		}
	}
	return th
}

// kitTint moves the terminal's background toward white (f > 0) or black (f < 0).
func kitTint(f float64) string {
	mix := func(c float64) int {
		if f < 0 {
			return int(255 * c * (1 + f))
		}
		return int(255 * (c + (1-c)*f))
	}
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", mix(termBackground.r), mix(termBackground.g), mix(termBackground.b))
}

// kitMarks are the glyphs. Each works without a Nerd Font, and each has a plain fallback for
// a terminal that is not UTF-8.
type kitMarks struct {
	selected, live, ended, closed, bar, dot string
}

var (
	kitUnicode = kitMarks{selected: "▶", live: "●", ended: "○", closed: "▸", bar: "━", dot: "·"}
	kitASCII   = kitMarks{selected: ">", live: "*", ended: "o", closed: "+", bar: "=", dot: "-"}
)

// kitMarksFor falls back only for a locale that says it is not UTF-8. No locale at all is
// common over SSH under a terminal that handles UTF-8 well.
func kitMarksFor(getenv func(string) string) kitMarks {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(name); v != "" {
			v = strings.ToLower(v)
			if strings.Contains(v, "utf-8") || strings.Contains(v, "utf8") {
				return kitUnicode
			}
			return kitASCII
		}
	}
	return kitUnicode
}

// kitSeg is a run of one style. A raw segment is already rendered and only its width counts;
// everything else is cleaned on the way out, so nothing in it can carry an escape.
type kitSeg struct {
	style, text string
	raw         bool
}

var kitSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

func kitPlain(s string) string { return kitSGR.ReplaceAllString(s, "") }

// kitText is the segments as plain text.
func kitText(segs []kitSeg) string {
	var b strings.Builder
	for _, s := range segs {
		if s.raw {
			b.WriteString(kitPlain(s.text))
		} else {
			b.WriteString(s.text)
		}
	}
	return b.String()
}

// kitLine fills exactly w cells, so bands run to the edge and a resize is only a redraw.
func kitLine(segs []kitSeg, bg string, w int) string {
	var b strings.Builder
	b.WriteString(sgrReset + bg)
	used := 0
	for _, s := range segs {
		left := w - used
		if left <= 0 {
			break
		}
		if s.raw {
			if n := cells(kitPlain(s.text)); n <= left {
				b.WriteString(s.text)
				used += n
			} else {
				t := truncate(kitPlain(s.text), left)
				b.WriteString(t)
				used += cells(t)
			}
			b.WriteString(sgrReset + bg)
			continue
		}
		t := truncate(strings.NewReplacer("\n", " ", "\t", " ").Replace(sanitize(s.text)), left)
		used += cells(t)
		if s.style == "" {
			b.WriteString(t)
			continue
		}
		b.WriteString(s.style + t + sgrReset + bg)
	}
	if used < w {
		b.WriteString(strings.Repeat(" ", w-used))
	}
	b.WriteString(sgrReset)
	return b.String()
}

// kitBar is a section bar: a label, then the heavy line to the edge.
func kitBar(label, style, bar string, w int) []kitSeg {
	return []kitSeg{{style: style, text: label}, {style: style, text: strings.Repeat(bar, max(w-cells(label), 0))}}
}

// asksHit is a clickable stretch of a row, in zero-based cells. r 0 means Enter.
type asksHit struct {
	from, to int
	r        rune
}

// kitKey is one key hint.
type kitKey struct {
	key, label string
	r          rune // what a click sends; 0 means Enter
}

// kitKeys lays out hints as a bold key and a dim label, two spaces apart. It returns the
// segments and where each hint sits, counted in cells from start.
func kitKeys(keys []kitKey, dim string, start int) ([]kitSeg, []asksHit) {
	var segs []kitSeg
	var hits []asksHit
	x := start
	for i, k := range keys {
		label := " " + k.label
		if i < len(keys)-1 {
			label += "  "
		}
		segs = append(segs, kitSeg{style: sgrBold, text: k.key}, kitSeg{style: dim, text: label})
		n := cells(k.key) + cells(" "+k.label)
		hits = append(hits, asksHit{from: x, to: x + n, r: k.r})
		x += cells(k.key) + cells(label)
	}
	return segs, hits
}
