package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// lineBreaks are what a reader might take as the start of a new line.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", " ", "\n", " ", "\n")

// dropControls blanks control characters that could hide text or restyle a terminal.
func dropControls(r rune) rune {
	if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
		return ' '
	}
	return r
}

// terminalCodes matches the sequences that color or move text in a terminal, or set
// its title: CSI, also in its one-byte form, OSC, charset selection and single escapes.
var terminalCodes = regexp.MustCompile(`(?:\x1b\[|\x{9b})[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()*+][0-9A-Za-z]|\x1b[@-Z\\^_]`)

// Plain removes terminal codes, normalizes line breaks and blanks control characters.
func Plain(s string) string {
	return strings.Map(dropControls, lineBreaks.Replace(terminalCodes.ReplaceAllString(s, "")))
}

// dropHidden removes control and format characters, which a reader cannot see but which
// stop a rule matching across them. Line breaks and tabs stay.
func dropHidden(r rune) rune {
	if r != '\n' && r != '\t' && r != '\r' && (unicode.Is(unicode.Cf, r) || unicode.IsControl(r)) {
		return -1
	}
	return r
}

// Clean redacts s both as recorded and as plain text. Each pass catches what the
// other misses: a line break inside a quoted value splits it once normalized. What a
// reader cannot see goes first, because it would stop either pass matching.
func Clean(s string) string {
	s, _ = Text(strings.Map(dropHidden, terminalCodes.ReplaceAllString(s, "")))
	s, _ = Text(Plain(s))
	return s
}
