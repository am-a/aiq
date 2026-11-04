// Package asks tracks what agents asked of the person, from outside the agents.
// It reads transcripts and writes only its own tables. Nothing in it can start a
// process, type into a pane or launch a provider.
package asks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// Meta says where a text came from.
type Meta struct {
	Chain     string
	Workspace string
	Provider  string
	SessionID string
}

// EntryKind says what an entry is to the rules.
type EntryKind int

const (
	EntryReply  EntryKind = iota // the person typed it: a prompt or a steer
	EntryAnswer                  // the agent finished a stretch with it
)

// Entry is one text to score. Text is already through redact.Clean, and pasted blocks
// are out of a reply. At is when it was written, in Unix seconds. Aiq marks an answer in a
// turn that aiq started, whose repeats only update last-seen.
type Entry struct {
	Meta
	Kind EntryKind
	Text string
	At   int64
	Aiq  bool
}

// Scorer applies the rules to one entry and returns what they decided. It writes nothing:
// the watcher applies the plan and the session's new position in one transaction, so an
// error at any call leaves the registry unchanged.
type Scorer interface {
	Score(ctx context.Context, e Entry) (state.AskPlan, error)
}

// Question is one question put to the model.
type Question struct {
	Instructions string
	True         string
	False        string
}

// Model answers questions about a state with a probability each.
type Model interface {
	Ask(ctx context.Context, state string, questions map[string]Question) (map[string]float64, error)
}

// ErrorClass says what the watcher does about a failed call.
type ErrorClass int

const (
	ClassServer    ErrorClass = iota // 5xx, timeout, unreachable: stop the pass and back off
	ClassAuth                        // 401, 402, 403, 404: stop the pass until the key changes
	ClassRate                        // 429: stop the pass, honor RetryAfter
	ClassMalformed                   // a 2xx that makes no sense: next session, skip after 5 tries
	ClassRejected                    // 400, 413, 422: skip the entry, stop after 3 in a row
	ClassCap                         // the day's cap is reached: wait, this is no failure
	ClassBackfill                    // the day's backfill limit is reached: this session waits
)

// ModelError is a failed call. It holds no response text and no URL, because its message
// goes to the status row and the log.
type ModelError struct {
	Class      ErrorClass
	Status     int
	RetryAfter time.Duration
}

func (e *ModelError) Error() string {
	switch e.Class {
	case ClassAuth:
		return fmt.Sprintf("the model refused the key (HTTP %d)", e.Status)
	case ClassRate:
		return "the model is rate limiting"
	case ClassMalformed:
		return "the model gave a malformed answer"
	case ClassRejected:
		return fmt.Sprintf("the model rejected a request (HTTP %d)", e.Status)
	case ClassCap:
		return "daily limit reached"
	case ClassBackfill:
		return "daily backfill limit reached"
	}
	if e.Status != 0 {
		return fmt.Sprintf("the model failed (HTTP %d)", e.Status)
	}
	return "the model could not be reached"
}

func classOf(err error) ErrorClass {
	var me *ModelError
	if errors.As(err, &me) {
		return me.Class
	}
	return ClassServer
}
