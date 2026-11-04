package main

import (
	"os"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/state"
)

func newAsksModel(key string) asks.Model { return asks.NewClient(key) }

func newAsksScorer(st *state.Store, model asks.Model) asks.Scorer {
	return &asks.Engine{St: st, Model: model}
}

// asksApp opens the side app, or prints the list when it cannot: not on a terminal, or a
// terminal that cannot draw it.
func asksApp() error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) || os.Getenv("TERM") == "dumb" {
		return printAsksList(reg, false)
	}
	return runAsksTUI(reg, leases)
}

// runAsksTUI is the side app, until it lands on this branch.
func runAsksTUI(reg, _ *state.Store) error { return printAsksList(reg, false) }
