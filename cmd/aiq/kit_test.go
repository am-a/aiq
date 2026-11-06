package main

import (
	"strings"
	"testing"
)

func TestKitLineFillsTheWidth(t *testing.T) {
	for _, w := range []int{1, 10, 45, 200} {
		got := kitLine([]kitSeg{{style: sgrBold, text: "aa\x1b[2Jbb"}, {text: "\x1b[1mcc\x1b[0m", raw: true}}, "\x1b[48;5;254m", w)
		if strings.Contains(got, "\x1b[2J") {
			t.Fatalf("an escape in plain text got through: %q", got)
		}
		if n := cells(kitPlain(got)); n != w {
			t.Fatalf("width %d: %d cells in %q", w, n, got)
		}
	}
}

func TestKitMarksFallBack(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "▶"},
		{map[string]string{"LANG": "en_CA.UTF-8"}, "▶"},
		{map[string]string{"LC_ALL": "C", "LANG": "en_CA.UTF-8"}, ">"},
		{map[string]string{"LC_CTYPE": "en_US.utf8", "LANG": "C"}, "▶"},
		{map[string]string{"LANG": "C"}, ">"},
	}
	for _, c := range cases {
		if got := kitMarksFor(func(k string) string { return c.env[k] }).selected; got != c.want {
			t.Fatalf("%v: %q", c.env, got)
		}
	}
}

func TestKitTintFollowsTheBackground(t *testing.T) {
	saved, savedBg := pal, termBackground
	defer func() { pal, termBackground = saved, savedBg }()
	t.Setenv("COLORTERM", "truecolor")
	pal = lightPalette
	termBackground.r, termBackground.g, termBackground.b, termBackground.ok = 0.99, 0.96, 0.89, true
	if th := kitThemeFor(false); !strings.HasPrefix(th.itemBand, "\x1b[48;2;") || th.itemBand == th.selBand {
		t.Fatalf("%+v", th)
	}
	t.Setenv("COLORTERM", "")
	if th := kitThemeFor(false); th.itemBand != kitLight.itemBand {
		t.Fatalf("without 24-bit colour the bands are the 256 greys: %q", th.itemBand)
	}
	termBackground.ok = false
	t.Setenv("COLORTERM", "truecolor")
	if th := kitThemeFor(false); th.itemBand != kitLight.itemBand {
		t.Fatalf("with no background reported the bands are the 256 greys: %q", th.itemBand)
	}
	if th := kitThemeFor(true); th.itemBand != "" || th.ink != "" {
		t.Fatalf("no colour: %+v", th)
	}
}

func TestAsksCodeAndTablesKeepTheirSpacing(t *testing.T) {
	rows := renderMarkdownRows("```\n  indented code\n```\n\n| a | b |\n|---|---|\n| one | two |", 40, "")
	joined := kitPlain(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "  indented code") || !strings.Contains(joined, "one") {
		t.Fatalf("%q", joined)
	}
	if !strings.Contains(joined, "┌") {
		t.Fatalf("a table should be drawn: %q", joined)
	}
	for _, row := range renderMarkdownRows(strings.Repeat("word ", 40)+strings.Repeat("x", 90), 30, "") {
		if n := cells(kitPlain(row)); n > 30 {
			t.Fatalf("row of %d cells: %q", n, row)
		}
	}
}

// Dismissing or restoring an item keeps the selection where it was: on the next item down,
// else the one above.
