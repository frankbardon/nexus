package agui

// Constructors set the discriminator ("type") field on each event so callers
// never have to remember it. Fields beyond those in the signature may be set on
// the returned value directly.

// NewRunStarted builds a RunStarted event.
func NewRunStarted(threadID, runID string) RunStartedEvent {
	return RunStartedEvent{BaseEvent: newBase(EventRunStarted), ThreadID: threadID, RunID: runID}
}

// NewRunFinished builds a RunFinished event.
func NewRunFinished(threadID, runID string) RunFinishedEvent {
	return RunFinishedEvent{BaseEvent: newBase(EventRunFinished), ThreadID: threadID, RunID: runID}
}

// NewRunError builds a RunError event.
func NewRunError(message string) RunErrorEvent {
	return RunErrorEvent{BaseEvent: newBase(EventRunError), Message: message}
}

// NewStepStarted builds a StepStarted event.
func NewStepStarted(stepName string) StepStartedEvent {
	return StepStartedEvent{BaseEvent: newBase(EventStepStarted), StepName: stepName}
}

// NewStepFinished builds a StepFinished event.
func NewStepFinished(stepName string) StepFinishedEvent {
	return StepFinishedEvent{BaseEvent: newBase(EventStepFinished), StepName: stepName}
}

// NewTextMessageStart builds a TextMessageStart event.
func NewTextMessageStart(messageID, role string) TextMessageStartEvent {
	return TextMessageStartEvent{BaseEvent: newBase(EventTextMessageStart), MessageID: messageID, Role: role}
}

// NewTextMessageContent builds a TextMessageContent event.
func NewTextMessageContent(messageID, delta string) TextMessageContentEvent {
	return TextMessageContentEvent{BaseEvent: newBase(EventTextMessageContent), MessageID: messageID, Delta: delta}
}

// NewTextMessageEnd builds a TextMessageEnd event.
func NewTextMessageEnd(messageID string) TextMessageEndEvent {
	return TextMessageEndEvent{BaseEvent: newBase(EventTextMessageEnd), MessageID: messageID}
}

// NewTextMessageChunk builds a TextMessageChunk event.
func NewTextMessageChunk(messageID, delta string) TextMessageChunkEvent {
	return TextMessageChunkEvent{BaseEvent: newBase(EventTextMessageChunk), MessageID: messageID, Delta: delta}
}

// NewToolCallStart builds a ToolCallStart event.
func NewToolCallStart(toolCallID, toolCallName string) ToolCallStartEvent {
	return ToolCallStartEvent{BaseEvent: newBase(EventToolCallStart), ToolCallID: toolCallID, ToolCallName: toolCallName}
}

// NewToolCallArgs builds a ToolCallArgs event.
func NewToolCallArgs(toolCallID, delta string) ToolCallArgsEvent {
	return ToolCallArgsEvent{BaseEvent: newBase(EventToolCallArgs), ToolCallID: toolCallID, Delta: delta}
}

// NewToolCallEnd builds a ToolCallEnd event.
func NewToolCallEnd(toolCallID string) ToolCallEndEvent {
	return ToolCallEndEvent{BaseEvent: newBase(EventToolCallEnd), ToolCallID: toolCallID}
}

// NewToolCallResult builds a ToolCallResult event.
func NewToolCallResult(messageID, toolCallID, content string) ToolCallResultEvent {
	return ToolCallResultEvent{BaseEvent: newBase(EventToolCallResult), MessageID: messageID, ToolCallID: toolCallID, Content: content}
}

// NewToolCallChunk builds a ToolCallChunk event.
func NewToolCallChunk(toolCallID string) ToolCallChunkEvent {
	return ToolCallChunkEvent{BaseEvent: newBase(EventToolCallChunk), ToolCallID: toolCallID}
}

// NewStateSnapshot builds a StateSnapshot event.
func NewStateSnapshot(snapshot []byte) StateSnapshotEvent {
	return StateSnapshotEvent{BaseEvent: newBase(EventStateSnapshot), Snapshot: snapshot}
}

// NewStateDelta builds a StateDelta event carrying an RFC 6902 JSON Patch.
func NewStateDelta(delta JSONPatch) StateDeltaEvent {
	return StateDeltaEvent{BaseEvent: newBase(EventStateDelta), Delta: delta}
}

// NewMessagesSnapshot builds a MessagesSnapshot event.
func NewMessagesSnapshot(messages []Message) MessagesSnapshotEvent {
	return MessagesSnapshotEvent{BaseEvent: newBase(EventMessagesSnapshot), Messages: messages}
}

// NewReasoningStart builds a ReasoningStart event.
func NewReasoningStart() ReasoningStartEvent {
	return ReasoningStartEvent{BaseEvent: newBase(EventReasoningStart)}
}

// NewReasoningMessageContent builds a ReasoningMessageContent event.
func NewReasoningMessageContent(delta string) ReasoningMessageContentEvent {
	return ReasoningMessageContentEvent{BaseEvent: newBase(EventReasoningMessageContent), Delta: delta}
}

// NewReasoningEnd builds a ReasoningEnd event.
func NewReasoningEnd() ReasoningEndEvent {
	return ReasoningEndEvent{BaseEvent: newBase(EventReasoningEnd)}
}

// NewRaw builds a Raw pass-through event.
func NewRaw(event []byte) RawEvent {
	return RawEvent{BaseEvent: newBase(EventRaw), Event: event}
}

// NewCustom builds a Custom named-value event.
func NewCustom(name string, value []byte) CustomEvent {
	return CustomEvent{BaseEvent: newBase(EventCustom), Name: name, Value: value}
}
