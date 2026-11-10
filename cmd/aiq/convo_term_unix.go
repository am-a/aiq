//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// queryBackground asks the terminal for its background colour (OSC 11) and
// reports whether it is light. Primary Device Attributes (DA1) goes out
// right after it: every terminal answers that, and in order, so once its
// reply is in, an OSC 11 reply has come before it or is not coming, and
// nothing is left to turn up later on the shell's or the pager's input.
// The terminal is in raw mode meanwhile, but for ISIG: Ctrl-C still
// raises SIGINT, which closes stop and ends the wait. A second with no
// DA1 reply ends it too.
func queryBackground(stop <-chan struct{}) (light, ok bool) {
	tty, err := openTTY()
	if err != nil {
		return false, false
	}
	defer tty.close()
	saved, err := term.MakeRaw(tty.fd)
	if err != nil {
		return false, false
	}
	defer term.Restore(tty.fd, saved)
	if mode, err := tty.getMode(); err == nil {
		mode.Lflag |= unix.ISIG
		tty.setMode(mode)
	}
	if _, err := unix.Write(tty.fd, []byte("\x1b]11;?\x1b\\\x1b[c")); err != nil {
		return false, false
	}
	reply := readUntilDA1(tty.wait, stop, time.Second)
	r, g, b, ok := parseOSC11(reply)
	if !ok {
		return false, false
	}
	termBackground.r, termBackground.g, termBackground.b, termBackground.ok = r, g, b, true
	return luminance(r, g, b) > 0.5, true
}

// devTTY is the controlling terminal, opened on its own and non-blocking:
// a read never waits, in whatever mode the terminal is (a shell may put
// it back in line mode under us after Ctrl-Z and fg).
type devTTY struct {
	f  *os.File
	fd int
}

func openTTY() (*devTTY, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	t := &devTTY{f: f, fd: int(f.Fd())}
	if err := unix.SetNonblock(t.fd, true); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *devTTY) close() { t.f.Close() }

func (t *devTTY) getMode() (*unix.Termios, error) { return unix.IoctlGetTermios(t.fd, ioctlGetTermios) }

func (t *devTTY) setMode(m *unix.Termios) error {
	return unix.IoctlSetTermios(t.fd, ioctlSetTermios, m)
}

// flush throws away input the terminal holds that nobody has read yet.
func (t *devTTY) flush() error { return flushInput(t.fd) }

// ttySelect is select(2). Tests swap it.
var ttySelect = unix.Select

// wait returns what the terminal has to read within d (nothing if nothing
// came). It waits in select(2): poll(2) does not work on terminals on
// macOS. A signal, such as a resize, ends select early; that is not
// quiet, so it waits again for the rest of d.
func (t *devTTY) wait(d time.Duration) []byte {
	deadline := time.Now().Add(d)
	for {
		var set unix.FdSet
		set.Set(t.fd)
		tv := unix.NsecToTimeval(max(time.Until(deadline), 0).Nanoseconds())
		n, err := ttySelect(t.fd+1, &set, nil, nil, &tv)
		if err == unix.EINTR && time.Until(deadline) > 0 {
			continue
		}
		if err != nil || n == 0 {
			return nil
		}
		break
	}
	buf := make([]byte, 512)
	n, _ := unix.Read(t.fd, buf) // EAGAIN (a half line in line mode) reads as nothing
	return buf[:max(n, 0)]
}

// muteTTY is what --follow needs from the terminal; tests fake it.
type muteTTY interface {
	getMode() (*unix.Termios, error)
	setMode(*unix.Termios) error
	wait(d time.Duration) []byte
	flush() error
	close()
}

// mutedInput swallows what is typed while --follow runs: echo and line
// editing are off, so keys show nowhere, and what comes in is read and
// thrown away, so nothing looks like it reaches the session and nothing
// is left for the shell when --follow ends. Ctrl-C still quits, and
// output is processed as usual.
type mutedInput struct {
	tty   muteTTY
	saved *unix.Termios
	muted *unix.Termios
	done  chan struct{}
	gone  chan struct{} // closed when discard has returned
	cont  chan os.Signal
}

func muteInput(tty muteTTY) (*mutedInput, error) {
	saved, err := tty.getMode()
	if err != nil {
		return nil, err
	}
	muted := *saved
	muted.Lflag &^= unix.ICANON | unix.ECHO
	muted.Cc[unix.VMIN], muted.Cc[unix.VTIME] = 1, 0
	if err := tty.setMode(&muted); err != nil {
		return nil, err
	}
	m := &mutedInput{tty: tty, saved: saved, muted: &muted, done: make(chan struct{}), gone: make(chan struct{}), cont: make(chan os.Signal, 1)}
	// After Ctrl-Z and fg the shell may have put its own mode back.
	signal.Notify(m.cont, syscall.SIGCONT)
	go m.discard()
	return m, nil
}

// discard reads and drops input, a tenth of a second at a time at most,
// so it sees done and SIGCONT promptly whatever the terminal's mode.
func (m *mutedInput) discard() {
	defer close(m.gone)
	for {
		select {
		case <-m.done:
			return
		case <-m.cont:
			m.tty.setMode(m.muted)
		default:
		}
		m.tty.wait(100 * time.Millisecond)
	}
}

// restore drops what is still waiting to be read and puts the terminal
// back as --follow found it. discard has returned first, so a SIGCONT it
// was handling cannot put the muted mode back after this.
func (m *mutedInput) restore() {
	signal.Stop(m.cont)
	close(m.done)
	<-m.gone
	m.tty.flush()
	m.tty.setMode(m.saved)
	m.tty.close()
}
