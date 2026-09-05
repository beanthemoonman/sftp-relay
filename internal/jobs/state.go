// Package jobs is the queue: a worker pool, the job state machine, and the
// glue between a job row, the remote server and lftp on the NAS.
package jobs

import (
	"errors"
	"fmt"
)

// Job statuses. These are the only values the jobs.status CHECK constraint
// accepts, and the state machine below is the only thing allowed to move
// between them.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusPaused      = "paused"
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted"
)

// ErrIllegalTransition is returned by Transition for a move that is not allowed.
var ErrIllegalTransition = errors.New("jobs: illegal state transition")

// AllStatuses is every legal status value.
var AllStatuses = []string{
	StatusQueued, StatusRunning, StatusPaused, StatusDone,
	StatusFailed, StatusCancelled, StatusInterrupted,
}

// transitions is the whole state machine. done, failed and cancelled are
// terminal: a retry clones the row rather than reviving it, so history stays
// honest about what happened the first time.
var transitions = map[string]map[string]bool{
	StatusQueued: {
		StatusRunning: true, StatusCancelled: true, StatusFailed: true,
	},
	StatusRunning: {
		StatusDone: true, StatusFailed: true, StatusCancelled: true,
		StatusInterrupted: true, StatusPaused: true,
	},
	StatusPaused: {
		StatusQueued: true, StatusRunning: true, StatusCancelled: true,
	},
	StatusInterrupted: {
		StatusQueued: true, StatusRunning: true, StatusCancelled: true, StatusFailed: true,
	},
	StatusDone:      {},
	StatusFailed:    {},
	StatusCancelled: {},
}

// CanTransition reports whether from → to is legal.
func CanTransition(from, to string) bool {
	return transitions[from][to]
}

// Transition validates a move and explains the refusal when there is one.
func Transition(from, to string) error {
	if _, ok := transitions[from]; !ok {
		return fmt.Errorf("%w: unknown status %q", ErrIllegalTransition, from)
	}
	if _, ok := transitions[to]; !ok {
		return fmt.Errorf("%w: unknown status %q", ErrIllegalTransition, to)
	}
	if !transitions[from][to] {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// Terminal reports whether a job in this status will never move again.
func Terminal(status string) bool {
	m, ok := transitions[status]
	return ok && len(m) == 0
}
