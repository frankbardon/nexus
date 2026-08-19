package a2a

import "fmt"

// allTaskStates is every legal TaskState including the unspecified zero value,
// in proto declaration order.
var allTaskStates = []TaskState{
	TaskStateUnspecified,
	TaskStateSubmitted,
	TaskStateWorking,
	TaskStateCompleted,
	TaskStateFailed,
	TaskStateCanceled,
	TaskStateInputRequired,
	TaskStateRejected,
	TaskStateAuthRequired,
}

// TaskStates returns the eight addressable task states, excluding the
// unspecified zero value. The returned slice is a fresh copy.
func TaskStates() []TaskState {
	out := make([]TaskState, 0, len(allTaskStates)-1)
	out = append(out, allTaskStates[1:]...)
	return out
}

// Valid reports whether s is a recognized task state. The unspecified zero value
// is not valid for a live task.
func (s TaskState) Valid() bool {
	for _, known := range allTaskStates[1:] {
		if s == known {
			return true
		}
	}
	return false
}

// Known reports whether s is a recognized enum value, including the unspecified
// zero value.
func (s TaskState) Known() bool {
	for _, known := range allTaskStates {
		if s == known {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s is a terminal state. Once a task reaches one, it
// accepts no further messages, no further transitions, and any open stream must
// close (specification sections 3.1.1 and 11.7).
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	default:
		return false
	}
}

// IsInterrupted reports whether s is an interrupted (non-terminal) state, in
// which the task is paused awaiting something from the client. A blocking
// SendMessage returns when a task reaches one of these.
func (s TaskState) IsInterrupted() bool {
	switch s {
	case TaskStateInputRequired, TaskStateAuthRequired:
		return true
	default:
		return false
	}
}

// IsActive reports whether s is a live, non-terminal state: the task may still
// progress. Interrupted states are active.
func (s TaskState) IsActive() bool {
	return s.Valid() && !s.IsTerminal()
}

// Cancelable reports whether a task in state s may be canceled. Terminal tasks
// may not be; attempting it yields TaskNotCancelableError.
func (s TaskState) Cancelable() bool { return s.IsActive() }

// transitions is the legal task state graph. The key is the current state; the
// value set is every state that may follow it.
//
// TaskStateUnspecified stands for "no prior state" and models task creation: a
// task may first be observed as submitted, as already working, or as rejected
// outright at creation time. Terminal states have no successors at all, which is
// what makes ValidateTransition reject any move out of one.
//
// Self-transitions are permitted for active states because agents legitimately
// re-emit a state with a new status message (a WORKING progress note, a second
// INPUT_REQUIRED question) without changing state.
var transitions = map[TaskState]map[TaskState]bool{
	TaskStateUnspecified: {
		TaskStateSubmitted: true,
		TaskStateWorking:   true,
		TaskStateRejected:  true,
	},
	TaskStateSubmitted: {
		TaskStateSubmitted:     true,
		TaskStateWorking:       true,
		TaskStateInputRequired: true,
		TaskStateAuthRequired:  true,
		TaskStateCompleted:     true,
		TaskStateFailed:        true,
		TaskStateCanceled:      true,
		TaskStateRejected:      true,
	},
	TaskStateWorking: {
		TaskStateWorking:       true,
		TaskStateInputRequired: true,
		TaskStateAuthRequired:  true,
		TaskStateCompleted:     true,
		TaskStateFailed:        true,
		TaskStateCanceled:      true,
		TaskStateRejected:      true,
	},
	TaskStateInputRequired: {
		TaskStateInputRequired: true,
		TaskStateWorking:       true,
		TaskStateAuthRequired:  true,
		TaskStateCompleted:     true,
		TaskStateFailed:        true,
		TaskStateCanceled:      true,
		TaskStateRejected:      true,
	},
	TaskStateAuthRequired: {
		TaskStateAuthRequired:  true,
		TaskStateWorking:       true,
		TaskStateInputRequired: true,
		TaskStateCompleted:     true,
		TaskStateFailed:        true,
		TaskStateCanceled:      true,
		TaskStateRejected:      true,
	},
	TaskStateCompleted: {},
	TaskStateFailed:    {},
	TaskStateCanceled:  {},
	TaskStateRejected:  {},
}

// CanTransition reports whether a task may move from state from to state to.
func CanTransition(from, to TaskState) bool {
	return transitions[from][to]
}

// ValidateTransition checks a task state transition and returns a protocol error
// describing why it is illegal, or nil when it is legal.
//
// The rules enforced are:
//
//   - Both states must be recognized enum values.
//   - The destination may not be TASK_STATE_UNSPECIFIED; a live task always has
//     a concrete state.
//   - No transition may leave a terminal state, including back to itself.
//   - Active states may re-enter themselves, since an agent may re-emit a state
//     carrying a new status message.
func ValidateTransition(from, to TaskState) error {
	if !from.Known() {
		return NewError(ErrorTypeInvalidAgentResponse,
			fmt.Sprintf("unknown source task state %q", string(from)))
	}
	if !to.Known() {
		return NewError(ErrorTypeInvalidAgentResponse,
			fmt.Sprintf("unknown target task state %q", string(to)))
	}
	if to == TaskStateUnspecified {
		return NewError(ErrorTypeInvalidAgentResponse,
			"task state may not transition to TASK_STATE_UNSPECIFIED")
	}
	if from.IsTerminal() {
		return NewError(ErrorTypeUnsupportedOperation,
			fmt.Sprintf("task is in terminal state %s and cannot transition to %s", from.String(), to.String()))
	}
	if !CanTransition(from, to) {
		return NewError(ErrorTypeInvalidAgentResponse,
			fmt.Sprintf("illegal task state transition %s -> %s", from.String(), to.String()))
	}
	return nil
}

// ValidateTransitionSequence walks a full sequence of states, starting from
// TaskStateUnspecified, and returns the first illegal transition it finds. It is
// a convenience for tests and conformance checks.
func ValidateTransitionSequence(states []TaskState) error {
	current := TaskStateUnspecified
	for i, next := range states {
		if err := ValidateTransition(current, next); err != nil {
			return fmt.Errorf("a2a: transition %d: %w", i, err)
		}
		current = next
	}
	return nil
}
