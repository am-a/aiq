package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/transcript"
)

// listHome sets up temporary homes and a working directory holding sessions
// of every provider, a Codex worker among them, and returns that directory
// and the path of the Claude session s2.
func listHome(t *testing.T) (work, s2 string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	claudeHome, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("PWD", work)
	write := func(path string, lines ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	project := filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"))
	claude := func(id, at, prompt string) {
		c := `"cwd":"` + work + `","sessionId":"` + id + `"`
		write(filepath.Join(project, id+".jsonl"),
			`{"type":"user",`+c+`,"timestamp":"2026-11-0`+at+`T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"`+prompt+`"}}`,
			`{"type":"assistant",`+c+`,"timestamp":"2026-11-0`+at+`T10:00:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
			`{"type":"assistant",`+c+`,"timestamp":"2026-11-0`+at+`T10:00:09Z","message":{"id":"m2","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Done."}]}}`)
	}
	claude("5e551011-0000-4000-8000-000000000001", "1", "fix the bug")
	claude("5e551011-0000-4000-8000-000000000002", "2", "write the tests")
	write(filepath.Join(project, "5e551011-0000-4000-8000-000000000003.jsonl"), `{"type":"permission-mode","permissionMode":"default"}`)
	codex := func(file, id, at, source string) {
		write(filepath.Join(codexHome, "sessions", "2026", "11", "0"+at, file),
			`{"timestamp":"2026-11-0`+at+`T11:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"`+work+`","source":"`+source+`"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:01Z","type":"turn_context","payload":{"model":"gpt-6","approval_policy":"never","sandbox_policy":{"type":"danger-full-access"}}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"ship it","turn_id":"a"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"Shipped."}}`)
	}
	codex("rollout-2026-11-03T11-00-00-a.jsonl", "019a0000-0000-7000-8000-00000000000a", "3", "cli")
	codex("rollout-2026-11-04T11-00-00-b.jsonl", "019a0000-0000-7000-8000-00000000000b", "4", "exec")
	write(filepath.Join(codexHome, "session_index.jsonl"), `{"id":"019a0000-0000-7000-8000-00000000000a","thread_name":"release"}`)
	appData := filepath.Join(home, ".gemini", "antigravity-cli")
	write(transcript.AgyTranscript(appData, "a1"),
		`{"type":"USER_INPUT","created_at":"2026-11-05T12:00:00Z","content":"<USER_REQUEST>\nlook around\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-11-05T12:00:10Z","content":"Looked.","tool_calls":[{}]}`)
	write(filepath.Join(appData, "history.jsonl"), `{"display":"look around","timestamp":1,"workspace":"`+work+`","conversationId":"a1"}`)
	return work, filepath.Join(project, "5e551011-0000-4000-8000-000000000002.jsonl")
}

// panicOn makes the parse of path panic for the rest of the test.
func panicOn(t *testing.T, path string) {
	restore := transcript.SetParserForTest(func(p string) (*transcript.Session, error) {
		switch {
		case p == path:
			panic("secret transcript text")
		case strings.HasPrefix(filepath.Base(p), "rollout-"):
			return transcript.ParseCodex(p)
		case filepath.Base(p) == "transcript.jsonl":
			return transcript.ParseAgy(p)
		}
		return transcript.ParseClaude(p)
	})
	t.Cleanup(restore)
}

// sessionOfRow is what a list row says of its session, as a Session: the
// resume list prints nothing a row does not carry.
func sessionOfRow(row transcript.Summary) transcript.Session {
	s := transcript.Session{Provider: row.Provider, ID: row.ID, Path: row.Path, Cwd: row.Cwd, Title: row.Label,
		Model: row.Model, Branch: row.Branch, Started: row.Started, Updated: row.Updated, Worker: row.Worker, Bypass: row.Bypass,
		Turns: make([]transcript.Turn, row.Turns)}
	s.Turns[0].Tools = row.Tools
	return s
}

// aiq resume --print lists the same sessions, byte for byte, whether they
// come from List or from ListWith over the direct walk.
func TestResumePrintSameFromListWith(t *testing.T) {
	work, _ := listHome(t)
	list, unreadable, err := transcript.List(defaultRoots(), work)
	if err != nil || len(unreadable) != 0 {
		t.Fatal(err, unreadable)
	}
	res, err := transcript.ListWith(context.Background(), transcript.DirectWalk(defaultRoots()), work, transcript.ParseSummary)
	if err != nil || res.Partial {
		t.Fatal(err, res.Partial)
	}
	var rows []transcript.Session
	for _, row := range res.Rows {
		if !row.Empty {
			rows = append(rows, sessionOfRow(row))
		}
	}
	if len(list) != 5 {
		t.Fatalf("%d sessions listed, want 5", len(list))
	}
	for _, all := range []bool{false, true} {
		fromList, fromRows := list, rows
		if !all {
			fromList, fromRows = interactiveOnly(list), interactiveOnly(rows)
		}
		got, want := renderSessionsPlain(work, fromRows, nil, nil), renderSessionsPlain(work, fromList, nil, nil)
		if got != want {
			t.Errorf("--all %v:\nListWith:\n%s\nList:\n%s", all, got, want)
		}
		for _, w := range []string{"release", "look around", "write the tests", "fix the bug"} {
			if !strings.Contains(want, w) {
				t.Errorf("--all %v: %q missing:\n%s", all, w, want)
			}
		}
	}
}

// A transcript whose parse panics: aiq resume lists the rest and names the
// file on stderr before anything else, and the panic's text goes nowhere.
func TestResumeNamesAnUnreadableTranscriptFirst(t *testing.T) {
	_, bad := listHome(t)
	panicOn(t, bad)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	runErr := cmdResume([]string{"--print"})
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	lines := strings.Split(string(out), "\n")
	if lines[0] != "aiq: 1 transcript could not be read: "+bad || strings.Count(string(out), "could not be read") != 1 {
		t.Fatalf("got:\n%s", out)
	}
	if !strings.HasPrefix(lines[1], "ID ") || !strings.Contains(string(out), "fix the bug") || strings.Contains(string(out), "write the tests") {
		t.Errorf("got:\n%s", out)
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("the panic's text was printed:\n%s", out)
	}
}

// The menu walks while its screen is up: the walk prints nothing, the status
// row counts what could not be read, and the paths print once the screen is
// gone, from what the walk has returned by then.
func TestMenuNamesUnreadableTranscriptsAfterItsScreen(t *testing.T) {
	work, bad := listHome(t)
	panicOn(t, bad)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	s := loadMenuSessions(work)
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	if out, _ := io.ReadAll(r); len(out) != 0 {
		t.Errorf("the walk printed %q", out)
	}
	if len(s.unreadable) != 1 || s.unreadable[0] != bad || s.count != 3 {
		t.Fatalf("%+v", s)
	}

	env := testMenuEnv()
	env.sessions = s
	m := &menu{env: env, c: defaultChoice(), w: 100, h: 30}
	var shown strings.Builder
	for _, l := range m.render() {
		for _, sg := range l {
			shown.WriteString(sg.text)
		}
		shown.WriteString("\n")
	}
	if !strings.Contains(shown.String(), " 1 transcript could not be read\n") || strings.Contains(shown.String(), bad) {
		t.Errorf("status:\n%s", shown.String())
	}

	want := "aiq: 1 transcript could not be read: " + bad + "\n"
	var b bytes.Buffer
	m.reportUnreadable(&b, nil)
	if b.String() != want {
		t.Errorf("after the screen: %q, want %q", b.String(), want)
	}
	// The walk returned but the screen never took its result.
	m.env.sessions = nil
	ch := make(chan *menuSessions, 1)
	ch <- s
	b.Reset()
	m.reportUnreadable(&b, ch)
	if b.String() != want {
		t.Errorf("walk done, not shown: %q, want %q", b.String(), want)
	}
	// The walk has not returned: nothing to say, and no wait.
	b.Reset()
	m.reportUnreadable(&b, make(chan *menuSessions, 1))
	if b.Len() != 0 {
		t.Errorf("walk still running: %q", b.String())
	}
}

// A record too long to read shows as one dim line where it came, once, also
// when the conversation is followed.
func TestConvoShowsASkippedRecord(t *testing.T) {
	s := &transcript.Session{Skipped: 1, Turns: []transcript.Turn{{Prompt: "fix it", Reply: "Fixed.", Skipped: 1}}}
	p := newConvoPrinter("claude", false, false)
	got := pieces(p)
	out := p.emit(s)
	line := "  · a record over 32 MB was skipped\n"
	if strings.Count(out, line) != 2 || !strings.HasPrefix(out, line) || strings.Index(out, "Fixed.") > strings.LastIndex(out, line) {
		t.Errorf("got:\n%s", out)
	}
	if k := turnKey(0, s.Turns[0]); len(got[k]) != 3 || got[k][2] != "skipped\x00a record over 32 MB was skipped" {
		t.Errorf("pieces %q", got)
	}
	if again := p.emit(s); again != "" {
		t.Errorf("printed again: %q", again)
	}
	s.Turns[0].Skipped = 2
	if more := p.emit(s); more != line {
		t.Errorf("a second record: %q", more)
	}
	plain := renderTurnsPlain(*s, nil)
	if strings.Count(plain, "(a record over 32 MB was skipped)\n") != 3 {
		t.Errorf("resume --print:\n%s", plain)
	}
}
