package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
)

// The preview keeps its registry and its key apart from aiq's own files, so ending it is
// deleting these, and main never inherits them.
const (
	asksDBName  = "asks-preview.db"
	asksKeyName = "typesafe-preview.key"
)

// asksCmd is how to run this feature, as typed, so advice names the binary actually in use.
var asksCmd = filepath.Base(os.Args[0]) + " asks"

func asksKeyPath() string { return filepath.Join(filepath.Dir(paths.ConfigFile()), asksKeyName) }

// asksKey is the model's key. Anything but a private regular file counts as no key, and
// with no key nothing leaves the machine.
func asksKey() string {
	info, err := os.Lstat(asksKeyPath())
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return ""
	}
	b, err := os.ReadFile(asksKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// openAsks opens the registry, and aiq's leases read only.
func openAsks() (reg, leases *state.Store, err error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(paths.StateDB()); err != nil {
		return nil, nil, errors.New("aiq has no state on this machine yet")
	}
	reg, err = state.Open(filepath.Join(paths.DataDir(), asksDBName))
	if err != nil {
		return nil, nil, err
	}
	if err := reg.EnsureAsks(); err != nil {
		reg.Close()
		return nil, nil, err
	}
	leases, err = state.OpenReadOnly(paths.StateDB())
	if err != nil {
		reg.Close()
		return nil, nil, err
	}
	return reg, leases, nil
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
	case "workspaces":
		return asksWorkspaces()
	case "off", "on":
		return asksOffOn(sub, args)
	case "", "--no-mouse", "--here", "--all":
		return asksApp()
	case "help", "--help", "-h":
		fmt.Print(strings.ReplaceAll(asksUsage, "aiq asks", asksCmd))
		return nil
	}
	return fmt.Errorf("%s", strings.TrimSpace(strings.ReplaceAll(asksUsage, "aiq asks", asksCmd)))
}

const asksUsage = `usage: aiq asks [--all] [--no-mouse]   the side app, on this directory's asks unless --all;
                                       the list when not on a terminal
       aiq asks list [--all] [--here]  the list
       aiq asks dismiss ID    dismiss an item
       aiq asks restore ID    put an item back to open
       aiq asks watch         read long sessions and score them, in its own window
       aiq asks workspaces    list long sessions' workspaces, on or off
       aiq asks off PATH      send nothing from a workspace; aiq asks on PATH undoes it
       aiq asks setup         store the TypeSafe key
`

func asksSetup() error {
	fmt.Fprint(os.Stderr, `aiq asks sends text from long sessions on this machine to api.typesafe.ai, redacted:
everything the agents finish a stretch with, and what you type. On its first run the watcher
sends up to the last 20 turns of each session. It stops for any workspace you turn off.

Paste the TypeSafe API key, then press Enter: `)
	var line string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return errors.New("could not read the key")
		}
		line = string(b)
	} else {
		var b [4096]byte
		n, _ := os.Stdin.Read(b[:])
		line = string(b[:n])
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return errors.New("no key entered")
	}
	if err := fsutil.WriteFileAtomic(asksKeyPath(), []byte(key+"\n")); err != nil {
		return err
	}
	if err := os.Chmod(asksKeyPath(), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Saved. Turn off any workspace you do not want sent ("+asksCmd+" workspaces, then off PATH), then run the watcher in its own window: "+asksCmd+" watch")
	return nil
}

func asksSetState(verb string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: aiq asks %s ID", verb)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("not an item number: %q", sanitize(args[0]))
	}
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	to := state.AskDismissed
	if verb == "restore" {
		to = state.AskOpen
	}
	if err := reg.SetAskByPerson(id, to, time.Now().Unix()); err != nil {
		return err
	}
	a, err := reg.GetAsk(id)
	if err != nil {
		return err
	}
	undo := "restore"
	if verb == "restore" {
		undo = "dismiss"
	}
	fmt.Printf("#%d %s: %s\nUndo: %s %s %d\n", id, to, asksCut(a.FirstQuote, 60), asksCmd, undo, id)
	return nil
}

// asksWorkspaceNames lists the workspaces of this machine's long sessions, as the watcher names them.
func asksWorkspaceNames(leases *state.Store) ([]string, error) {
	list, err := leases.ListLeases()
	if err != nil {
		return nil, err
	}
	host := hostname()
	seen := map[string]bool{}
	out := []string{}
	for _, l := range list {
		name := l.Workspace
		if name == "" {
			name = l.Cwd
		}
		if l.Mode == state.ModeLong && l.Hostname == host && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func asksWorkspaces() error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	names, err := asksWorkspaceNames(leases)
	if err != nil {
		return err
	}
	off, err := reg.AskWorkspacesOff()
	if err != nil {
		return err
	}
	for _, name := range names {
		word := "on "
		if off[name] {
			word = "off"
		}
		fmt.Printf("%s  %s\n", word, sanitize(name))
	}
	for name := range off {
		if !containsString(names, name) {
			fmt.Printf("off  %s\n", sanitize(name))
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func asksOffOn(verb string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: aiq asks %s PATH", verb)
	}
	path, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	if err := reg.SetAskWorkspaceOff(path, verb == "off", time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", verb, sanitize(path))
	return nil
}

func asksCut(s string, n int) string {
	s = strings.Join(strings.Fields(sanitize(s)), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func asksList(args []string) error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	here := ""
	if hasFlag(args, "--here") {
		here = asksHereDir()
	}
	return printAsksList(reg, hasFlag(args, "--all"), here)
}

// asksHereDir is the directory --here and the f key filter on.
func asksHereDir() string {
	d, _ := os.Getwd()
	return d
}

// asksInDir says an item belongs with a directory: its workspace is that directory, holds
// it, or sits under it.
func asksInDir(workspace, dir string) bool {
	if workspace == "" || dir == "" {
		return false
	}
	return workspace == dir || strings.HasPrefix(dir, workspace+"/") || strings.HasPrefix(workspace, dir+"/")
}

// asksFilter keeps the items that belong with a directory. No directory keeps all of them.
func asksFilter(items []state.Ask, dir string) []state.Ask {
	if dir == "" {
		return items
	}
	out := []state.Ask{}
	for _, it := range items {
		if asksInDir(it.Workspace, dir) {
			out = append(out, it)
		}
	}
	return out
}

func printAsksList(reg *state.Store, all bool, here string) error {
	now := time.Now()
	items, err := reg.ListAsks("")
	if err != nil {
		return err
	}
	shown := asksFilter(items, here)
	hidden := len(items) - len(shown)
	items = shown
	st, err := reg.GetAsksStatus(now)
	if err != nil {
		return err
	}
	moved, err := reg.ListAsksMoved(asksDayStart(now))
	if err != nil {
		return err
	}
	if here != "" {
		fmt.Printf("Only %s. %d others hidden, all states.\n", sanitize(here), hidden)
		moved = asksFilter(moved, here)
	}
	fmt.Print(renderAsksList(items, moved, st, asksKey() != "", all, now))
	return nil
}

// asksDayStart is the start of the local day, for what moved today.
func asksDayStart(now time.Time) int64 {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix()
}

// asksStaleAfter is how long since a pass counts as nothing watching.
const asksStaleAfter = 5 * time.Minute

// asksStatusLine says plainly whether an empty list can be trusted. The side app and the
// list both use it, so they never disagree.
func asksStatusLine(st state.AsksStatus, keyOK bool, open int, now time.Time) string {
	clock := func(t int64) string { return time.Unix(t, 0).Format("15:04") }
	scope := "long sessions, this machine"
	switch {
	case !keyOK:
		return "Off: no key. Run " + asksCmd + " setup"
	case st.PassAt == 0:
		return "Not watching. Run " + asksCmd + " watch in its own window"
	case now.Sub(time.Unix(st.PassAt, 0)) > asksStaleAfter:
		return "Not watching since " + clock(st.PassAt) + ". Run " + asksCmd + " watch"
	case st.LastError != "":
		return "Failing since " + clock(st.ErrorAt) + ": " + sanitize(st.LastError)
	case st.Capped:
		// Days are UTC, so say when that is here.
		next := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Local().Format("15:04")
		if st.Waiting > 0 {
			return fmt.Sprintf("Daily limit hit: %d left. Resumes at %s", st.Waiting, next)
		}
		return "Daily limit hit. Resumes at " + next
	case st.Waiting > 0:
		return fmt.Sprintf("Catching up: %d left. %d watched", st.Waiting, st.Watched)
	case open == 0:
		return fmt.Sprintf("Nothing open. Watching %d, %s ago (%s)", st.Watched, fmtAge(now.Sub(time.Unix(st.PassAt, 0))), scope)
	}
	return fmt.Sprintf("Watching %d, %s ago (%s)", st.Watched, fmtAge(now.Sub(time.Unix(st.PassAt, 0))), scope)
}

func fmtAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}

// asksGroup is an open item's section.
func asksGroup(it state.Ask, now int64) string {
	switch {
	case it.Risk >= .8 && it.AsksReader < .6:
		return "Warnings"
	case now-it.RaisedAt <= 48*3600 && it.AsksReader >= .6:
		return "Asks"
	}
	return "Other open"
}

func renderAsksList(items, moved []state.Ask, st state.AsksStatus, keyOK, all bool, now time.Time) string {
	groups := map[string][]state.Ask{}
	open := 0
	var done, shut []state.Ask
	for _, it := range items {
		switch it.State {
		case state.AskOpen:
			g := asksGroup(it, now.Unix())
			groups[g] = append(groups[g], it)
			open++
		case state.AskProbablyDone:
			done = append(done, it)
		default:
			shut = append(shut, it)
		}
	}
	var b strings.Builder
	fmt.Fprintln(&b, asksStatusLine(st, keyOK, open, now))
	if st.Skipped > 0 {
		fmt.Fprintf(&b, "%d older texts not read\n", st.Skipped)
	}
	for _, nw := range st.NotWatched {
		fmt.Fprintln(&b, "Not watched: "+sanitize(nw))
	}
	closed, prob := 0, 0
	for _, it := range moved {
		if it.State == state.AskClosed {
			closed++
		} else {
			prob++
		}
	}
	fmt.Fprintf(&b, "Moved today: %d closed, %d probably done\n", closed, prob)
	for _, it := range moved {
		fmt.Fprintf(&b, "  #%d %s, by: %s\n     %s\n", it.ID, it.State, asksCut(it.StateText, 100), asksCut(it.FirstQuote, 100))
	}
	section := func(name string, list []state.Ask) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s\n", name)
		for _, it := range list {
			fmt.Fprintf(&b, "  #%d  %s  %s  %s ago", it.ID, sanitize(filepath.Base(it.Workspace)), sanitize(it.Provider), fmtAge(now.Sub(time.Unix(it.RaisedAt, 0))))
			if it.Repeats > 0 {
				fmt.Fprintf(&b, "  asked %dx", it.Repeats+1)
			}
			fmt.Fprintln(&b)
			for _, line := range strings.Split(sanitize(it.FirstQuote), "\n") {
				fmt.Fprintf(&b, "      > %s\n", line)
			}
			if it.State != state.AskOpen {
				fmt.Fprintf(&b, "      %s", it.State)
				if it.StateReason != "" {
					fmt.Fprintf(&b, " (%s)", sanitize(it.StateReason))
				}
				fmt.Fprintln(&b)
			}
		}
	}
	section("Asks", groups["Asks"])
	section("Warnings", groups["Warnings"])
	section("Other open", groups["Other open"])
	if all {
		section("Probably done", done)
		section("Closed and dismissed", shut)
	} else if len(done)+len(shut) > 0 {
		fmt.Fprintf(&b, "\n%d probably done, %d closed or dismissed: %s list --all\n", len(done), len(shut), asksCmd)
	}
	return b.String()
}

// holdAsksWatch takes the one watcher's place on this machine.
func holdAsksWatch() (*os.File, error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), "asks-watch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another process is already watching")
		}
		return nil, err
	}
	return f, nil
}

func asksWatch() error {
	if asksKey() == "" {
		return errors.New("no key: run " + asksCmd + " setup")
	}
	lock, err := holdAsksWatch()
	if err != nil {
		return err
	}
	defer lock.Close()
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	host := hostname()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fmt.Fprintln(os.Stderr, "Watching long sessions on this machine. Ctrl-C stops it.")
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
	key := ""
	var w *asks.Watcher
	const pause = 30 * time.Second
	wait := pause
	lastErr := ""
	// Sleep first: a pass that dies at once must not spin through restarts.
	first := time.After(time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-first:
		case <-time.After(wait):
		}
		first = nil
		k := asksKey()
		if k == "" {
			logf("the key file is gone or not private: nothing is being sent")
			wait = pause
			continue
		}
		if k != key {
			key = k
			capped := &asks.CappedModel{Inner: newAsksModel(key), St: reg}
			w = &asks.Watcher{Asks: reg, Leases: leases, Scorer: newAsksScorer(reg, capped), Budget: capped, Hostname: host,
				KeyOK: func() bool { return asksKey() == key }, Logf: logf}
			wait = pause
		}
		res := w.Pass(ctx)
		switch {
		case res.Err == nil:
			wait = pause
			lastErr = ""
		case res.RetryAfter > 0:
			wait = res.RetryAfter
		default:
			wait = min(wait*2, 15*time.Minute)
		}
		if res.Err != nil && res.Err.Error() != lastErr {
			lastErr = res.Err.Error()
			logf("%s", lastErr)
		}
	}
}
