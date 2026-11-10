//go:build unix

package main

import (
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// readKeys delivers what arrives on terminal fd until stop is called. stop
// returns once the reader has quit, so nothing in this process is left
// reading the terminal. A reader still blocked in read(2) after the browser
// closed took input meant for the tmux client started next: the terminal's
// answers to tmux's startup queries woke it instead of tmux, tmux's own read
// found nothing, and tmux dropped the client ("server exited unexpectedly").
// It waits in select(2), as devTTY does: poll(2) does not work on terminals
// on macOS.
func readKeys(fd int) (keys <-chan []byte, stop func()) { return readKeysHanding(fd, 0, nil) }

// readKeysHanding is readKeys, but a read in hand when stop comes is still handed on, for up
// to hand, before the keys end: aiq convo's parser decides from it whether a paste may still
// be coming. A read nobody takes in that time is dropped, and lost is set, since it may have
// begun one.
func readKeysHanding(fd int, hand time.Duration, lost *atomic.Bool) (keys <-chan []byte, stop func()) {
	out := make(chan []byte)
	quit := make(chan struct{})
	done := make(chan struct{})
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
			var set unix.FdSet
			set.Set(fd)
			tv := unix.NsecToTimeval((50 * time.Millisecond).Nanoseconds())
			n, err := unix.Select(fd+1, &set, nil, nil, &tv)
			if err == unix.EINTR || (err == nil && n == 0) {
				continue
			}
			if err != nil {
				return
			}
			n, err = unix.Read(fd, buf)
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			if err != nil || n <= 0 {
				return
			}
			read := append([]byte(nil), buf[:n]...)
			select {
			case out <- read:
			case <-quit:
				if hand > 0 {
					select {
					case out <- read:
					case <-time.After(hand):
						if lost != nil {
							lost.Store(true)
						}
					}
				}
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

// watchResize delivers a value each time the terminal changes size
// (SIGWINCH), until stop is called.
func watchResize() (resized <-chan os.Signal, stop func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	return winch, func() { signal.Stop(winch) }
}
