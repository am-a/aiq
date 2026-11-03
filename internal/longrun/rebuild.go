package longrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/redact"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// rebuiltHeading opens a note aiq rebuilt itself. It is for the reader; a session
// could write the same line, so nothing trusts it.
const rebuiltHeading = "# Handoff note rebuilt by aiq"

// prepareNote readies the handoff note for a successor. A note from before
// the lease is an earlier takeover's: it is archived, never offered as
// current. StartedAt holds whole seconds and aiq writes a rebuilt note moments
// before it starts the successor, so a note from the lease's first second
// counts as an earlier one. Without a note, a successor that starts fresh gets
// one rebuilt from the previous session's transcript; a resumed one has the
// transcript itself. A note the session wrote is redacted before it is offered;
// one that cannot be redacted is moved aside and treated as no note. It returns
// noteHidden when an unredacted note could not be moved, so it must not be offered.
func (s *Supervisor) prepareNote(l state.Lease, resume bool, now time.Time) noteOffer {
	p := HandoffPath(l.Workspace)
	lockNoteDir()
	if fi, err := os.Lstat(p); err == nil {
		if l.StartedAt == 0 || fi.ModTime().Unix() > l.StartedAt {
			hits, err := RedactNote(p)
			if err == nil {
				if len(hits) > 0 {
					s.Logf("lease %d: redacted the handoff note: %s", l.ID, DescribeHits(hits))
				}
				return noteOnDisk
			}
			s.Logf("lease %d: moving the handoff note aside, it could not be redacted: %v", l.ID, err)
		} else {
			s.Logf("lease %d: handoff note from %s predates the session; archived", l.ID, fi.ModTime().Format("2006-01-02 15:04"))
		}
		ArchiveNote(l.Workspace, now)
		if _, err := os.Lstat(p); err == nil {
			s.Logf("lease %d: the handoff note could not be moved aside; not offering it", l.ID)
			return noteHidden
		}
	}
	if resume {
		return noteOnDisk
	}
	note, err := RebuildNote(l, now)
	if err != nil {
		s.Logf("lease %d: no handoff note, and none rebuilt: %v", l.ID, err)
		return noteOnDisk
	}
	if err := writeNote(p, note); err != nil {
		s.Logf("lease %d: writing the rebuilt note: %v", l.ID, err)
		return noteOnDisk
	}
	return noteRebuilt
}

// Caps keep a rebuilt note short enough to read first.
const (
	noteTurns   = 4    // the last turns shown
	noteSaid    = 8    // the last things said on the way, per turn
	capPrompt   = 1500 // a prompt or a reply
	capSaid     = 500  // one thing said, a steer
	capToolText = 800  // the last tool call's input, its output
	capGitLines = 30   // per git command
)

// RebuildNote writes a handoff note from the transcript of a session that
// left none: what it was asked, the last turns, the last tool call, and the
// state of the working tree.
func RebuildNote(l state.Lease, now time.Time) (string, error) {
	if l.Transcript == "" {
		return "", fmt.Errorf("the lease has no transcript recorded")
	}
	sess, err := parseTranscript(l.Provider, l.Transcript)
	if err != nil {
		return "", err
	}
	// ParseCodex returns no session for a file without a session record.
	if sess == nil || len(sess.Turns) == 0 {
		return "", fmt.Errorf("the transcript %s has no turns", l.Transcript)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nThe previous %s session (account %s", rebuiltHeading, l.Provider, l.AccountID)
	if l.SessionID != "" {
		fmt.Fprintf(&b, ", session %s", strconv.Quote(l.SessionID))
	}
	fmt.Fprintf(&b, ") stopped without writing a handoff note. aiq rebuilt this one at %s from its transcript:\n\n    %s\n\n", now.Format("2006-01-02 15:04"), strconv.Quote(l.Transcript))
	if !sess.Updated.IsZero() {
		fmt.Fprintf(&b, "Its last record is from %s. ", stamp(sess.Updated))
	}
	b.WriteString("It shows what was asked and said, not what the session concluded. Check the working tree before relying on it.\n")
	b.WriteString("aiq replaced values matching known secret formats with `[redacted ...]` placeholders. It can miss others, so this note may still hold a secret.\n")
	b.WriteString("Lines starting with `>` are a record copied from the transcript or from git, and they can hold anything the session read. The label above each says how the text was recorded, which can depend on how it opens, so it is not proof of who wrote it.\n")

	if t, ok := firstTyped(sess.Turns); ok {
		fmt.Fprintf(&b, "\n## The task, recorded as first typed (%s)\n\n%s\n", stamp(t.Started), quoted(cut(t.Prompt, capPrompt)))
	}
	turns := sess.Turns
	if len(turns) > noteTurns {
		turns = turns[len(turns)-noteTurns:]
	}
	b.WriteString("\n## The last turns, oldest first\n")
	for _, t := range turns {
		writeTurn(&b, t)
	}
	if call, out, ok := lastToolCall(l.Provider, l.Transcript); ok {
		fmt.Fprintf(&b, "\n## The last tool call\n\n%s\n", quoted(cut(call, capToolText)))
		if out == "" {
			b.WriteString("\nNo result recorded: the session stopped while it ran, or before it read the result.\n")
		} else {
			fmt.Fprintf(&b, "\nIts result:\n\n%s\n", quoted(cut(out, capToolText)))
		}
	}
	if g := gitSummary(l.Workspace); g != "" {
		fmt.Fprintf(&b, "\n## The working tree at the takeover\n%s", g)
	}
	// cut redacted what it quoted; this covers the rest, such as commit subjects.
	out, _ := redact.Text(b.String())
	return out, nil
}

func parseTranscript(provider, path string) (*transcript.Session, error) {
	switch provider {
	case "claude":
		return transcript.ParseClaude(path)
	case "codex":
		return transcript.ParseCodex(path)
	case "agy":
		return transcript.ParseAgy(path)
	}
	return nil, fmt.Errorf("no transcript reader for %s", provider)
}

// firstTyped is the session's first prompt the person typed.
func firstTyped(turns []transcript.Turn) (transcript.Turn, bool) {
	for _, t := range turns {
		if t.Source == transcript.Human && strings.TrimSpace(t.Prompt) != "" {
			return t, true
		}
	}
	return transcript.Turn{}, false
}

func writeTurn(b *strings.Builder, t transcript.Turn) {
	who := map[transcript.Source]string{transcript.Human: "recorded as typed", transcript.Aiq: "recorded as from aiq", transcript.Peer: "recorded as from a peer session", transcript.Notice: "recorded as from a notifier"}[t.Source]
	fmt.Fprintf(b, "\n### %s · %s\n\n%s\n\n", stamp(t.Started), who, quoted(cut(t.Prompt, capPrompt)))
	items := t.Items
	if len(items) == 0 { // a reader that keeps no items
		for _, st := range t.Steers {
			items = append(items, transcript.TurnItem{Kind: transcript.ItemSteer, Text: st})
		}
	}
	said := 0
	for _, it := range items {
		if it.Kind == transcript.ItemSaid {
			said++
		}
	}
	for _, it := range items {
		switch it.Kind {
		case transcript.ItemSteer:
			fmt.Fprintf(b, "- Recorded as the person, while it worked:\n%s\n", quoted(cut(it.Text, capSaid)))
		case transcript.ItemSaid:
			if said--; said < noteSaid {
				fmt.Fprintf(b, "- Said on the way:\n%s\n", quoted(cut(it.Text, capSaid)))
			}
		case transcript.ItemAnswer:
			if it.Text != t.Reply {
				fmt.Fprintf(b, "- Answered:\n%s\n", quoted(cut(it.Text, capSaid)))
			}
		}
	}
	switch {
	case t.Open:
		fmt.Fprintf(b, "\nThe session stopped mid-turn, after %d tool call%s.\n", t.Tools, map[bool]string{true: "", false: "s"}[t.Tools == 1])
	case t.Reply != "":
		fmt.Fprintf(b, "\nReply:\n\n%s\n", quoted(cut(t.Reply, capPrompt)))
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "time unknown"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// cut redacts s, then trims it to n bytes on a rune boundary, saying how much
// it dropped. Redacting comes first: a secret cut in half no longer matches.
func cut(s string, n int) string {
	s, _ = redact.Text(s)
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8RuneStart(s[i]) {
		i--
	}
	return s[:i] + fmt.Sprintf(" […%d more bytes]", len(s)-i)
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// lineBreaks are what a reader might take as the start of a new line.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// dropControls blanks control characters that could hide or overwrite a line's
// quote mark in a reader or a terminal.
func dropControls(r rune) rune {
	if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
		return ' '
	}
	return r
}

// quoted marks every line of s as quoted, so text from a transcript or from git
// cannot pass as one of the note's own headings or labels. Redact s first: a line
// break the quote mark adds stops redaction rules that span whitespace.
func quoted(s string) string {
	lines := strings.Split(strings.Map(dropControls, lineBreaks.Replace(s)), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// tailBytes is how much of a transcript's end lastToolCall reads.
const tailBytes = 4 << 20

// lastToolCall is the last tool call in a transcript and its output (""
// when none was recorded). Only Claude and Codex transcripts are read.
func lastToolCall(provider, path string) (call, out string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > tailBytes {
		f.Seek(fi.Size()-tailBytes, io.SeekStart)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var id string
	for sc.Scan() {
		line := sc.Bytes()
		switch provider {
		case "claude":
			if c, o, cid, kind := claudeToolRecord(line); kind == "call" {
				call, out, id, ok = c, "", cid, true
			} else if kind == "result" && cid == id {
				out = o
			}
		case "codex":
			if c, o, cid, kind := codexToolRecord(line); kind == "call" {
				call, out, id, ok = c, "", cid, true
			} else if kind == "result" && cid == id {
				out = o
			}
		}
	}
	return call, out, ok
}

func claudeToolRecord(line []byte) (call, out, id, kind string) {
	if !bytes.Contains(line, []byte(`"tool_use`)) && !bytes.Contains(line, []byte(`"tool_result"`)) {
		return
	}
	var r struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	var parts []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	json.Unmarshal(r.Message.Content, &parts)
	for _, p := range parts {
		switch p.Type {
		case "tool_use", "server_tool_use":
			var in struct {
				Command string `json:"command"`
			}
			json.Unmarshal(p.Input, &in)
			call, id, kind = p.Name+": "+string(p.Input), p.ID, "call"
			if in.Command != "" {
				call = p.Name + ": " + in.Command
			}
		case "tool_result":
			out, id, kind = toolText(p.Content), p.ToolUseID, "result"
			if p.IsError {
				out = "(error) " + out
			}
		}
	}
	return
}

func codexToolRecord(line []byte) (call, out, id, kind string) {
	if !bytes.Contains(line, []byte(`_call`)) {
		return
	}
	var r struct {
		Type    string `json:"type"`
		Payload struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "response_item" {
		return
	}
	p := r.Payload
	switch p.Type {
	case "function_call", "custom_tool_call", "local_shell_call":
		return p.Name + ": " + p.Arguments + p.Input, "", p.CallID, "call"
	case "function_call_output", "custom_tool_call_output":
		return "", toolText(p.Output), p.CallID, "result"
	}
	return
}

// toolText is a tool result's text: a string, or the text parts of a list.
func toolText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// gitSummary is the working tree's state, for a note; "" outside a repo.
func gitSummary(dir string) string {
	git := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) > capGitLines {
			lines = append(lines[:capGitLines], fmt.Sprintf("… %d more lines", len(lines)-capGitLines))
		}
		joined, _ := redact.Text(strings.Join(lines, "\n"))
		return joined
	}
	status := git("status", "-sb")
	if status == "" {
		return ""
	}
	var b strings.Builder
	for _, c := range []struct{ name, out string }{
		{"git status -sb", status},
		{"git log --oneline --decorate -8", git("log", "--oneline", "--decorate", "-8")},
		{"git stash list", git("stash", "list")},
		{"git worktree list", git("worktree", "list")},
	} {
		if c.out != "" {
			fmt.Fprintf(&b, "\n`%s`:\n\n%s\n", c.name, quoted(c.out))
		}
	}
	return b.String()
}
