//go:build windows

package main

import (
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// readKeys delivers what arrives on console handle fd until stop is called.
// As on Unix, stop returns once the reader has quit. It waits on the handle
// a twentieth of a second at a time so it sees stop promptly. A console
// handle also wakes for events ReadFile does not return (focus, mouse
// without mouse input enabled), and then the read waits for the next key:
// stop can take until that key.
func readKeys(fd int) (keys <-chan []byte, stop func()) {
	out := make(chan []byte)
	quit := make(chan struct{})
	done := make(chan struct{})
	h := windows.Handle(fd)
	go func() {
		defer close(done)
		defer close(out)
		buf := make([]byte, 256)
		for {
			select {
			case <-quit:
				return
			default:
			}
			ev, err := windows.WaitForSingleObject(h, 50)
			if err != nil {
				return
			}
			if ev == uint32(windows.WAIT_TIMEOUT) {
				continue
			}
			var n uint32
			if err := windows.ReadFile(h, buf, &n, nil); err != nil || n == 0 {
				return
			}
			select {
			case out <- append([]byte(nil), buf[:n]...):
			case <-quit:
				return
			}
		}
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() { close(quit) })
		<-done
	}
}

// resized is the value watchResize sends; Windows has no SIGWINCH.
type resized struct{}

func (resized) String() string { return "window size changed" }
func (resized) Signal()        {}

var _ os.Signal = resized{}

// watchResize delivers a value each time the console changes size, until
// stop is called. Windows sends no signal for it, so it checks the size of
// standard output four times a second.
func watchResize() (changed <-chan os.Signal, stop func()) {
	ch := make(chan os.Signal, 1)
	quit := make(chan struct{})
	fd := int(os.Stdout.Fd())
	go func() {
		w, h, _ := term.GetSize(fd)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
			}
			nw, nh, err := term.GetSize(fd)
			if err != nil || nw == w && nh == h {
				continue
			}
			w, h = nw, nh
			select {
			case ch <- resized{}:
			default:
			}
		}
	}()
	var once sync.Once
	return ch, func() { once.Do(func() { close(quit) }) }
}
