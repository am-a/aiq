package main

import (
	"os"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/longrun"
)

func TestHandoffRedactNeedsApplyToWrite(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	invite := "or1." + strings.Repeat("Ab3_", 12)
	note := longrun.HandoffPath("/w")
	os.MkdirAll(longrun.HandoffDir(), 0o755)
	os.WriteFile(note, []byte(invite), 0o644)

	if err := cmdHandoff([]string{"redact"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(note); string(b) != invite {
		t.Fatalf("the listing changed the note: %q", b)
	}
	if err := cmdHandoff([]string{"redact", "--apply"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(note); string(b) != "[redacted orchestra invite]" {
		t.Fatalf("note: %q", b)
	}
	if di, _ := os.Stat(longrun.HandoffDir()); di.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", di.Mode().Perm())
	}
	for _, args := range [][]string{nil, {"list"}, {"redact", "--force"}} {
		if err := cmdHandoff(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
