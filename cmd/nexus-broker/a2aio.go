package main

import (
	"encoding/json"
	"fmt"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// This file is the broker's half of the instance IO contract: the payload
// carried inside a brokerframe.SignalIO frame, which until now this binary
// forwarded without ever looking at.
//
// THE CONTRACT AND ITS RISK. The payload's authoritative definition is the
// unexported `ioMessage` in plugins/io/broker/server.go, which is what an
// instance actually encodes. That type cannot be imported (it is unexported,
// and importing a plugin into this binary would drag the whole engine in), so
// this is a MIRROR — and a mirror is exactly the kind of thing that falls
// silently out of step when somebody adds a field on the other side. That is
// the recorded risk of teaching the broker to read these payloads at all.
//
// It is answered by a test rather than by discipline: a2aio_contract_test.go
// parses plugins/io/broker/server.go and fails if the two structs disagree on
// any field's name, Go type or JSON tag. A field added, renamed or retyped over
// there breaks the build over here, naming the field.
//
// Decoding is deliberately LENIENT about content: a payload carrying a field
// this broker does not know is decoded and the field ignored, and a payload
// whose `type` this broker does not handle is logged and dropped. Neither
// fails a task. An instance is free to be newer than the broker in front of it.

// brokerIOMessage mirrors plugins/io/broker's ioMessage field for field.
//
// It is a flat union: only the fields relevant to a given Type are populated,
// and omitempty keeps frames compact. The field ORDER matches the original so a
// reviewer can diff the two by eye, and so the contract test's positional
// comparison reports a reordering as the change it is.
type brokerIOMessage struct {
	Type string `json:"type"`

	// Common output/streaming fields.
	TurnID  string `json:"turn_id,omitempty"`
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`

	// stream.end
	FinishReason string `json:"finish_reason,omitempty"`

	// status
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`

	// approval.request / approval.response
	PromptID    string `json:"prompt_id,omitempty"`
	Description string `json:"description,omitempty"`
	ToolCall    string `json:"tool_call,omitempty"`
	Risk        string `json:"risk,omitempty"`
	Approved    bool   `json:"approved,omitempty"`
	Always      bool   `json:"always,omitempty"`

	// hitl.request / hitl.response
	RequestID string           `json:"request_id,omitempty"`
	Prompt    string           `json:"prompt,omitempty"`
	Mode      string           `json:"mode,omitempty"`
	Choices   []brokerIOChoice `json:"choices,omitempty"`
	ChoiceID  string           `json:"choice_id,omitempty"`
	FreeText  string           `json:"free_text,omitempty"`

	// cancel.complete (instance -> broker). Pointer so "not set" is
	// distinguishable from an explicit false.
	Resumable *bool `json:"resumable,omitempty"`

	// cancel (broker -> instance)
	Source string `json:"source,omitempty"`
}

// brokerIOChoice mirrors plugins/io/broker's ioChoice: one option of a
// multiple-choice question, whose ID is what an answer echoes back.
type brokerIOChoice struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// The `type` values this broker understands. They are the vocabulary of the
// instance IO envelope; anything else is another transport's business and is
// ignored rather than refused.
const (
	// Broker -> instance.
	ioTypeInput            = "input"
	ioTypeHITLResponse     = "hitl.response"
	ioTypeApprovalResponse = "approval.response"
	ioTypeCancel           = "cancel"

	// Instance -> broker.
	ioTypeOutput          = "output"
	ioTypeStreamDelta     = "stream.delta"
	ioTypeStreamEnd       = "stream.end"
	ioTypeStatus          = "status"
	ioTypeApprovalRequest = "approval.request"
	ioTypeHITLRequest     = "hitl.request"
	ioTypeCancelComplete  = "cancel.complete"
)

// ioStateIdle is the io.status state every shipped Nexus agent loop reports
// when its turn is over (see plugins/agents/react, planexec, orchestrator).
//
// It is the ONLY end-of-turn signal the IO envelope carries: agent.turn.end is
// not forwarded by nexus.io.broker, and llm.stream.end fires once per model
// response rather than once per turn. That is why the A2A task completes here
// rather than at a stream end — see a2aTask.onStatus.
const ioStateIdle = "idle"

// ioCancelSource labels a cancellation this broker originates, alongside the
// "tui" / "browser" / "a2a" sources the other transports use.
const ioCancelSource = "broker.a2a"

// encodeIOFrame renders one IO payload as the brokerframe envelope an instance
// reads.
//
// It stamps SignalIO and nothing else: Frame.Secret is meaningless on an IO
// frame and must never be populated on one, and the version is stamped by
// brokerframe.Encode. No wire change is implied by this file — the envelope is
// exactly the one the gateway has always forwarded, read rather than relayed.
func encodeIOFrame(leaseID string, msg brokerIOMessage) ([]byte, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encoding %s io payload: %w", msg.Type, err)
	}
	data, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding io frame: %w", err)
	}
	return data, nil
}

// decodeIOPayload decodes the payload of a SignalIO frame.
//
// Unknown FIELDS are ignored (no DisallowUnknownFields, on purpose): an
// instance newer than the broker in front of it must keep working, and a task
// must not fail because a field it never needed appeared. Unknown TYPES are
// decoded successfully and dispatched to nothing — see a2aTask.deliver.
//
// A payload that is not an object at all is a real defect and is reported, so a
// mis-framed sender is diagnosable rather than silently inert.
func decodeIOPayload(payload json.RawMessage) (brokerIOMessage, error) {
	if len(payload) == 0 {
		return brokerIOMessage{}, fmt.Errorf("io frame carried an empty payload")
	}
	var msg brokerIOMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return brokerIOMessage{}, fmt.Errorf("decoding io payload: %w", err)
	}
	return msg, nil
}

// choiceIDs projects a question's options onto the ids an answer may echo.
func (m brokerIOMessage) choiceIDs() []string {
	if len(m.Choices) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.Choices))
	for _, c := range m.Choices {
		if c.ID != "" {
			out = append(out, c.ID)
		}
	}
	return out
}
