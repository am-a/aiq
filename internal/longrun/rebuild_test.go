package longrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// claudeTranscript writes a Claude transcript that stops mid-turn, its
// last command still running.
func claudeTranscript(t *testing.T) string {
	t.Helper()
	min := 0
	ts := func() string { min++; return "2026-09-15T10:" + strconv.Itoa(10+min) + ":00Z" }
	rec := func(body string) string {
		return `{"isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":"` + ts() + `",` + body + `}`
	}
	q := strconv.Quote
	lines := []string{
		rec(`"type":"user","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":` + q("Move the billing jobs onto the new queue.") + `}`),
		rec(`"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","role":"assistant","content":[{"type":"text","text":"Reading the job runner."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./jobs"}}]}`),
		rec(`"type":"user","promptSource":"system","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"FAIL jobs 0.2s"}]}`),
		rec(`"type":"attachment","attachment":{"type":"queued_command","prompt":"keep the old queue as a fallback","commandMode":"prompt","origin":{"kind":"human"}}`),
		rec(`"type":"assistant","message":{"id":"m2","stop_reason":"tool_use","role":"assistant","content":[{"type":"text","text":"Keeping the old queue. Running the migration."},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"make migrate"}}]}`),
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRebuildNote(t *testing.T) {
	l := state.Lease{ID: 7, Provider: "claude", AccountID: "claude/a", SessionID: "s", Transcript: claudeTranscript(t), Workspace: t.TempDir()}
	note, err := RebuildNote(l, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		rebuiltHeading + "\n",
		"## The task, as first typed",
		"Move the billing jobs onto the new queue.",
		"- Said on the way: Reading the job runner.",
		"- The person, while it worked: keep the old queue as a fallback",
		"- Said on the way: Keeping the old queue. Running the migration.",
		"The session stopped mid-turn, after 2 tool calls.",
		"```\nBash: make migrate\n```\n\nNo result recorded",
	} {
		if !strings.Contains(note, w) {
			t.Errorf("lacks %q:\n%s", w, note)
		}
	}
	if strings.Contains(note, "## The working tree") {
		t.Errorf("a git summary outside a repo:\n%s", note)
	}
	if _, err := RebuildNote(state.Lease{Provider: "claude"}, time.Now()); err == nil {
		t.Error("rebuilt a note with no transcript")
	}
}

// A note the session wrote is offered as is; one older than the session is
// an earlier takeover's and is archived; with none, a fresh successor gets
// a rebuilt note, and a resumed one the plain prompt.
func TestPrepareNote(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	s := &Supervisor{Logf: t.Logf}
	now := time.Now()
	l := state.Lease{ID: 7, Provider: "claude", AccountID: "claude/a", Transcript: claudeTranscript(t), Workspace: "/w", StartedAt: now.Add(-time.Hour).Unix()}
	note := HandoffPath(l.Workspace)
	os.MkdirAll(filepath.Dir(note), 0o700)

	os.WriteFile(note, []byte("# mine"), 0o600)
	s.prepareNote(l, false, now)
	if b, _ := os.ReadFile(note); string(b) != "# mine" {
		t.Fatalf("the session's own note was replaced: %q", b)
	}
	if p := TakeoverPrompt(l.Workspace, false, "claude"); !strings.Contains(p, "Read the handoff note at "+note) {
		t.Errorf("own note: %s", p)
	}

	old := now.Add(-2 * time.Hour)
	os.Chtimes(note, old, old)
	s.prepareNote(l, true, now)
	if _, err := os.Stat(note); err == nil {
		t.Fatal("a stale note stayed in place for a resumed successor")
	}
	if p := TakeoverPrompt(l.Workspace, true, "claude"); !strings.Contains(p, "No handoff note was written") || strings.Contains(p, "rebuilt") {
		t.Errorf("resumed: %s", p)
	}
	if archived, _ := filepath.Glob(strings.TrimSuffix(note, ".md") + ".*.md"); len(archived) != 1 {
		t.Errorf("archived %v", archived)
	}

	s.prepareNote(l, false, now)
	if !rebuilt(note) {
		t.Fatal("no rebuilt note for a fresh successor")
	}
	p := TakeoverPrompt(l.Workspace, false, "claude")
	if !strings.Contains(p, "aiq rebuilt one from its transcript: read "+note) || strings.Contains(p, "Read the handoff note at") {
		t.Errorf("rebuilt: %s", p)
	}
	// The resume that fell back to a fresh start finds the rebuilt note
	// current and keeps it.
	s.prepareNote(l, false, now)
	if !rebuilt(note) {
		t.Error("the rebuilt note did not survive a second launch")
	}
}

// aiq writes a rebuilt note just before it starts the successor, often within
// the second the successor's lease records. At the next takeover that note
// is still the earlier session's, and must not be offered as this one's.
func TestPrepareNoteArchivesNoteFromTheLeasesFirstSecond(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	s := &Supervisor{Logf: t.Logf}
	now := time.Now()
	started := now.Add(-40 * time.Hour).Truncate(time.Second)
	l := state.Lease{ID: 7, Provider: "claude", AccountID: "claude/a", Transcript: claudeTranscript(t), Workspace: "/w", StartedAt: started.Unix()}
	note := HandoffPath(l.Workspace)
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte("# earlier session"), 0o600)
	written := started.Add(170 * time.Millisecond)
	os.Chtimes(note, written, written)

	s.prepareNote(l, false, now)
	if b, _ := os.ReadFile(note); !rebuilt(note) || strings.Contains(string(b), "# earlier session") {
		t.Fatalf("the earlier session's note stayed current: %q", b)
	}
	if archived, _ := filepath.Glob(strings.TrimSuffix(note, ".md") + ".*.md"); len(archived) != 1 {
		t.Errorf("archived %v", archived)
	}
}

// A transcript with no session in it is an error, on every provider.
func TestRebuildNoteFromEmptyTranscript(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy"} {
		p := filepath.Join(t.TempDir(), "empty.jsonl")
		os.WriteFile(p, nil, 0o600)
		if _, err := RebuildNote(state.Lease{Provider: provider, Transcript: p}, time.Now()); err == nil {
			t.Errorf("%s: rebuilt a note from an empty transcript", provider)
		}
	}
}

func TestCodexToolRecord(t *testing.T) {
	call, _, id, kind := codexToolRecord([]byte(`{"type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"cmd\":\"ls\"}"}}`))
	if kind != "call" || id != "c1" || call != `shell: {"cmd":"ls"}` {
		t.Errorf("call %q %q %q", call, id, kind)
	}
	_, out, id, kind := codexToolRecord([]byte(`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}}`))
	if kind != "result" || id != "c1" || out != "a\nb" {
		t.Errorf("output %q %q %q", out, id, kind)
	}
}

func TestCut(t *testing.T) {
	if got := cut("héllo world", 2); got != "h […11 more bytes]" {
		t.Errorf("%q", got)
	}
	if got := cut("  short ", 10); got != "short" {
		t.Errorf("%q", got)
	}
}

// A synthetic invite, assembled here so the source holds nothing to report.
var fakeInvite = "or1." + strings.Repeat("Ab3_", 12)

// claudeTranscriptWith is a one-turn Claude transcript whose prompt, reply,
// command and command output are the given strings.
func claudeTranscriptWith(t *testing.T, prompt, reply, command, output string) string {
	t.Helper()
	rec := func(body string) string {
		return `{"isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":"2026-09-15T10:11:00Z",` + body + `}`
	}
	q := strconv.Quote
	lines := []string{
		rec(`"type":"user","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":` + q(prompt) + `}`),
		rec(`"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":` + q(command) + `}}]}`),
		rec(`"type":"user","promptSource":"system","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + q(output) + `}]}`),
		rec(`"type":"assistant","message":{"id":"m2","stop_reason":"end_turn","role":"assistant","content":[{"type":"text","text":` + q(reply) + `}]}`),
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRebuildNoteRedactsWhatItQuotes(t *testing.T) {
	// Padding puts the invite across capPrompt, where cutting first would
	// leave a tail too short to match.
	long := strings.Repeat("x ", (capPrompt-10)/2) + fakeInvite
	l := state.Lease{Provider: "claude", Transcript: claudeTranscriptWith(t,
		"join with "+fakeInvite, "joined using "+fakeInvite, "tool join '"+fakeInvite+"'", long)}
	note, err := RebuildNote(l, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(note, fakeInvite[:12]) || strings.Contains(note, fakeInvite[8:20]) {
		t.Fatalf("the invite reached the note:\n%s", note)
	}
	if !strings.Contains(note, "[redacted orchestra invite]") || !strings.Contains(note, "may still hold a secret") {
		t.Fatalf("the note does not say what was done:\n%s", note)
	}
	if !strings.HasPrefix(note, rebuiltHeading) {
		t.Fatalf("the heading moved, so a takeover would not recognise the note:\n%s", note)
	}
}

// The commit log goes into the note without passing through cut.
func TestRebuildNoteRedactsTheGitSummary(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "use "+fakeInvite)
	note, err := RebuildNote(state.Lease{Provider: "claude", Workspace: dir, Transcript: claudeTranscript(t)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(note, fakeInvite) || !strings.Contains(note, "use [redacted orchestra invite]") {
		t.Fatalf("the commit subject was not redacted:\n%s", note)
	}
}

func TestPrepareNoteRedactsTheSessionsNote(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	s := &Supervisor{Logf: t.Logf}
	now := time.Now()
	l := state.Lease{ID: 7, Provider: "claude", Workspace: "/w", StartedAt: now.Add(-time.Hour).Unix()}
	note := HandoffPath(l.Workspace)
	os.MkdirAll(filepath.Dir(note), 0o755)
	os.WriteFile(note, []byte("next: join with "+fakeInvite+"\n"), 0o644)
	written := now.Add(-time.Minute).Truncate(time.Second)
	os.Chtimes(note, written, written)

	s.prepareNote(l, false, now)

	b, err := os.ReadFile(note)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "next: join with [redacted orchestra invite]\n" {
		t.Fatalf("note: %q", b)
	}
	fi, _ := os.Stat(note)
	if fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(written) {
		t.Fatalf("mode %v, modified %v, want 0600 and %v", fi.Mode().Perm(), fi.ModTime(), written)
	}
	di, _ := os.Stat(filepath.Dir(note))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", di.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(note), ".redact-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestArchiveNoteRedacts(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	note := HandoffPath("/w")
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte(fakeInvite), 0o644)
	now := time.Now()
	ArchiveNote("/w", now)
	b, err := os.ReadFile(strings.TrimSuffix(note, ".md") + "." + now.Format("20060102-150405") + ".md")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[redacted orchestra invite]" {
		t.Fatalf("archive: %q", b)
	}
}

func TestPruneArchivesDeletesOnlyOldArchives(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	dir := HandoffDir()
	os.MkdirAll(dir, 0o700)
	now := time.Now()
	name := func(age time.Duration) string {
		return "caa3d066003c11ac." + now.Add(-age).Format("20060102-150405") + ".md"
	}
	old, recent := name(ArchiveRetention+time.Hour), name(ArchiveRetention-time.Hour)
	keep := []string{recent, "caa3d066003c11ac.md", "notes.20200101-000000.md", "caa3d066003c11ac.20200101-000000.txt"}
	for _, n := range append(keep, old) {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	// The current note is old by its file time, and still stays.
	os.Chtimes(filepath.Join(dir, "caa3d066003c11ac.md"), now.AddDate(-1, 0, 0), now.AddDate(-1, 0, 0))

	removed, err := PruneArchives(now)
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Fatalf("the old archive is still there: %v", err)
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s went: %v", n, err)
		}
	}
}

func TestScanNoteChangesNothing(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	note := HandoffPath("/w")
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte(fakeInvite), 0o644)
	hits, err := ScanNote(note)
	if err != nil || hits["orchestra invite"] != 1 {
		t.Fatalf("hits %v, err %v", hits, err)
	}
	b, _ := os.ReadFile(note)
	fi, _ := os.Stat(note)
	if string(b) != fakeInvite || fi.Mode().Perm() != 0o644 {
		t.Fatalf("the scan changed the note: %q, %v", b, fi.Mode().Perm())
	}
	if got := DescribeHits(hits); got != "1 orchestra invite" || strings.Contains(got, fakeInvite) {
		t.Fatalf("description: %q", got)
	}
}

func TestRedactNoteRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte(fakeInvite), 0o644)
	link := filepath.Join(dir, "note.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if _, err := RedactNote(link); err == nil {
		t.Fatal("a symlink was rewritten")
	}
}
