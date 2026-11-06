//go:build windows

package main

import "os"

// convoTTY stands in for the real terminal, which the screen cannot run on under Windows yet:
// openTTY always fails there, so aiq convo prints the conversation as it does off a terminal,
// and nothing makes a convoTTY. Its methods fail or do nothing in case one is ever made.
type convoTTY struct{}

func newConvoTTY(in *devTTY, out *os.File) *convoTTY { return &convoTTY{} }

func (t *convoTTY) raw() error                    { return errNoTTY }
func (t *convoTTY) write(s string) error          { return errNoTTY }
func (t *convoTTY) size() (int, int, error)       { return 0, 0, errNoTTY }
func (t *convoTTY) keys() (<-chan []byte, func()) { return nil, func() {} }
func (t *convoTTY) discard(pasting bool)          {}
func (t *convoTTY) cooked()                       {}
func (t *convoTTY) abandon()                      {}
