//go:build windows

package main

import "errors"

// errNoTTY is why aiq convo cannot take the console's input aside on
// Windows: there is no /dev/tty, and console modes are not wired up yet.
var errNoTTY = errors.New("muting the terminal for aiq convo --follow is not supported on Windows yet")

// queryBackground cannot ask the console for its colour yet, so it never
// knows; the theme falls back to $COLORFGBG, then dark.
func queryBackground(stop <-chan struct{}) (light, ok bool) { return false, false }

// devTTY stands in for the controlling terminal, which openTTY cannot open
// on Windows yet.
type devTTY struct{}

func openTTY() (*devTTY, error) { return nil, errNoTTY }

func (t *devTTY) close() {}

// mutedInput is never made on Windows: muteInput always fails, and
// --follow then runs with keys echoing, as it does on Unix when the
// terminal cannot be muted.
type mutedInput struct{}

func muteInput(tty *devTTY) (*mutedInput, error) { return nil, errNoTTY }

func (m *mutedInput) restore() {}
