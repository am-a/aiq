//go:build unix

package main

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The key reader convo uses hands on a read it holds when it is stopped, so the parser sees
// it; readKeys, as resume and asks use it, drops it as before.
func TestReadKeysHandsOnARead(t *testing.T) {
	for _, hand := range []time.Duration{0, time.Second, -1} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var keys <-chan []byte
		var stop func()
		if hand < 0 {
			// -1: the reader the convo screen takes from its terminal.
			keys, stop = newConvoTTY(&devTTY{f: r, fd: int(r.Fd())}, w).keys()
		} else {
			keys, stop = readKeysHanding(int(r.Fd()), hand, nil)
		}
		w.Write([]byte("\x1b[20"))
		time.Sleep(100 * time.Millisecond) // read, and held: nobody takes it yet
		stopped := make(chan struct{})
		go func() { stop(); close(stopped) }()
		time.Sleep(20 * time.Millisecond) // stopped before anyone takes the keys
		var got []byte
		for b := range keys {
			got = append(got, b...)
		}
		<-stopped
		r.Close()
		w.Close()
		if want := hand != 0; (string(got) == "\x1b[20") != want {
			t.Fatalf("hand %v: got %q", hand, got)
		}
	}
}

// A read the convo screen's key reader drops, as nobody took it in time, may have begun a
// paste: the way out drains the rest as one, and nothing of it is left for the shell.
func TestConvoTTYDrainsALostRead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	tty := newConvoTTY(&devTTY{f: r, fd: int(r.Fd())}, w)
	_, stop := tty.keys()
	w.Write([]byte("\x1b[20"))
	time.Sleep(100 * time.Millisecond) // read, and held
	stop()                             // nobody takes the keys: the read is dropped
	w.Write([]byte("0~pasted text"))
	tty.discard(false) // the parser never saw the read, so it cannot say a paste began
	if b := tty.in.wait(20 * time.Millisecond); len(b) > 0 {
		t.Fatalf("left for the shell %q", b)
	}
}

// A signal, such as a resize, that ends select early is not quiet: the drain waits on, and
// a tail that comes later in the same quiet window is still dropped.
func TestConvoTTYWaitOutlastsASignal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	interrupted := 0
	old := ttySelect
	t.Cleanup(func() { ttySelect = old })
	ttySelect = func(n int, rd, wr, ex *unix.FdSet, tv *unix.Timeval) (int, error) {
		if interrupted == 0 {
			interrupted++
			time.Sleep(20 * time.Millisecond)
			return -1, unix.EINTR
		}
		return old(n, rd, wr, ex, tv)
	}
	tty := newConvoTTY(&devTTY{f: r, fd: int(r.Fd())}, w)
	written := make(chan struct{})
	go func() {
		defer close(written)
		time.Sleep(60 * time.Millisecond) // inside the 100 ms quiet window
		w.Write([]byte("pasted tail\n"))
	}()
	tty.discard(true)
	<-written
	if interrupted != 1 {
		t.Fatalf("select interrupted %d times; want 1", interrupted)
	}
	ttySelect = old
	if b := tty.in.wait(20 * time.Millisecond); len(b) > 0 {
		t.Fatalf("left for the shell %q", b)
	}
}
