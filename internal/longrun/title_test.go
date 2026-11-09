package longrun

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

func TestSessionTitleUsesLatestSavedName(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"custom-title\",\"customTitle\":\"Old name\"}\n{\"type\":\"custom-title\",\"customTitle\":\"Harp 雪 'quoted'\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l := state.Lease{Provider: "claude", Transcript: path, Args: `["--name","Launch name"]`}
	if got := sessionTitle(l, home); got != "Harp 雪 'quoted'" {
		t.Fatalf("Claude title = %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte("{\"id\":\"other\",\"thread_name\":\"Other\"}\n{\"id\":\"s1\",\"thread_name\":\"Old name\"}\n{\"id\":\"s1\",\"thread_name\":\"Harp\"}\n{\"id\":\"s1\""), 0600); err != nil {
		t.Fatal(err)
	}
	l.Provider, l.SessionID = "codex", "s1"
	if got := sessionTitle(l, home); got != "Harp" {
		t.Fatalf("Codex title = %q", got)
	}
}

func TestSessionTitleFallbackAndUnnamed(t *testing.T) {
	for _, tt := range []struct{ args, want string }{
		{`["--name","Harp"]`, "Harp"},
		{`["-n","Old","--name=Harp"]`, "Harp"},
		{`["--","--name","prompt text"]`, ""},
		{`["--model","opus"]`, ""},
	} {
		for _, provider := range []string{"claude", "copilot"} {
			if got := sessionTitle(state.Lease{Provider: provider, Args: tt.args}, t.TempDir()); got != tt.want {
				t.Errorf("%s %s: got %q, want %q", provider, tt.args, got, tt.want)
			}
		}
	}
	if got := sessionTitle(state.Lease{Provider: "codex", SessionID: "missing"}, t.TempDir()); got != "" {
		t.Fatalf("unnamed session received %q", got)
	}
}

func TestReplayDropsStaleStartupName(t *testing.T) {
	for _, provider := range []string{"claude", "copilot"} {
		args := []string{"--name", "Old", "-n", "Older", "--name=Oldest", "--model", "x", "--", "--name", "literal prompt"}
		want := []string{"--model", "x", "--", "--name", "literal prompt"}
		if got := replayArgs(provider, args); !reflect.DeepEqual(got, want) {
			t.Errorf("%s replay = %q, want %q", provider, got, want)
		}
	}
}

// A saved name reaches the successor as one argument it can carry: no
// control character (a NUL cannot be passed at all), no line break, and not
// too long.
func TestSessionTitleIsCleaned(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"custom-title\",\"customTitle\":\"fix\\u0000 the\\n\\tbug\\u2028now\\u001b[0m \"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := sessionTitle(state.Lease{Provider: "claude", Transcript: path}, home); got != "fix the bug now [0m" {
		t.Fatalf("title = %q", got)
	}
	for in, want := range map[string]string{
		"Harp 雪 'quoted'":               "Harp 雪 'quoted'",
		"  a \x7f b\u0085c  ":           "a b c",
		strings.Repeat("雪", 300):        strings.Repeat("雪", maxTitle),
		strings.Repeat("a", 199) + " b": strings.Repeat("a", 199),
	} {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
