package jobs

import (
	"errors"
	"slices"
	"testing"
)

// legal is the transition table restated by hand. It exists so that a change to
// the state machine has to be made deliberately in two places, not once.
var legal = map[string][]string{
	StatusQueued:      {StatusRunning, StatusCancelled, StatusFailed},
	StatusRunning:     {StatusDone, StatusFailed, StatusCancelled, StatusInterrupted, StatusPaused},
	StatusPaused:      {StatusQueued, StatusRunning, StatusCancelled},
	StatusInterrupted: {StatusQueued, StatusRunning, StatusCancelled, StatusFailed},
	StatusDone:        {},
	StatusFailed:      {},
	StatusCancelled:   {},
}

func TestEveryTransitionIsAssertedInBothDirections(t *testing.T) {
	if len(legal) != len(AllStatuses) {
		t.Fatalf("the table covers %d statuses, AllStatuses has %d", len(legal), len(AllStatuses))
	}
	for _, from := range AllStatuses {
		allowed, ok := legal[from]
		if !ok {
			t.Fatalf("status %q is missing from the test table", from)
		}
		for _, to := range AllStatuses {
			want := slices.Contains(allowed, to)
			t.Run(from+"->"+to, func(t *testing.T) {
				if got := CanTransition(from, to); got != want {
					t.Errorf("CanTransition = %v, want %v", got, want)
				}
				err := Transition(from, to)
				switch {
				case want && err != nil:
					t.Errorf("Transition rejected a legal move: %v", err)
				case !want && !errors.Is(err, ErrIllegalTransition):
					t.Errorf("Transition allowed an illegal move: err = %v", err)
				}
			})
		}
	}
}

func TestTransitionRejectsUnknownStatuses(t *testing.T) {
	tests := []struct{ from, to string }{
		{"nonsense", StatusRunning},
		{StatusQueued, "nonsense"},
		{"", ""},
		{StatusQueued, ""},
	}
	for _, tc := range tests {
		if err := Transition(tc.from, tc.to); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Transition(%q, %q) = %v, want ErrIllegalTransition", tc.from, tc.to, err)
		}
		if CanTransition(tc.from, tc.to) {
			t.Errorf("CanTransition(%q, %q) = true", tc.from, tc.to)
		}
	}
}

func TestTerminal(t *testing.T) {
	tests := map[string]bool{
		StatusDone: true, StatusFailed: true, StatusCancelled: true,
		StatusQueued: false, StatusRunning: false, StatusPaused: false,
		StatusInterrupted: false, "nonsense": false,
	}
	for status, want := range tests {
		if got := Terminal(status); got != want {
			t.Errorf("Terminal(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestSelfTransitionsAreRejected(t *testing.T) {
	for _, s := range AllStatuses {
		if CanTransition(s, s) {
			t.Errorf("%s → %s should not be legal", s, s)
		}
	}
}
