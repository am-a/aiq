package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
)

// The command keeps its own schema version and the whole event list, and reads as
// many rows as it is asked for.
func TestStatusJSONKeepsEventsAndItsVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	t.Setenv("AIQ_CONFIG", t.TempDir()+"/config.toml")
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		st.LogEvent("claude", "claude/a", "launch", "x", time.Now())
	}
	st.Close()

	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"--json"}, 15},
		{[]string{"--json", "--events", "17"}, 17},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = w
		runErr := cmdStatus(c.args)
		os.Stdout = stdout
		w.Close()
		out, _ := io.ReadAll(r)
		if runErr != nil {
			t.Fatal(runErr)
		}
		var doc struct {
			SchemaVersion int               `json:"schema_version"`
			Events        []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("%v: %v", err, string(out))
		}
		if doc.SchemaVersion != 1 || len(doc.Events) != c.want {
			t.Errorf("%v: version %d with %d events, want 1 with %d", c.args, doc.SchemaVersion, len(doc.Events), c.want)
		}
	}
}

func TestStatusEvents(t *testing.T) {
	for _, c := range []struct {
		args []string
		want int
	}{
		{nil, 15},
		{[]string{"--json"}, 15},
		{[]string{"--events", "50"}, 50},
		{[]string{"--json", "--events=200"}, 200},
		{[]string{"--events", "0"}, 0},
	} {
		got, err := statusEvents(c.args)
		if err != nil || got != c.want {
			t.Errorf("%v: got %d, %v, want %d", c.args, got, err, c.want)
		}
	}
	for _, args := range [][]string{{"--events"}, {"--events", "many"}, {"--events=-1"}, {"--events", "--json"}} {
		if _, err := statusEvents(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
