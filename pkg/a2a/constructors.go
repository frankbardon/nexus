package a2a

import (
	"encoding/json"
	"fmt"
	"time"
)

// Constructors keep the flattened Part oneof and the required-field rules in one
// place so callers never hand-assemble a partially-valid object.

// TextPart builds a text Part.
func TextPart(text string) Part {
	return Part{Text: &text}
}

// URLPart builds a Part referencing externally-hosted content. An empty
// mediaType is omitted.
func URLPart(u, mediaType string) Part {
	return Part{URL: &u, MediaType: mediaType}
}

// RawPart builds a Part carrying inline binary content, which serializes as
// base64. An empty mediaType or filename is omitted.
func RawPart(raw []byte, mediaType, filename string) Part {
	return Part{Raw: raw, MediaType: mediaType, Filename: filename}
}

// DataPart builds a Part carrying arbitrary structured JSON. The value is
// marshaled immediately so malformed data fails at construction rather than on
// the wire.
func DataPart(value any) (Part, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Part{}, fmt.Errorf("a2a: encode data part: %w", err)
	}
	return Part{Data: json.RawMessage(raw)}, nil
}

// NewMessage builds a Message with the required messageId, role and parts.
func NewMessage(messageID string, role Role, parts ...Part) Message {
	return Message{MessageID: messageID, Role: role, Parts: parts}
}

// NewUserMessage builds a user-role text message.
func NewUserMessage(messageID, text string) Message {
	return NewMessage(messageID, RoleUser, TextPart(text))
}

// NewAgentMessage builds an agent-role text message.
func NewAgentMessage(messageID, text string) Message {
	return NewMessage(messageID, RoleAgent, TextPart(text))
}

// InContext stamps a context id onto the message and returns it, for chaining.
func (m Message) InContext(contextID string) Message {
	m.ContextID = contextID
	return m
}

// ForTask stamps a task id onto the message and returns it, for chaining. This
// is how a client resumes an interrupted task: a new message carrying the same
// taskId and contextId (specification section 3.4).
func (m Message) ForTask(taskID string) Message {
	m.TaskID = taskID
	return m
}

// NewArtifact builds an Artifact with the required artifactId and parts.
func NewArtifact(artifactID string, parts ...Part) Artifact {
	return Artifact{ArtifactID: artifactID, Parts: parts}
}

// NewTextArtifact builds a named Artifact holding a single text part.
func NewTextArtifact(artifactID, name, text string) Artifact {
	return Artifact{ArtifactID: artifactID, Name: name, Parts: []Part{TextPart(text)}}
}

// NewTaskStatus builds a TaskStatus stamped with the current time.
func NewTaskStatus(state TaskState) TaskStatus {
	return TaskStatus{State: state, Timestamp: NewTimestamp(time.Now())}
}

// NewTaskStatusAt builds a TaskStatus stamped with a specific time, for
// deterministic tests and for replaying recorded histories.
func NewTaskStatusAt(state TaskState, at time.Time) TaskStatus {
	return TaskStatus{State: state, Timestamp: NewTimestamp(at)}
}

// WithMessage attaches a status message and returns the status, for chaining.
// An INPUT_REQUIRED question travels this way.
func (s TaskStatus) WithMessage(m Message) TaskStatus {
	s.Message = &m
	return s
}

// NewTask builds a Task in the submitted state.
func NewTask(id, contextID string) Task {
	return Task{ID: id, ContextID: contextID, Status: NewTaskStatus(TaskStateSubmitted)}
}

// NewStatusUpdate builds a TaskStatusUpdateEvent.
func NewStatusUpdate(taskID, contextID string, status TaskStatus) TaskStatusUpdateEvent {
	return TaskStatusUpdateEvent{TaskID: taskID, ContextID: contextID, Status: status}
}

// NewArtifactUpdate builds a TaskArtifactUpdateEvent delivering a whole
// artifact in one frame.
func NewArtifactUpdate(taskID, contextID string, artifact Artifact) TaskArtifactUpdateEvent {
	return TaskArtifactUpdateEvent{TaskID: taskID, ContextID: contextID, Artifact: artifact, LastChunk: true}
}

// NewArtifactChunk builds a TaskArtifactUpdateEvent delivering one chunk of a
// streamed artifact. Chunks after the first set append; the final chunk sets
// lastChunk.
func NewArtifactChunk(taskID, contextID string, artifact Artifact, appendChunk, last bool) TaskArtifactUpdateEvent {
	return TaskArtifactUpdateEvent{
		TaskID:    taskID,
		ContextID: contextID,
		Artifact:  artifact,
		Append:    appendChunk,
		LastChunk: last,
	}
}

// TaskResponse wraps a task as a SendMessageResponse.
func TaskResponse(t Task) SendMessageResponse { return SendMessageResponse{Task: &t} }

// MessageResponse wraps a direct message reply as a SendMessageResponse.
func MessageResponse(m Message) SendMessageResponse { return SendMessageResponse{Message: &m} }

// StreamTask wraps a task snapshot as a StreamResponse frame.
func StreamTask(t Task) StreamResponse { return StreamResponse{Task: &t} }

// StreamMessage wraps a direct message as a StreamResponse frame.
func StreamMessage(m Message) StreamResponse { return StreamResponse{Message: &m} }

// StreamStatusUpdate wraps a status update as a StreamResponse frame.
func StreamStatusUpdate(e TaskStatusUpdateEvent) StreamResponse {
	return StreamResponse{StatusUpdate: &e}
}

// StreamArtifactUpdate wraps an artifact update as a StreamResponse frame.
func StreamArtifactUpdate(e TaskArtifactUpdateEvent) StreamResponse {
	return StreamResponse{ArtifactUpdate: &e}
}

// PageSize returns a pointer to a page size, for the optional ListTasksRequest
// field.
func PageSize(n int) *int { return &n }

// HistoryLength returns a pointer to a history length, for the optional
// history-length fields. Zero means "omit history entirely", which is distinct
// from leaving the field unset.
func HistoryLength(n int) *int { return &n }
