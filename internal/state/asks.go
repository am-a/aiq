package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// AskState is where an open ask stands. Nothing is ever deleted.
type AskState string

const (
	AskOpen         AskState = "open"
	AskProbablyDone AskState = "probably-done"
	AskClosed       AskState = "closed"
	AskDismissed    AskState = "dismissed"
)

// Ask is a stretch of an agent's own words that asks the person for something.
type Ask struct {
	ID          int64    `json:"id"`
	Chain       string   `json:"chain"`
	Workspace   string   `json:"workspace"`
	Provider    string   `json:"provider"`
	SessionID   string   `json:"session_id"`
	RaisedAt    int64    `json:"raised_at"`
	State       AskState `json:"state"`
	StateAt     int64    `json:"state_at"`
	StateReason string   `json:"state_reason"` // what moved it: "reply", "later-message", "person"
	StateText   string   `json:"state_text"`   // the text that moved it, redacted
	FirstQuote  string   `json:"first_quote"`
	LastQuote   string   `json:"last_quote"`
	AsksReader  float64  `json:"asks_reader"`
	Risk        float64  `json:"risk"`
	Refs        []string `json:"refs"`
	Repeats     int      `json:"repeats"`
	LastSeen    int64    `json:"last_seen"`
	Unconfirmed bool     `json:"unconfirmed"`
}

const asksSchema = `
CREATE TABLE IF NOT EXISTS asks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    chain        TEXT NOT NULL,
    workspace    TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    raised_at    INTEGER NOT NULL,
    state        TEXT NOT NULL,
    state_at     INTEGER NOT NULL,
    state_reason TEXT NOT NULL DEFAULT '',
    state_text   TEXT NOT NULL DEFAULT '',
    first_quote  TEXT NOT NULL,
    last_quote   TEXT NOT NULL,
    asks_reader  REAL NOT NULL DEFAULT 0,
    risk         REAL NOT NULL DEFAULT 0,
    refs         TEXT NOT NULL DEFAULT '[]',
    repeats      INTEGER NOT NULL DEFAULT 0,
    last_seen    INTEGER NOT NULL,
    unconfirmed  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS asks_chain_state ON asks(chain, state);
CREATE TABLE IF NOT EXISTS asks_seen (
    chain      TEXT NOT NULL,
    session_id TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    turn_index INTEGER NOT NULL,
    seen_at    INTEGER NOT NULL,
    PRIMARY KEY (chain, session_id, started_at, turn_index)
);
CREATE TABLE IF NOT EXISTS asks_usage (
    day   TEXT PRIMARY KEY,
    calls INTEGER NOT NULL DEFAULT 0
);
`

const askColumns = `id, chain, workspace, provider, session_id, raised_at, state, state_at, state_reason,
	state_text, first_quote, last_quote, asks_reader, risk, refs, repeats, last_seen, unconfirmed`

func scanAsk(row interface{ Scan(...any) error }) (Ask, error) {
	var a Ask
	var refs string
	var unconfirmed int
	err := row.Scan(&a.ID, &a.Chain, &a.Workspace, &a.Provider, &a.SessionID, &a.RaisedAt, &a.State, &a.StateAt,
		&a.StateReason, &a.StateText, &a.FirstQuote, &a.LastQuote, &a.AsksReader, &a.Risk, &refs, &a.Repeats,
		&a.LastSeen, &unconfirmed)
	if err != nil {
		return a, err
	}
	a.Unconfirmed = unconfirmed != 0
	json.Unmarshal([]byte(refs), &a.Refs)
	return a, nil
}

// AddAsk stores a new item and returns its ID. State defaults to open.
func (s *Store) AddAsk(a Ask) (int64, error) {
	if a.State == "" {
		a.State = AskOpen
	}
	if a.StateAt == 0 {
		a.StateAt = a.RaisedAt
	}
	if a.LastSeen == 0 {
		a.LastSeen = a.RaisedAt
	}
	if a.LastQuote == "" {
		a.LastQuote = a.FirstQuote
	}
	if a.Refs == nil {
		a.Refs = []string{}
	}
	refs, _ := json.Marshal(a.Refs)
	unconfirmed := 0
	if a.Unconfirmed {
		unconfirmed = 1
	}
	res, err := s.db.Exec(`INSERT INTO asks (chain, workspace, provider, session_id, raised_at, state, state_at, state_reason,
		state_text, first_quote, last_quote, asks_reader, risk, refs, repeats, last_seen, unconfirmed)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Chain, a.Workspace, a.Provider, a.SessionID, a.RaisedAt, a.State, a.StateAt, a.StateReason, a.StateText,
		a.FirstQuote, a.LastQuote, a.AsksReader, a.Risk, string(refs), a.Repeats, a.LastSeen, unconfirmed)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetAsk returns one item.
func (s *Store) GetAsk(id int64) (Ask, error) {
	return scanAsk(s.db.QueryRow(`SELECT `+askColumns+` FROM asks WHERE id = ?`, id))
}

// ListAsks returns items newest first. An empty chain means every chain, and no
// states means every state.
func (s *Store) ListAsks(chain string, states ...AskState) ([]Ask, error) {
	q := `SELECT ` + askColumns + ` FROM asks`
	var where []string
	var args []any
	if chain != "" {
		where = append(where, "chain = ?")
		args = append(args, chain)
	}
	if len(states) > 0 {
		marks := make([]string, len(states))
		for i, st := range states {
			marks[i] = "?"
			args = append(args, st)
		}
		where = append(where, "state IN ("+strings.Join(marks, ",")+")")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(q+` ORDER BY raised_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ask{}
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAskState moves an item and records what moved it.
func (s *Store) SetAskState(id int64, st AskState, reason, text string, now int64) error {
	res, err := s.db.Exec(`UPDATE asks SET state = ?, state_at = ?, state_reason = ?, state_text = ? WHERE id = ?`,
		st, now, reason, text, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no ask %d", id)
	}
	return nil
}

// RepeatAsk records that an agent raised an existing item again. The first quote
// stays the headline.
func (s *Store) RepeatAsk(id int64, quote string, refs []string, now int64) error {
	a, err := s.GetAsk(id)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	merged := []string{}
	for _, r := range append(a.Refs, refs...) {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}
	b, _ := json.Marshal(merged)
	_, err = s.db.Exec(`UPDATE asks SET last_quote = ?, refs = ?, repeats = repeats + 1, last_seen = ? WHERE id = ?`,
		quote, string(b), now, id)
	return err
}

// AskTurnSeen says whether a turn was already handled.
func (s *Store) AskTurnSeen(chain, sessionID string, startedAt int64, turn int) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT 1 FROM asks_seen WHERE chain=? AND session_id=? AND started_at=? AND turn_index=?`,
		chain, sessionID, startedAt, turn).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// MarkAskTurnSeen records a handled turn.
func (s *Store) MarkAskTurnSeen(chain, sessionID string, startedAt int64, turn int, now int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO asks_seen (chain, session_id, started_at, turn_index, seen_at) VALUES (?,?,?,?,?)`,
		chain, sessionID, startedAt, turn, now)
	return err
}

// CountAskCalls adds n model calls to a day's count and returns the total.
func (s *Store) CountAskCalls(day string, n int) (int, error) {
	if _, err := s.db.Exec(`INSERT INTO asks_usage (day, calls) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET calls = calls + excluded.calls`, day, n); err != nil {
		return 0, err
	}
	var total int
	err := s.db.QueryRow(`SELECT calls FROM asks_usage WHERE day = ?`, day).Scan(&total)
	return total, err
}
