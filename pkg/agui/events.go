package agui

import (
	"encoding/json"
	"fmt"
)

// EventType is the discriminator carried in every AG-UI event's "type" field.
type EventType string

// Canonical AG-UI event types.
const (
	// Lifecycle.
	EventRunStarted   EventType = "RunStarted"
	EventRunFinished  EventType = "RunFinished"
	EventRunError     EventType = "RunError"
	EventStepStarted  EventType = "StepStarted"
	EventStepFinished EventType = "StepFinished"

	// Text message.
	EventTextMessageStart   EventType = "TextMessageStart"
	EventTextMessageContent EventType = "TextMessageContent"
	EventTextMessageEnd     EventType = "TextMessageEnd"
	EventTextMessageChunk   EventType = "TextMessageChunk"

	// Tool call.
	EventToolCallStart  EventType = "ToolCallStart"
	EventToolCallArgs   EventType = "ToolCallArgs"
	EventToolCallEnd    EventType = "ToolCallEnd"
	EventToolCallResult EventType = "ToolCallResult"
	EventToolCallChunk  EventType = "ToolCallChunk"

	// State.
	EventStateSnapshot    EventType = "StateSnapshot"
	EventStateDelta       EventType = "StateDelta"
	EventMessagesSnapshot EventType = "MessagesSnapshot"

	// Activity.
	EventActivitySnapshot EventType = "ActivitySnapshot"
	EventActivityDelta    EventType = "ActivityDelta"

	// Reasoning (thinking).
	EventReasoningStart          EventType = "ReasoningStart"
	EventReasoningMessageStart   EventType = "ReasoningMessageStart"
	EventReasoningMessageContent EventType = "ReasoningMessageContent"
	EventReasoningMessageEnd     EventType = "ReasoningMessageEnd"
	EventReasoningMessageChunk   EventType = "ReasoningMessageChunk"
	EventReasoningEnd            EventType = "ReasoningEnd"
	EventReasoningEncryptedValue EventType = "ReasoningEncryptedValue"

	// Special.
	EventRaw    EventType = "Raw"
	EventCustom EventType = "Custom"
	EventMeta   EventType = "MetaEvent"
)

// Event is implemented by every AG-UI event struct. Type returns the value
// carried in the "type" JSON field.
type Event interface {
	EventType() EventType
}

// BaseEvent carries fields common to every AG-UI event. It is embedded into
// each concrete event struct.
type BaseEvent struct {
	Type EventType `json:"type"`
	// Timestamp is an optional epoch-millisecond timestamp.
	Timestamp *int64 `json:"timestamp,omitempty"`
	// RawEvent optionally carries the provider-native event this was derived from.
	RawEvent json.RawMessage `json:"rawEvent,omitempty"`
}

// EventType returns the discriminator carried by the event.
func (b BaseEvent) EventType() EventType { return b.Type }

// ---- Lifecycle ----

// RunStartedEvent signals the beginning of an agent run.
type RunStartedEvent struct {
	BaseEvent
	ThreadID    string          `json:"threadId"`
	RunID       string          `json:"runId"`
	ParentRunID string          `json:"parentRunId,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
}

// RunFinishedEvent signals the end of an agent run and terminates the SSE stream.
type RunFinishedEvent struct {
	BaseEvent
	ThreadID string          `json:"threadId,omitempty"`
	RunID    string          `json:"runId,omitempty"`
	Outcome  string          `json:"outcome,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// RunErrorEvent reports a fatal error for the run.
type RunErrorEvent struct {
	BaseEvent
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// StepStartedEvent marks the start of a named step within a run.
type StepStartedEvent struct {
	BaseEvent
	StepName string `json:"stepName"`
}

// StepFinishedEvent marks the end of a named step within a run.
type StepFinishedEvent struct {
	BaseEvent
	StepName string `json:"stepName"`
}

// ---- Text message ----

// TextMessageStartEvent opens a streamed text message.
type TextMessageStartEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
}

// TextMessageContentEvent carries an incremental text delta.
type TextMessageContentEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

// TextMessageEndEvent closes a streamed text message.
type TextMessageEndEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
}

// TextMessageChunkEvent is the combined start/content/end form for a text message.
type TextMessageChunkEvent struct {
	BaseEvent
	MessageID string `json:"messageId,omitempty"`
	Role      string `json:"role,omitempty"`
	Delta     string `json:"delta,omitempty"`
}

// ---- Tool call ----

// ToolCallStartEvent opens a streamed tool call.
type ToolCallStartEvent struct {
	BaseEvent
	ToolCallID      string `json:"toolCallId"`
	ToolCallName    string `json:"toolCallName"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
}

// ToolCallArgsEvent carries an incremental tool-argument delta.
type ToolCallArgsEvent struct {
	BaseEvent
	ToolCallID string `json:"toolCallId"`
	Delta      string `json:"delta"`
}

// ToolCallEndEvent closes a streamed tool call.
type ToolCallEndEvent struct {
	BaseEvent
	ToolCallID string `json:"toolCallId"`
}

// ToolCallResultEvent reports the result of a tool call.
type ToolCallResultEvent struct {
	BaseEvent
	MessageID  string `json:"messageId"`
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"`
	Role       string `json:"role,omitempty"`
}

// ToolCallChunkEvent is the combined start/args/end form for a tool call.
type ToolCallChunkEvent struct {
	BaseEvent
	ToolCallID      string `json:"toolCallId,omitempty"`
	ToolCallName    string `json:"toolCallName,omitempty"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
	Delta           string `json:"delta,omitempty"`
}

// ---- State ----

// StateSnapshotEvent carries a full snapshot of the shared state.
type StateSnapshotEvent struct {
	BaseEvent
	Snapshot json.RawMessage `json:"snapshot"`
}

// StateDeltaEvent carries an incremental state mutation as an RFC 6902 JSON Patch.
type StateDeltaEvent struct {
	BaseEvent
	Delta JSONPatch `json:"delta"`
}

// MessagesSnapshotEvent carries a full snapshot of the conversation messages.
type MessagesSnapshotEvent struct {
	BaseEvent
	Messages []Message `json:"messages"`
}

// ---- Activity ----

// ActivitySnapshotEvent carries a full snapshot of an activity stream.
type ActivitySnapshotEvent struct {
	BaseEvent
	MessageID    string          `json:"messageId"`
	ActivityType string          `json:"activityType"`
	Content      json.RawMessage `json:"content"`
	Replace      bool            `json:"replace,omitempty"`
}

// ActivityDeltaEvent carries an incremental activity mutation as a JSON Patch.
type ActivityDeltaEvent struct {
	BaseEvent
	MessageID    string    `json:"messageId"`
	ActivityType string    `json:"activityType"`
	Patch        JSONPatch `json:"patch"`
}

// ---- Reasoning (thinking) ----

// ReasoningStartEvent opens a reasoning (thinking) section.
type ReasoningStartEvent struct {
	BaseEvent
}

// ReasoningMessageStartEvent opens a streamed reasoning message.
type ReasoningMessageStartEvent struct {
	BaseEvent
	MessageID string `json:"messageId,omitempty"`
	Role      string `json:"role,omitempty"`
}

// ReasoningMessageContentEvent carries an incremental reasoning delta.
type ReasoningMessageContentEvent struct {
	BaseEvent
	MessageID string `json:"messageId,omitempty"`
	Delta     string `json:"delta"`
}

// ReasoningMessageEndEvent closes a streamed reasoning message.
type ReasoningMessageEndEvent struct {
	BaseEvent
	MessageID string `json:"messageId,omitempty"`
}

// ReasoningMessageChunkEvent is the combined form for a reasoning message.
type ReasoningMessageChunkEvent struct {
	BaseEvent
	MessageID string `json:"messageId,omitempty"`
	Role      string `json:"role,omitempty"`
	Delta     string `json:"delta,omitempty"`
}

// ReasoningEndEvent closes a reasoning (thinking) section.
type ReasoningEndEvent struct {
	BaseEvent
}

// ReasoningEncryptedValueEvent carries an opaque encrypted reasoning payload.
type ReasoningEncryptedValueEvent struct {
	BaseEvent
	Value string `json:"value"`
}

// ---- Special ----

// RawEvent wraps a provider-native event for pass-through.
type RawEvent struct {
	BaseEvent
	Event  json.RawMessage `json:"event"`
	Source string          `json:"source,omitempty"`
}

// CustomEvent carries an application-defined named value. Nexus rides
// non-canonical bus events (workflow progress, code-exec stdout, etc.) on this.
type CustomEvent struct {
	BaseEvent
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

// MetaEvent is a draft meta-event carrier.
type MetaEvent struct {
	BaseEvent
	Name  string          `json:"name,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ---- Shared value types ----

// Message is a single conversation message as carried in MessagesSnapshot and
// RunAgentInput.
type Message struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolCalls  []ToolCall      `json:"toolCalls,omitempty"`
	RawContent json.RawMessage `json:"rawContent,omitempty"`
}

// ToolCall describes a tool invocation attached to an assistant message.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the function payload of a ToolCall.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// JSONPatch is an RFC 6902 JSON Patch document (an ordered array of operations).
type JSONPatch []JSONPatchOp

// JSONPatchOp is a single RFC 6902 operation.
type JSONPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// newBase constructs a BaseEvent for the given type.
func newBase(t EventType) BaseEvent { return BaseEvent{Type: t} }

// EventType assertions (compile-time interface satisfaction).
var (
	_ Event = RunStartedEvent{}
	_ Event = RunFinishedEvent{}
	_ Event = RunErrorEvent{}
	_ Event = StepStartedEvent{}
	_ Event = StepFinishedEvent{}
	_ Event = TextMessageStartEvent{}
	_ Event = TextMessageContentEvent{}
	_ Event = TextMessageEndEvent{}
	_ Event = TextMessageChunkEvent{}
	_ Event = ToolCallStartEvent{}
	_ Event = ToolCallArgsEvent{}
	_ Event = ToolCallEndEvent{}
	_ Event = ToolCallResultEvent{}
	_ Event = ToolCallChunkEvent{}
	_ Event = StateSnapshotEvent{}
	_ Event = StateDeltaEvent{}
	_ Event = MessagesSnapshotEvent{}
	_ Event = ActivitySnapshotEvent{}
	_ Event = ActivityDeltaEvent{}
	_ Event = ReasoningStartEvent{}
	_ Event = ReasoningMessageStartEvent{}
	_ Event = ReasoningMessageContentEvent{}
	_ Event = ReasoningMessageEndEvent{}
	_ Event = ReasoningMessageChunkEvent{}
	_ Event = ReasoningEndEvent{}
	_ Event = ReasoningEncryptedValueEvent{}
	_ Event = RawEvent{}
	_ Event = CustomEvent{}
	_ Event = MetaEvent{}
)

// eventPrototype returns a pointer to a zero value of the concrete event struct
// registered for the given type, or an error for unknown types.
func eventPrototype(t EventType) (Event, error) {
	switch t {
	case EventRunStarted:
		return &RunStartedEvent{}, nil
	case EventRunFinished:
		return &RunFinishedEvent{}, nil
	case EventRunError:
		return &RunErrorEvent{}, nil
	case EventStepStarted:
		return &StepStartedEvent{}, nil
	case EventStepFinished:
		return &StepFinishedEvent{}, nil
	case EventTextMessageStart:
		return &TextMessageStartEvent{}, nil
	case EventTextMessageContent:
		return &TextMessageContentEvent{}, nil
	case EventTextMessageEnd:
		return &TextMessageEndEvent{}, nil
	case EventTextMessageChunk:
		return &TextMessageChunkEvent{}, nil
	case EventToolCallStart:
		return &ToolCallStartEvent{}, nil
	case EventToolCallArgs:
		return &ToolCallArgsEvent{}, nil
	case EventToolCallEnd:
		return &ToolCallEndEvent{}, nil
	case EventToolCallResult:
		return &ToolCallResultEvent{}, nil
	case EventToolCallChunk:
		return &ToolCallChunkEvent{}, nil
	case EventStateSnapshot:
		return &StateSnapshotEvent{}, nil
	case EventStateDelta:
		return &StateDeltaEvent{}, nil
	case EventMessagesSnapshot:
		return &MessagesSnapshotEvent{}, nil
	case EventActivitySnapshot:
		return &ActivitySnapshotEvent{}, nil
	case EventActivityDelta:
		return &ActivityDeltaEvent{}, nil
	case EventReasoningStart:
		return &ReasoningStartEvent{}, nil
	case EventReasoningMessageStart:
		return &ReasoningMessageStartEvent{}, nil
	case EventReasoningMessageContent:
		return &ReasoningMessageContentEvent{}, nil
	case EventReasoningMessageEnd:
		return &ReasoningMessageEndEvent{}, nil
	case EventReasoningMessageChunk:
		return &ReasoningMessageChunkEvent{}, nil
	case EventReasoningEnd:
		return &ReasoningEndEvent{}, nil
	case EventReasoningEncryptedValue:
		return &ReasoningEncryptedValueEvent{}, nil
	case EventRaw:
		return &RawEvent{}, nil
	case EventCustom:
		return &CustomEvent{}, nil
	case EventMeta:
		return &MetaEvent{}, nil
	default:
		return nil, fmt.Errorf("agui: unknown event type %q", t)
	}
}
