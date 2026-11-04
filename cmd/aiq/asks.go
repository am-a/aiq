package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

const asksKeyEnv = "TYPESAFE_API_KEY"

func asksKeyPath() string { return filepath.Join(filepath.Dir(paths.ConfigFile()), "typesafe.key") }

// asksKey is the model's key, from the environment or the file. Without one the
// feature does nothing and nothing leaves the machine.
func asksKey() string {
	if k := strings.TrimSpace(os.Getenv(asksKeyEnv)); k != "" {
		return k
	}
	b, err := os.ReadFile(asksKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func cmdAsks(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "setup":
		return asksSetup()
	case "dismiss", "restore":
		return asksSetState(sub, args)
	case "list":
		return asksList(args)
	case "watch":
		return asksWatch()
	case "":
		return asksApp()
	}
	return fmt.Errorf("usage: aiq asks [list [--all] [--json] | dismiss ID | restore ID | watch | setup]")
}

func asksSetup() error {
	fmt.Fprintf(os.Stderr, "aiq asks sends agents' final answers and your typed replies, redacted, to api.typesafe.ai.\nPaste the TypeSafe API key, then press Enter: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	key := strings.TrimSpace(line)
	if key == "" {
		if err != nil {
			return fmt.Errorf("no key entered")
		}
		return fmt.Errorf("no key entered")
	}
	if err := os.MkdirAll(filepath.Dir(asksKeyPath()), 0o700); err != nil {
		return err
	}
	// The file exists with its mode before the key goes in.
	if f, err := os.OpenFile(asksKeyPath(), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	if err := fsutil.WriteFileAtomic(asksKeyPath(), []byte(key+"\n")); err != nil {
		return err
	}
	if err := os.Chmod(asksKeyPath(), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "saved to", asksKeyPath())
	return nil
}

func asksSetState(verb string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: aiq asks %s ID", verb)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("not an item number: %q", args[0])
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	to := state.AskDismissed
	if verb == "restore" {
		to = state.AskOpen
	}
	return a.st.SetAskState(id, to, "person", "", time.Now().Unix())
}

func asksList(args []string) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	items, err := a.st.ListAsks("")
	if err != nil {
		return err
	}
	if hasFlag(args, "--json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	fmt.Print(renderAsksList(items, hasFlag(args, "--all"), time.Now()))
	if asksKey() == "" {
		fmt.Fprintln(os.Stderr, "\naiq asks is off: run aiq asks setup to give it a key.")
	}
	return nil
}

func renderAsksList(items []state.Ask, all bool, now time.Time) string {
	var asksNow, warn, older, done, shut []state.Ask
	perSession := map[string]int{}
	hidden := 0
	for _, it := range items {
		switch {
		case it.State == state.AskProbablyDone:
			done = append(done, it)
		case it.State != state.AskOpen:
			shut = append(shut, it)
		case now.Unix()-it.RaisedAt <= 48*3600 && it.AsksReader >= .6:
			perSession[it.SessionID]++
			if perSession[it.SessionID] > 10 && !all {
				hidden++
				continue
			}
			asksNow = append(asksNow, it)
		case it.Risk >= .8 && it.AsksReader < .5:
			warn = append(warn, it)
		default:
			older = append(older, it)
		}
	}
	var b strings.Builder
	section := func(name string, list []state.Ask) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s\n", name)
		for _, it := range list {
			repeat := ""
			if it.Repeats > 0 {
				repeat = fmt.Sprintf(" ×%d", it.Repeats+1)
			}
			fmt.Fprintf(&b, "  #%d  %s  %s ago%s\n", it.ID, it.Workspace, (time.Duration(now.Unix()-it.RaisedAt) * time.Second).Round(time.Minute), repeat)
			for _, line := range strings.Split(it.FirstQuote, "\n") {
				fmt.Fprintf(&b, "      > %s\n", line)
			}
			if it.State != state.AskOpen && it.StateReason != "" {
				fmt.Fprintf(&b, "      %s: %s\n", it.State, it.StateReason)
			}
		}
		b.WriteString("\n")
	}
	section("Asks", asksNow)
	if hidden > 0 {
		fmt.Fprintf(&b, "  %d more: aiq asks list --all\n\n", hidden)
	}
	section("Warnings", warn)
	section("Older", older)
	if all {
		section("Probably done", done)
		section("Closed and dismissed", shut)
	} else if len(done)+len(shut) > 0 {
		fmt.Fprintf(&b, "%d probably done, %d closed or dismissed: aiq asks list --all\n", len(done), len(shut))
	}
	if b.Len() == 0 {
		return "Nothing open.\n"
	}
	return b.String()
}

// holdAsksWatch takes the one watcher's place on this machine. False means another
// process holds it.
func holdAsksWatch() (*os.File, bool) {
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), "asks-watch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// watchAsks runs passes until stop closes. Failures back off and change nothing.
func watchAsks(a *app, key string, stop <-chan struct{}, logf func(string, ...any)) {
	w := &asks.Watcher{St: a.st, H: newAsksEngine(a.st, key), Hostname: pool.Hostname(), Logf: logf}
	wait := 30 * time.Second
	for {
		if err := w.Pass(); err != nil {
			logf("%v", err)
			wait = min(wait*2, 15*time.Minute)
		} else {
			wait = 30 * time.Second
		}
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
	}
}

func asksWatch() error {
	key := asksKey()
	if key == "" {
		return fmt.Errorf("aiq asks is off: run aiq asks setup to give it a key")
	}
	lock, ok := holdAsksWatch()
	if !ok {
		return fmt.Errorf("another process is already watching on this machine")
	}
	defer lock.Close()
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	fmt.Fprintln(os.Stderr, "watching long sessions on this machine. Ctrl-C stops.")
	watchAsks(a, key, make(chan struct{}), func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	return nil
}
