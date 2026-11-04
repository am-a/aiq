package main

import (
	"context"
	"errors"
	"os"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/state"
)

// Placeholders until the rules land on this branch. They refuse every call, so nothing can be sent.
type noModel struct{}

func (noModel) Ask(context.Context, string, map[string]asks.Question) (map[string]float64, error) {
	return nil, errors.New("rules not built yet")
}

type noScorer struct{}

func (noScorer) Score(context.Context, asks.Entry) (state.AskPlan, error) {
	return state.AskPlan{}, errors.New("rules not built yet")
}

func newAsksModel(string) asks.Model { return noModel{} }

func newAsksScorer(*state.Store, asks.Model) asks.Scorer { return noScorer{} }

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
