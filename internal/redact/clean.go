package redact

import (
	"regexp"
	"strings"
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

// terminalColors matches the codes that color text, and the line clear grep writes
// beside them, which hide nothing by going.
var terminalColors = regexp.MustCompile(`\x1b\[[0-9;:]*[mK]`)

// Plain removes terminal codes, normalizes line breaks and blanks control characters.
func Plain(s string) string {
	return strings.Map(dropControls, lineBreaks.Replace(terminalCodes.ReplaceAllString(s, "")))
}

// Clean redacts s both as recorded and as plain text. Each pass catches what the
// other misses: a line break inside a quoted value splits it once normalized, and a
// control character between a key and its value hides the value until then. Colors go
// first, because a color in the middle of a secret would stop either pass matching.
func Clean(s string) string {
	s, _ = Text(terminalColors.ReplaceAllString(s, ""))
	s, _ = Text(Plain(s))
	return s
}
