package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/state"
)

// Placeholders until the rules and the side app land on this branch.
type noEngine struct{}

func (noEngine) Answer(asks.Meta, string) error { return errors.New("rules not built yet") }
func (noEngine) Reply(asks.Meta, string) error  { return errors.New("rules not built yet") }

func newAsksEngine(*state.Store, string) asks.Handler { return noEngine{} }

func asksApp() error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	items, err := a.st.ListAsks("")
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stdout, renderAsksList(items, false, time.Now()))
	return nil
}
