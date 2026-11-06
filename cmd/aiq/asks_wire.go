package main

import (
	"os"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/state"
)

func newAsksModel(key string) asks.Model { return asks.NewClient(key) }

func newAsksScorer(st *state.Store, model asks.Model) asks.Scorer {
	return &asks.Engine{St: st, Model: model}
}

// asksApp prints the list.
func asksApp() error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	here := ""
	if hasFlag(os.Args[1:], "--here") {
		if here, err = asksHereDir(); err != nil {
			return err
		}
	}
	return printAsksList(reg, false, here)
}
