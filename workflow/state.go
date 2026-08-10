package workflow

// State is the lifecycle position of a workflow execution or of a single
// step within one.
//
// The same type describes both because they move through the same
// phases, which keeps the persisted model and the dashboard's rendering
// uniform. Transitions are guarded by [State.CanTransitionTo] so a
// concurrent update cannot move an execution backwards out of a terminal
// state.
type State uint8

const (
	// StatePending is the state of an execution that has been created
	// and persisted but whose first step has not started.
	StatePending State = iota

	// StateRunning means at least one step is executing.
	StateRunning

	// StateWaiting means execution is suspended between attempts —
	// a step failed and its retry backoff has not elapsed yet.
	StateWaiting

	// StateCompleted means every required step succeeded.
	StateCompleted

	// StateFailed means a step exhausted its attempts, or a step
	// failed with no compensation to run.
	StateFailed

	// StateCancelled means the execution stopped because its context
	// was cancelled, not because anything failed.
	StateCancelled

	// StateCompensating means a failure triggered rollback and the
	// compensation handlers are running.
	StateCompensating

	// StateCompensated means rollback finished successfully. The
	// original failure still stands; the side effects were undone.
	StateCompensated

	// StateTimedOut means the execution exceeded its own deadline.
	StateTimedOut
)

// stateNames is indexed by State, so String stays allocation-free.
var stateNames = [...]string{
	StatePending:      "pending",
	StateRunning:      "running",
	StateWaiting:      "waiting",
	StateCompleted:    "completed",
	StateFailed:       "failed",
	StateCancelled:    "cancelled",
	StateCompensating: "compensating",
	StateCompensated:  "compensated",
	StateTimedOut:     "timed_out",
}

// String returns the lower-case, underscore-separated name of the state.
// The values are stable: they are persisted and rendered by the
// dashboard, so they are part of the package's compatibility surface.
func (s State) String() string {
	if int(s) >= len(stateNames) {
		return "unknown"
	}
	return stateNames[s]
}

// Terminal reports whether the state is final. A terminal execution is
// never resumed and accepts no further transitions.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateCompensated, StateTimedOut:
		return true
	default:
		return false
	}
}

// transitions lists, for each state, the states it may move to.
//
// It is written out in full rather than derived from rules because the
// legality of a transition is a design decision worth reading at a
// glance: a reviewer can see immediately that nothing leaves a terminal
// state, and that compensation is reachable only from a failure.
var transitions = map[State][]State{
	StatePending:      {StateRunning, StateCancelled, StateTimedOut, StateFailed},
	StateRunning:      {StateWaiting, StateCompleted, StateFailed, StateCancelled, StateCompensating, StateTimedOut},
	StateWaiting:      {StateRunning, StateFailed, StateCancelled, StateCompensating, StateTimedOut},
	StateCompensating: {StateCompensated, StateFailed, StateCancelled},
	StateCompleted:    nil,
	StateFailed:       nil,
	StateCancelled:    nil,
	StateCompensated:  nil,
	StateTimedOut:     nil,
}

// CanTransitionTo reports whether moving from s to next is legal.
//
// A state may always "transition" to itself: re-persisting the current
// state is how a step records progress (an attempt counter, a timestamp)
// without changing phase.
func (s State) CanTransitionTo(next State) bool {
	if s == next {
		return true
	}
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}
