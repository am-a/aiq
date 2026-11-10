//go:build unix

package transcript

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenRegularRefusesWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range []string{fifo, os.DevNull, dir} {
			if f, err := OpenRegular(p); err == nil {
				f.Close()
				t.Errorf("%s was opened", p)
			}
		}
		for name, parse := range map[string]func(string) (*Session, error){"claude": ParseClaude, "codex": ParseCodex, "agy": ParseAgy} {
			if _, err := parse(fifo); err == nil {
				t.Errorf("%s read a pipe", name)
			}
		}
		f, err := OpenRegular(file)
		if err != nil {
			t.Errorf("a regular file: %v", err)
			return
		}
		f.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("opening blocked")
	}
}
