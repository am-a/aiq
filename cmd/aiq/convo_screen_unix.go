//go:build unix

package main

import (
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// convoTTY is the real terminal: the keyboard is the controlling terminal, the screen stdout.
type convoTTY struct {
	in    *devTTY
	out   *os.File
	outFd int
	old   *term.State
	// lost says the key reader dropped a read the parser never saw, as it was stopped.
	lost atomic.Bool
}

func newConvoTTY(in *devTTY, out *os.File) *convoTTY {
	return &convoTTY{in: in, out: out, outFd: int(out.Fd())}
}

func (t *convoTTY) raw() (err error) {
	t.old, err = term.MakeRaw(t.in.fd)
	return err
}

func (t *convoTTY) write(s string) error {
	_, err := t.out.WriteString(s)
	return err
}

func (t *convoTTY) size() (int, int, error) { return term.GetSize(t.outFd) }

func (t *convoTTY) keys() (<-chan []byte, func()) {
	return readKeysHanding(t.in.fd, convoPieceWait, &t.lost)
}

// discard drops what was typed and not read. A read the key reader dropped may have begun a
// paste, so the rest is drained as one.
func (t *convoTTY) discard(pasting bool) {
	if pasting || t.lost.Load() {
		drainPaste(t.in.wait, convoPasteQuiet, convoPasteLimit)
	}
	t.in.flush()
}

func (t *convoTTY) cooked() {
	if t.old != nil {
		term.Restore(t.in.fd, t.old)
	}
}

// abandon drops the output the terminal has not taken, and points stdin, stdout and stderr
// at /dev/null: on macOS the last close of a terminal opened to block, as aiq's own exit can
// be, waits for that output to go. The keyboard, opened not to block, then closes last, and
// such a close drops what is left instead.
func (t *convoTTY) abandon() {
	flushOutput(t.outFd)
	flushOutput(t.in.fd)
	null, err := unix.Open(os.DevNull, unix.O_RDWR, 0)
	if err != nil {
		return
	}
	for fd := 0; fd <= 2; fd++ {
		toNull(null, fd)
	}
}
