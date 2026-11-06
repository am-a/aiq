//go:build unix

package main

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeTTY is a terminal whose mode is a cooked or muted Lflag, and which
// can put cooked mode back on its own, as bash does on fg.
type fakeTTY struct {
	mu    sync.Mutex
	mode  unix.Termios
	log   []string
	typed int // bytes waiting
}

func (f *fakeTTY) getMode() (*unix.Termios, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "get")
	m := f.mode
	return &m, nil
}

func (f *fakeTTY) setMode(m *unix.Termios) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = *m
	if m.Lflag&unix.ICANON != 0 {
		f.log = append(f.log, "set cooked")
	} else {
		f.log = append(f.log, "set muted")
	}
	return nil
}

func (f *fakeTTY) wait(d time.Duration) []byte {
	time.Sleep(min(d, 5*time.Millisecond))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode.Lflag&unix.ICANON != 0 || f.typed == 0 {
		return nil // line mode with no whole line: nothing, and no blocking
	}
	f.typed = 0
	return []byte("x")
}

func (f *fakeTTY) flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "flush")
	f.typed = 0
	return nil
}

func (f *fakeTTY) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "close")
}

// cookedBy is what a shell does on fg: line mode back on.
func (f *fakeTTY) cookedBy() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode.Lflag |= unix.ICANON | unix.ECHO
}

func (f *fakeTTY) lflag() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(f.mode.Lflag)
}

func (f *fakeTTY) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func cookedTTY() *fakeTTY {
	return &fakeTTY{mode: unix.Termios{Lflag: unix.ICANON | unix.ECHO | unix.ISIG}, typed: 7}
}

// Whatever --follow ends with, the terminal comes back as it was and what
// was typed into it is thrown away.
func TestMutedInputRestoresTheTerminal(t *testing.T) {
	for name, body := range map[string]func() error{
		"return": func() error { return nil },
		"error":  func() error { return errors.New("boom") },
		"panic":  func() error { panic("bug") },
	} {
		tty := cookedTTY()
		m, err := muteInput(tty)
		if err != nil {
			t.Fatal(err)
		}
		if tty.lflag()&uint64(unix.ICANON|unix.ECHO) != 0 || tty.lflag()&uint64(unix.ISIG) == 0 {
			t.Fatalf("%s: muted mode has Lflag %#x", name, tty.lflag())
		}
		func() {
			defer func() { recover() }()
			withMutedInput(m, body)
		}()
		if got := strings.Join(tty.calls(), "|"); got != "get|set muted|flush|set cooked|close" {
			t.Errorf("%s: calls %q", name, got)
		}
		if tty.lflag() != uint64(unix.ICANON|unix.ECHO|unix.ISIG) {
			t.Errorf("%s: left with Lflag %#x", name, tty.lflag())
		}
	}
}

// bash puts line mode back on fg. The discard loop never blocks in it: it
// mutes again on SIGCONT, and restore returns promptly either way.
func TestMutedInputSurvivesASuspend(t *testing.T) {
	for i := 0; i < 50; i++ {
		tty := cookedTTY()
		m, err := muteInput(tty)
		if err != nil {
			t.Fatal(err)
		}
		tty.cookedBy()
		m.cont <- syscall.SIGCONT
		if i%2 == 0 {
			deadline := time.Now().Add(time.Second)
			for tty.lflag()&uint64(unix.ICANON) != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if tty.lflag()&uint64(unix.ICANON) != 0 {
				t.Fatal("not muted again after SIGCONT")
			}
		}
		done := make(chan struct{})
		go func() { m.restore(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("restore hung")
		}
		calls := tty.calls()
		if calls[len(calls)-2] != "set cooked" || calls[len(calls)-3] != "flush" {
			t.Fatalf("calls end %q", calls)
		}
	}
}
