package agui

import "encoding/json"

// RunAgentInput is the request body a client POSTs to start (or resume) an
// agent run. The server responds with a text/event-stream SSE stream.
type RunAgentInput struct {
	ThreadID       string          `json:"threadId"`
	RunID          string          `json:"runId"`
	Messages       []Message       `json:"messages,omitempty"`
	State          json.RawMessage `json:"state,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	Context        []ContextItem   `json:"context,omitempty"`
	ForwardedProps json.RawMessage `json:"forwardedProps,omitempty"`
	// Resume carries the resolution of open interrupts when starting a
	// continuation run. All open interrupts must be addressed in one request.
	Resume []ResumeItem `json:"resume,omitempty"`
}

// Tool describes a tool the client makes available to the agent.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ContextItem is a supplemental context entry attached to a run.
type ContextItem struct {
	Description string `json:"description,omitempty"`
	Value       string `json:"value"`
}

// ResumeStatus is the resolution status of an interrupt in a resume request.
type ResumeStatus string

const (
	// ResumeResolved indicates the user answered; Payload carries the answer.
	ResumeResolved ResumeStatus = "resolved"
	// ResumeCancelled indicates the interrupt was abandoned.
	ResumeCancelled ResumeStatus = "cancelled"
)

// ResumeItem correlates a resume payload to a specific interrupt.
type ResumeItem struct {
	InterruptID string          `json:"interruptId"`
	Status      ResumeStatus    `json:"status"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// DecodeRunAgentInput parses a RunAgentInput from JSON.
func DecodeRunAgentInput(data []byte) (RunAgentInput, error) {
	var in RunAgentInput
	if err := json.Unmarshal(data, &in); err != nil {
		return RunAgentInput{}, err
	}
	return in, nil
}

// Encode serializes the RunAgentInput to JSON.
func (in RunAgentInput) Encode() ([]byte, error) {
	return json.Marshal(in)
}
