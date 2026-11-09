package longrun

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

// A long session that cannot move says why: in a line under it in
// `aiq long list` and on the status page, and in its tmux window's name. The
// daemon works both out after each decision, from the lease as it then is,
// and never feeds them back into a decision.

// Kinds of waiting, the reasons a decision could not move a lease.
const (
	waitNoSuccessor = "no successor"
	waitNoPane      = "no pane"
	waitNoLauncher  = "no launcher"
	waitRespawn     = "respawn failed"
)

// waiting is why a decision could not move a lease this tick.
type waiting struct {
	key      string   // one of the wait* kinds
	text     string   // the event to log once, "" when the caller logs its own
	none     *nowhere // for waitNoSuccessor
	provider string   // for waitNoLauncher: where it would have gone
	launcher string
}

// nowhere is why no account can take a lease over.
type nowhere struct {
	lead    string   // the first sentence
	login   bool     // logging an account in would free one
	reasons []string // one per account or provider, never empty
	word    string   // the own account's short word when it is out
}

func (n nowhere) advice() string {
	if n.login {
		return " Log in to free one: aiq doctor runs the logins."
	}
	return ""
}

// line is the form stored for people: the lead, then one reason per line.
func (n nowhere) line() string {
	return n.lead + n.advice() + "\n" + strings.Join(n.reasons, "\n")
}

// event is the form logged on one line.
func (n nowhere) event() string {
	return n.lead + n.advice() + " " + strings.Join(n.reasons, "; ")
}

// maxReasons caps the accounts a reason list names.
const maxReasons = 8

// noSuccessor is the wait for a lease no account can take over.
func (s *Supervisor) noSuccessor(l state.Lease, now time.Time) waiting {
	n := s.nowhere(l, now)
	return waiting{key: waitNoSuccessor, text: n.event(), none: &n}
}

// nowhere says, for every account the successor search looks at, why it did
// not take l over. It reads recorded state only.
func (s *Supervisor) nowhere(l state.Lease, now time.Time) nowhere {
	floor := s.Pool.Cfg.Long.DrainPct
	remaining, _, blocked := s.headroom(l.AccountID, l.Provider, now)
	ownRung := s.rung(l.AccountID, l.Provider, now)
	need := floor
	if blocked {
		need = 0
	}
	var n nowhere
	switch phrase, word, login := s.ownOut(l, now); {
	case blocked:
		n.word, n.login = word, login
		n.lead = "its account is out: " + phrase + ". No other account can take it."
		if word == "disabled" {
			n.lead += " It stays here; aiq long stop . ends it."
		}
	case ownRung == selector.RungExtra:
		n.lead = fmt.Sprintf("it stays on extra usage until an account with more than %.0f%% plan quota frees up.", floor)
	case ownRung > selector.RungLastResort:
		n.lead = fmt.Sprintf("it stays on this last-resort account until an account with more than %.0f%% plan quota frees up.", floor)
	default:
		n.lead = fmt.Sprintf("no account to move to; it keeps going on the %.0f%% left.", remaining)
	}

	type entry struct {
		fixable bool // a login problem: listed first
		order   int  // the provider's place in the search
		id      string
		text    string
	}
	var es []entry
	for i, provider := range s.providerOrder(l) {
		if s.tierSkips(l, provider) {
			es = append(es, entry{order: i, text: provider + ": no model of this session's tier"})
			continue
		}
		cands, err := s.Pool.Candidates(provider)
		if err != nil {
			es = append(es, entry{order: i, text: provider + ": accounts unreadable"})
			continue
		}
		pol := s.policy(provider, now)
		ranked := map[string]selector.Ranked{}
		for _, r := range selector.Rank(pol, cands) {
			ranked[r.ID] = r
		}
		others := 0
		for _, c := range cands {
			if c.ID == l.AccountID {
				continue
			}
			others++
			r := ranked[c.ID]
			e := entry{order: i, id: c.ID}
			if r.Eligible {
				e.text = passedOver(c, r, pol.ModelScope, now, blocked, l.AccountID, floor)
			} else {
				e.text, _, e.fixable = outPhrase(c, pol.ModelScope, now)
				// A login frees it only if nothing else holds it back.
				if e.fixable && c.CooldownUntil <= now.Unix() && (c.LastResort == 0 || blocked) {
					if _, spent := selector.ExhaustedWindow(c.Windows, pol.ModelScope, now); !spent {
						if h, _ := selector.Headroom(c.Windows, pol.ModelScope, now); h > need {
							n.login = true
						}
					}
				}
			}
			e.text = c.ID + ": " + e.text
			es = append(es, e)
		}
		if others == 0 {
			es = append(es, entry{order: i, text: provider + ": no other account"})
		}
	}
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.fixable != b.fixable {
			return a.fixable
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.id < b.id
	})
	for i, e := range es {
		if i == maxReasons {
			n.reasons = append(n.reasons, fmt.Sprintf("and %d more", len(es)-maxReasons))
			break
		}
		n.reasons = append(n.reasons, e.text)
	}
	if len(n.reasons) == 0 {
		n.reasons = []string{"no account to try (long.fallback names no provider)"}
	}
	return n
}

// ownOut is how the lease's own account is out, if it is: the phrase, a
// short word for the window name, and whether a login fixes it.
func (s *Supervisor) ownOut(l state.Lease, now time.Time) (phrase, word string, login bool) {
	cands, err := s.Pool.Candidates(l.Provider)
	if err != nil {
		return "unavailable", "unavailable", false
	}
	scope := s.policy(l.Provider, now).ModelScope
	for _, c := range cands {
		if c.ID == l.AccountID {
			return outPhrase(c, scope, now)
		}
	}
	return "unavailable", "unavailable", false
}

// outPhrase says why an account cannot serve, from its fields only: never
// from UnavailableReason or a provider's text, which can name identities.
func outPhrase(c selector.Candidate, scope string, now time.Time) (phrase, word string, login bool) {
	login = true
	switch {
	case !c.Enabled:
		return "disabled", "disabled", false
	case !c.HasCredential:
		return "no credential — run: aiq account login " + c.ID, "logged out", login
	case c.Unavailable == selector.UnavailableLoginOut:
		return "CLI not logged in — run: aiq account login " + c.ID, "logged out", login
	case c.Unavailable == selector.UnavailableLoginMismatch:
		return "CLI logged in as another identity — run: aiq account login " + c.ID, "logged out", login
	case c.Unavailable == selector.UnavailableAuthRequired:
		return "not logged in (authentication required) — run: aiq account login " + c.ID, "logged out", login
	case c.CooldownUntil > now.Unix():
		return "exhausted until " + resetAt(c.CooldownUntil, now), "exhausted", false
	}
	if w, ok := selector.ExhaustedWindow(c.Windows, scope, now); ok {
		kind := "a"
		if w.Kind == state.KindShort || w.Kind == state.KindWeekly {
			kind = w.Kind
		}
		phrase = kind + " window exhausted"
		if w.ResetsAt > 0 {
			phrase += " until " + resetAt(w.ResetsAt, now)
		}
		return phrase, "exhausted", false
	}
	return "unavailable", "unavailable", false
}

// passedOver says why the search passed over an account that could serve.
func passedOver(c selector.Candidate, r selector.Ranked, scope string, now time.Time, blocked bool, own string, floor float64) string {
	switch {
	case r.Rung == selector.RungExtra:
		return "extra usage; this session is already on extra usage"
	case r.Rung > selector.RungLastResort && !blocked:
		return "last resort; used only once " + own + " runs out"
	}
	h, _ := selector.Headroom(c.Windows, scope, now)
	if h <= 0 {
		return "no quota left"
	}
	return fmt.Sprintf("%.0f%% left, at or below the %.0f%% floor", h, floor)
}

// resetAt is a reset time as people read it: the clock today, the weekday
// further out.
func resetAt(unix int64, now time.Time) string {
	t := time.Unix(unix, 0).Local()
	if t.Sub(now) > 24*time.Hour {
		return t.Format("Mon 15:04")
	}
	return t.Format("15:04")
}

// facts is what apply learned about a lease after its decision.
type facts struct {
	blocked bool
	own     string // the own account's phrase when it is out
	word    string // and its short word
	wait    waiting
	found   bool     // some account can take it over
	none    *nowhere // why not, when none can
}

// standing is the window name and the line a lease shows, from its state
// after the decision. The line is "" for a session running normally; the
// name is "" where there is no window.
func standing(l state.Lease, f facts) (label, line string) {
	acc := tmux.SafeName(l.AccountID)
	switch {
	case l.Drain == state.DrainNone && !f.blocked:
		return acc, ""
	case l.Pane == "":
		return "", "no tmux pane recorded; it cannot be moved"
	case f.wait.key == waitNoLauncher:
		return acc + " · cannot move", fmt.Sprintf("no launcher for %s in the %q chain, so it cannot move there", f.wait.provider, f.wait.launcher)
	case f.wait.key == waitRespawn:
		return acc + " · cannot move", "the move could not start in its pane; see the daemon log"
	case !f.found && f.none != nil:
		label = acc + " · "
		if f.blocked {
			label += f.none.word + ", "
		}
		label += "no account to move to"
		if f.none.login {
			label += ": log in (aiq doctor)"
		}
		return label, f.none.line()
	case f.blocked:
		return acc + " · " + f.word + ", moving", "its account is out: " + f.own + ". It moves to another account at the next check"
	case l.Drain == state.DrainRequested && silentLauncher(l):
		return acc + " · moves after next turn", fmt.Sprintf(
			"no turn seen yet from launcher %q; it moves after its first reported turn, or at once if its account runs out. If it has had turns, its hooks cannot reach aiq", l.Launcher)
	case l.Drain == state.DrainRequested:
		return acc + " · moves after next turn", "asks for a handoff note at its next prompt or turn end, then moves to another account"
	case l.Drain == state.DrainDraining:
		return acc + " · writing handoff note", "writing its handoff note; moves to another account when this turn ends"
	case l.InTurn():
		return acc + " · moves when this turn ends", "wrapped up; moves to another account when the current turn ends"
	}
	return acc + " · moving", "wrapped up; moves to another account at the next check"
}

// apply shows a lease's standing after its decision. It acts only on the
// lease that was judged: a move releases it, and its id can be taken by
// another lease before this runs.
func (s *Supervisor) apply(l state.Lease, now time.Time) {
	cur, err := s.Pool.St.GetLease(l.ID)
	if err != nil || cur.StartedAt != l.StartedAt || cur.PID != l.PID || cur.Pane != l.Pane {
		return
	}
	f := facts{wait: s.waits[cur.ID], found: true}
	_, _, f.blocked = s.headroom(cur.AccountID, cur.Provider, now)
	if (cur.Drain != state.DrainNone || f.blocked) && cur.Pane != "" {
		switch f.wait.key {
		case waitNoLauncher, waitRespawn:
		case waitNoSuccessor:
			f.found, f.none = false, f.wait.none
		default:
			if _, err := s.successor(cur, now); err != nil {
				n := s.nowhere(cur, now)
				f.found, f.none = false, &n
			}
		}
		if f.blocked {
			f.own, f.word, _ = s.ownOut(cur, now)
		}
	}
	label, line := standing(cur, f)
	s.show(cur, label, line)
}

// shown names one lease in one pane, for what the daemon last showed of it.
type shown struct {
	id, startedAt int64
	pane          string
}

// show writes a lease's line and names its window, each only when it
// changed. A window not yet named by the daemon holds the account id, as
// the launch named it, so a daemon restart renames nothing that runs normally.
func (s *Supervisor) show(l state.Lease, label, line string) {
	if s.lines == nil {
		s.lines, s.labels = map[shown]string{}, map[shown]string{}
	}
	k := shown{l.ID, l.StartedAt, l.Pane}
	if last, ok := s.lines[k]; !ok || last != line {
		if s.Pool.St.SetLeaseWhy(l.ID, l.StartedAt, l.PID, line) == nil {
			s.lines[k] = line
		}
	}
	if label == "" {
		return
	}
	last, ok := s.labels[k]
	if !ok {
		last = tmux.SafeName(l.AccountID)
	}
	// A failed rename is not retried until the name changes again.
	s.labels[k] = label
	if label == last {
		return
	}
	rename := tmux.NameWindow
	if s.rename != nil {
		rename = s.rename
	}
	if err := rename(l.Pane, label); err != nil {
		s.Logf("lease %d: naming the window of %s %q: %v", l.ID, l.Pane, label, err)
	}
}

// providerOrder is the order the successor search walks providers in: the
// lease's fallback list, or its provider then long.fallback, with the last
// resorts last and each provider once.
func (s *Supervisor) providerOrder(l state.Lease) []string {
	order := strings.Split(l.Fallback, ",")
	if l.Fallback == "" {
		order = append([]string{l.Provider}, s.Pool.Cfg.Long.Fallback...)
	}
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := s.Pool.Cfg.LastResortRank(strings.TrimSpace(order[i])), s.Pool.Cfg.LastResortRank(strings.TrimSpace(order[j]))
		return ri < rj
	})
	seen := map[string]bool{}
	var out []string
	for _, provider := range order {
		provider = strings.TrimSpace(provider)
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	return out
}
