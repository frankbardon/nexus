package agui

import "encoding/json"

// RunFinished outcome discriminators. The AG-UI terminal-run model ends a run
// with an outcome; an interrupt outcome signals the client that the agent is
// awaiting a resume (see concepts/interrupts). A cancelled outcome marks a run
// that was abandoned before completion.
const (
	// OutcomeInterrupt marks a RunFinished that ends the run because the agent
	// is awaiting human (or client-executed-tool) input. The Result carries an
	// Interrupt payload the client renders and later resolves via the resume[]
	// field of a continuation RunAgentInput.
	OutcomeInterrupt = "interrupt"
	// OutcomeCancelled marks a RunFinished for a run terminated before it
	// completed (e.g. the pending interrupt was retracted).
	OutcomeCancelled = "cancelled"
)

// InterruptMode mirrors the response shape an interrupt accepts so the client
// can render the correct prompt affordance. Values align with the Nexus HITL
// modes (free_text, choices, both) but are duplicated here to keep pkg/agui
// free of any dependency on the engine or events packages.
type InterruptMode string

const (
	InterruptModeFreeText InterruptMode = "free_text"
	InterruptModeChoices  InterruptMode = "choices"
	InterruptModeBoth     InterruptMode = "both"
)

// InterruptChoice is one option the client presents to the user when an
// interrupt is a choice (or both) mode.
type InterruptChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind,omitempty"`
}

// Interrupt is the payload carried in a RunFinished(interrupt) Result. It gives
// the client everything needed to render a prompt and, on resume, to correlate
// its resolution back to the pending server-side request via InterruptID.
type Interrupt struct {
	// InterruptID uniquely identifies this interrupt for the lifetime of the
	// pending request. The client echoes it in ResumeItem.InterruptID.
	InterruptID string `json:"interruptId"`
	// Prompt is the rendered question/approval text to show the user.
	Prompt string `json:"prompt,omitempty"`
	// Mode controls whether the response is freeform, a choice, or either.
	Mode InterruptMode `json:"mode,omitempty"`
	// Choices lists the options when Mode is choices or both.
	Choices []InterruptChoice `json:"choices,omitempty"`
	// DefaultChoiceID is the choice applied if the user does not answer (the
	// server-side deadline default).
	DefaultChoiceID string `json:"defaultChoiceId,omitempty"`
}

// NewRunFinishedInterrupt builds a RunFinished event carrying an interrupt
// outcome. The Interrupt payload is JSON-encoded into Result. A marshalling
// failure yields a null Result rather than a partial event; the outcome
// discriminator still tells the client the run was interrupted.
func NewRunFinishedInterrupt(threadID, runID string, in Interrupt) RunFinishedEvent {
	ev := RunFinishedEvent{
		BaseEvent: newBase(EventRunFinished),
		ThreadID:  threadID,
		RunID:     runID,
		Outcome:   OutcomeInterrupt,
	}
	if raw, err := json.Marshal(in); err == nil {
		ev.Result = raw
	}
	return ev
}

// NewRunFinishedCancelled builds a RunFinished event carrying a cancelled
// outcome, used when a pending interrupt is retracted before it is resolved.
func NewRunFinishedCancelled(threadID, runID string) RunFinishedEvent {
	return RunFinishedEvent{
		BaseEvent: newBase(EventRunFinished),
		ThreadID:  threadID,
		RunID:     runID,
		Outcome:   OutcomeCancelled,
	}
}
