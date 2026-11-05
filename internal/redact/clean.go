package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// lineBreaks are what a reader might take as the start of a new line.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// dropControls blanks control characters.
func dropControls(r rune) rune {
	if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
		return ' '
	}
	return r
}

const (
	// Sequences that color or move text in a terminal or set its title: CSI, also in its
	// one-byte form, OSC, charset selection and single escapes.
	terminalCode = `(?:\x1b\[|\x{9b})[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()*+][0-9A-Za-z]|\x1b[@-Z\\^_]`

	// Characters with no visible mark: format and control characters except line breaks
	// and tabs, variation selectors and fillers.
	unmarked = `[\p{Cf}\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f\x{34F}\x{115F}\x{1160}\x{17B4}\x{17B5}\x{3164}\x{FE00}-\x{FE0F}\x{FFA0}\x{E0100}-\x{E01EF}]`
)

// gap is what can sit inside a value unseen: unmarked, less what lineBreaks reads as a
// line break.
const gap = `[\p{Cf}\x00-\x08\x0e-\x1f\x7f-\x84\x86-\x9f\x{34F}\x{115F}\x{1160}\x{17B4}\x{17B5}\x{3164}\x{FE00}-\x{FE0F}\x{FFA0}\x{E0100}-\x{E01EF}]`

var (
	terminalCodes = regexp.MustCompile(terminalCode)
	colorCodes    = regexp.MustCompile(`\x1b\[[0-9;:]*[mK]|\x1b\(B`)
	unmarkedChars = regexp.MustCompile(unmarked)
	words         = regexp.MustCompile(`\S+`)

	// afterPlaceholder is a placeholder, then what continues its value past a gap.
	afterPlaceholder = regexp.MustCompile(`(\[redacted [^\]]*\])(?:` + terminalCode + `|` + gap + `)(?:` + terminalCode + `|` + gap + `|[A-Za-z0-9_.~+/=-])*`)

	// beforePlaceholder is an opened code that a placeholder would otherwise complete.
	beforePlaceholder = regexp.MustCompile(`(?:(?:\x1b\[|\x{9b})[0-?]*[ -/]*|\x1b[()*+]?)(\[redacted )`)
)

// Plain removes terminal codes, normalizes line breaks and blanks control characters.
func Plain(s string) string {
	return normalize(terminalCodes.ReplaceAllString(s, ""))
}

// normalize makes every line break a newline and blanks control characters.
func normalize(s string) string {
	return strings.Map(dropControls, lineBreaks.Replace(s))
}

// dropFormat removes format characters from text meant for reading. The joiners stay,
// because some scripts and emoji need them.
func dropFormat(r rune) rune {
	if unicode.Is(unicode.Cf, r) && r != 0x200c && r != 0x200d {
		return -1
	}
	return r
}

// cleanPass runs the rules over s and adds to hits.
func cleanPass(s string, hits map[string]int) string {
	out, found := Text(s)
	for kind, n := range found {
		hits[kind] += n
	}
	return beforePlaceholder.ReplaceAllString(afterPlaceholder.ReplaceAllString(out, "$1"), "$1")
}

// joinWords redacts a word that a rule matches once its unmarked characters are gone.
// Any other word keeps them: they may separate a name from its value.
func joinWords(s string, hits map[string]int) string {
	return words.ReplaceAllStringFunc(s, func(word string) string {
		joined := unmarkedChars.ReplaceAllString(word, "")
		if joined == word {
			return word
		}
		out, found := Text(joined)
		if len(found) == 0 {
			return word
		}
		for kind, n := range found {
			hits[kind] += n
		}
		return out
	})
}

// CleanCount redacts s for a reader and says how many values of each kind it replaced.
// The passes go from the text as recorded to plain text, because each catches what the
// others miss: a line break inside a quoted value splits it once normalized.
func CleanCount(s string) (string, map[string]int) {
	hits := map[string]int{}
	s = cleanPass(colorCodes.ReplaceAllString(s, ""), hits)
	s = cleanPass(joinWords(terminalCodes.ReplaceAllString(s, ""), hits), hits)
	s = cleanPass(joinWords(normalize(s), hits), hits)
	s = cleanPass(strings.Map(dropFormat, s), hits)
	return s, hits
}

// Clean is CleanCount without the counts.
func Clean(s string) string {
	out, _ := CleanCount(s)
	return out
}
