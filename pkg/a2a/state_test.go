package a2a

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestTaskStateEnumValues pins the eight addressable 1.0 task states and their
// exact ProtoJSON spellings. A rename here is a wire break.
func TestTaskStateEnumValues(t *testing.T) {
	want := []TaskState{
		"TASK_STATE_SUBMITTED",
		"TASK_STATE_WORKING",
		"TASK_STATE_COMPLETED",
		"TASK_STATE_FAILED",
		"TASK_STATE_CANCELED",
		"TASK_STATE_INPUT_REQUIRED",
		"TASK_STATE_REJECTED",
		"TASK_STATE_AUTH_REQUIRED",
	}
	got := TaskStates()
	if len(got) != 8 {
		t.Fatalf("TaskStates() returned %d states, want the 8 addressable 1.0 states: %v", len(got), got)
	}
	seen := map[TaskState]bool{}
	for _, s := range got {
		seen[s] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("missing task state %q", w)
		}
	}
	if TaskStates()[0] == TaskStateUnspecified {
		t.Error("TaskStates() must exclude the unspecified zero value")
	}
}

// TestTaskStateClassification pins which states are terminal, interrupted and
// cancelable.
func TestTaskStateClassification(t *testing.T) {
	tests := []struct {
		state       TaskState
		valid       bool
		terminal    bool
		interrupted bool
		active      bool
		cancelable  bool
	}{
		{state: TaskStateUnspecified, valid: false},
		{state: TaskStateSubmitted, valid: true, active: true, cancelable: true},
		{state: TaskStateWorking, valid: true, active: true, cancelable: true},
		{state: TaskStateInputRequired, valid: true, interrupted: true, active: true, cancelable: true},
		{state: TaskStateAuthRequired, valid: true, interrupted: true, active: true, cancelable: true},
		{state: TaskStateCompleted, valid: true, terminal: true},
		{state: TaskStateFailed, valid: true, terminal: true},
		{state: TaskStateCanceled, valid: true, terminal: true},
		{state: TaskStateRejected, valid: true, terminal: true},
		{state: TaskState("TASK_STATE_BOGUS"), valid: false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.state.IsTerminal(); got != tc.terminal {
				t.Errorf("IsTerminal() = %v, want %v", got, tc.terminal)
			}
			if got := tc.state.IsInterrupted(); got != tc.interrupted {
				t.Errorf("IsInterrupted() = %v, want %v", got, tc.interrupted)
			}
			if got := tc.state.IsActive(); got != tc.active {
				t.Errorf("IsActive() = %v, want %v", got, tc.active)
			}
			if got := tc.state.Cancelable(); got != tc.cancelable {
				t.Errorf("Cancelable() = %v, want %v", got, tc.cancelable)
			}
		})
	}
}

// TestValidateTransition covers the legal and illegal moves in the task state
// graph, with the terminal-state rule as the headline case.
func TestValidateTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    TaskState
		to      TaskState
		wantErr bool
		// wantType is the expected error taxonomy entry when wantErr is set.
		wantType ErrorType
	}{
		// Creation.
		{name: "create as submitted", from: TaskStateUnspecified, to: TaskStateSubmitted},
		{name: "create as working", from: TaskStateUnspecified, to: TaskStateWorking},
		{name: "reject at creation", from: TaskStateUnspecified, to: TaskStateRejected},
		{
			name: "cannot be created already complete", from: TaskStateUnspecified, to: TaskStateCompleted,
			wantErr: true, wantType: ErrorTypeInvalidAgentResponse,
		},

		// Happy path.
		{name: "submitted to working", from: TaskStateSubmitted, to: TaskStateWorking},
		{name: "working to completed", from: TaskStateWorking, to: TaskStateCompleted},
		{name: "working to failed", from: TaskStateWorking, to: TaskStateFailed},
		{name: "working to canceled", from: TaskStateWorking, to: TaskStateCanceled},

		// Interruption and resume.
		{name: "working to input required", from: TaskStateWorking, to: TaskStateInputRequired},
		{name: "input required back to working", from: TaskStateInputRequired, to: TaskStateWorking},
		{name: "working to auth required", from: TaskStateWorking, to: TaskStateAuthRequired},
		{name: "auth required back to working", from: TaskStateAuthRequired, to: TaskStateWorking},
		{name: "input required can be canceled", from: TaskStateInputRequired, to: TaskStateCanceled},

		// Self transitions on active states are legal: an agent re-emits a
		// state with a fresh status message.
		{name: "working to working", from: TaskStateWorking, to: TaskStateWorking},
		{name: "input required to input required", from: TaskStateInputRequired, to: TaskStateInputRequired},

		// Terminal states are absorbing. This is the acceptance criterion.
		{
			name: "completed to working", from: TaskStateCompleted, to: TaskStateWorking,
			wantErr: true, wantType: ErrorTypeUnsupportedOperation,
		},
		{
			name: "failed to working", from: TaskStateFailed, to: TaskStateWorking,
			wantErr: true, wantType: ErrorTypeUnsupportedOperation,
		},
		{
			name: "canceled to completed", from: TaskStateCanceled, to: TaskStateCompleted,
			wantErr: true, wantType: ErrorTypeUnsupportedOperation,
		},
		{
			name: "rejected to submitted", from: TaskStateRejected, to: TaskStateSubmitted,
			wantErr: true, wantType: ErrorTypeUnsupportedOperation,
		},
		{
			name: "completed to completed", from: TaskStateCompleted, to: TaskStateCompleted,
			wantErr: true, wantType: ErrorTypeUnsupportedOperation,
		},

		// Malformed.
		{
			name: "to unspecified", from: TaskStateWorking, to: TaskStateUnspecified,
			wantErr: true, wantType: ErrorTypeInvalidAgentResponse,
		},
		{
			name: "unknown source", from: TaskState("TASK_STATE_BOGUS"), to: TaskStateWorking,
			wantErr: true, wantType: ErrorTypeInvalidAgentResponse,
		},
		{
			name: "unknown target", from: TaskStateWorking, to: TaskState("TASK_STATE_BOGUS"),
			wantErr: true, wantType: ErrorTypeInvalidAgentResponse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransition(tc.from, tc.to)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateTransition(%s, %s) = %v, want nil", tc.from, tc.to, err)
				}
				if !CanTransition(tc.from, tc.to) {
					t.Fatalf("CanTransition(%s, %s) = false, disagrees with ValidateTransition", tc.from, tc.to)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateTransition(%s, %s) = nil, want an error", tc.from, tc.to)
			}
			var protoErr *Error
			if !errors.As(err, &protoErr) {
				t.Fatalf("error %v is not an *a2a.Error", err)
			}
			if protoErr.Type != tc.wantType {
				t.Fatalf("error type = %q, want %q", protoErr.Type, tc.wantType)
			}
		})
	}
}

// TestNoTransitionEscapesTerminalState is the exhaustive form of the headline
// rule: from every terminal state, every target is rejected.
func TestNoTransitionEscapesTerminalState(t *testing.T) {
	for _, from := range TaskStates() {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range TaskStates() {
			if err := ValidateTransition(from, to); err == nil {
				t.Errorf("ValidateTransition(%s, %s) = nil, terminal states must be absorbing", from, to)
			}
		}
	}
}

// TestValidateTransitionSequence walks whole lifecycles.
func TestValidateTransitionSequence(t *testing.T) {
	tests := []struct {
		name    string
		states  []TaskState
		wantErr bool
	}{
		{
			name:   "simple completion",
			states: []TaskState{TaskStateSubmitted, TaskStateWorking, TaskStateCompleted},
		},
		{
			name: "multi-turn with input required",
			states: []TaskState{
				TaskStateSubmitted, TaskStateWorking, TaskStateInputRequired,
				TaskStateWorking, TaskStateCompleted,
			},
		},
		{
			name:   "rejected outright",
			states: []TaskState{TaskStateRejected},
		},
		{
			name:    "work resumed after completion",
			states:  []TaskState{TaskStateSubmitted, TaskStateWorking, TaskStateCompleted, TaskStateWorking},
			wantErr: true,
		},
		{
			name:    "starts already completed",
			states:  []TaskState{TaskStateCompleted},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransitionSequence(tc.states)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateTransitionSequence = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestTaskStateJSON confirms states travel as their bare ProtoJSON names.
func TestTaskStateJSON(t *testing.T) {
	data, err := json.Marshal(TaskStatus{State: TaskStateInputRequired})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"state":"TASK_STATE_INPUT_REQUIRED"}`
	if string(data) != want {
		t.Fatalf("= %s, want %s", data, want)
	}
}

// TestTaskStateString covers the log-facing short form.
func TestTaskStateString(t *testing.T) {
	if got := TaskStateInputRequired.String(); got != "INPUT_REQUIRED" {
		t.Fatalf("= %q, want %q", got, "INPUT_REQUIRED")
	}
}

// TestRoleValid pins the two addressable roles.
func TestRoleValid(t *testing.T) {
	tests := []struct {
		role Role
		want bool
	}{
		{RoleUser, true},
		{RoleAgent, true},
		{RoleUnspecified, false},
		{Role("user"), false},
		{Role(""), false},
	}
	for _, tc := range tests {
		if got := tc.role.Valid(); got != tc.want {
			t.Errorf("Role(%q).Valid() = %v, want %v", tc.role, got, tc.want)
		}
	}
}
