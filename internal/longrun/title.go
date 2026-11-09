package longrun

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// maxTitle caps a title the takeover passes on, in characters.
const maxTitle = 200

// sessionTitle takes the current saved name before a pane is respawned, as
// cleanTitle leaves it.
func sessionTitle(l state.Lease, home string) string { return cleanTitle(savedTitle(l, home)) }

// cleanTitle makes a saved name safe to pass on as one argument: control
// characters become spaces (a NUL cannot be passed at all), runs of spaces
// close up, and it is cut to maxTitle characters.
func cleanTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	if r := []rune(title); len(r) > maxTitle {
		title = strings.TrimSpace(string(r[:maxTitle]))
	}
	return title
}

// savedTitle is the session's current saved name. Launch flags are only a
// fallback: /rename may have changed it since then.
func savedTitle(l state.Lease, home string) string {
	var title string
	switch l.Provider {
	case "claude":
		title = transcript.ClaudeTitle(l.Transcript)
	case "codex":
		if home != "" && l.SessionID != "" {
			title = transcript.CodexTitle(filepath.Join(home, "session_index.jsonl"), l.SessionID)
		}
	}
	if title != "" {
		return title
	}
	if l.Provider == "claude" || l.Provider == "copilot" {
		args := splitArgs(l.Args)
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--":
				return title
			case (args[i] == "--name" || args[i] == "-n") && i+1 < len(args):
				i++
				title = args[i]
			case strings.HasPrefix(args[i], "--name="):
				title = strings.TrimPrefix(args[i], "--name=")
			}
		}
	}
	return title
}
