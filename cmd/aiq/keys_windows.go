//go:build windows

package main

import "os"

// The full-screen views (the start menu, the resume browser, the asks and convo screens)
// do not run on Windows yet: the command gate refuses them, and openTTY fails for convo.
// These stand in for the key reader and the resize watch so the shared code builds; a
// console reader needs its own design (a console handle wakes for key-up and focus records
// that ReadFile then waits past).

// readKeys delivers nothing: the keys it returns are closed at once, as when the terminal
// ends, and stop does nothing.
func readKeys(fd int) (keys <-chan []byte, stop func()) {
	out := make(chan []byte)
	close(out)
	return out, func() {}
}

// watchResize never reports a change, and stop does nothing.
func watchResize() (resized <-chan os.Signal, stop func()) {
	return nil, func() {}
}
