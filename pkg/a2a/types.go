package a2a

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- Timestamps ----

// timestampLayout is the ISO 8601 UTC form with millisecond precision required
// by specification section 5.6.1: YYYY-MM-DDTHH:mm:ss.sssZ.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// Timestamp wraps time.Time so that it marshals to the ISO 8601 UTC form A2A
// requires. Values are converted to UTC on encode; offsets other than "Z" are
// never emitted. Decoding accepts any RFC 3339 timestamp, including ones with a
// non-UTC offset or no fractional seconds, and normalizes to UTC.
type Timestamp struct {
	time.Time
}

// NewTimestamp wraps t as an A2A timestamp, normalized to UTC.
func NewTimestamp(t time.Time) *Timestamp {
	ts := Timestamp{Time: t.UTC()}
	return &ts
}

// MarshalJSON renders the timestamp as an ISO 8601 UTC string.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Time.UTC().Format(timestampLayout))
}

// UnmarshalJSON parses an ISO 8601 / RFC 3339 timestamp into UTC.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("a2a: decode timestamp: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return fmt.Errorf("a2a: parse timestamp %q: %w", raw, err)
	}
	t.Time = parsed.UTC()
	return nil
}

// ---- Enums ----

// TaskState is the lifecycle state of a Task. Values are the ProtoJSON enum
// names from specification section 4.1.
type TaskState string

// Canonical task states. TaskStateUnspecified is the proto zero value and is
// never a legal state for a live task; it stands for "no state known yet".
const (
	TaskStateUnspecified   TaskState = "TASK_STATE_UNSPECIFIED"
	TaskStateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking       TaskState = "TASK_STATE_WORKING"
	TaskStateCompleted     TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed        TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled      TaskState = "TASK_STATE_CANCELED"
	TaskStateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateRejected      TaskState = "TASK_STATE_REJECTED"
	TaskStateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// Role identifies the sender of a Message.
type Role string

// Canonical message roles.
const (
	RoleUnspecified Role = "ROLE_UNSPECIFIED"
	RoleUser        Role = "ROLE_USER"
	RoleAgent       Role = "ROLE_AGENT"
)

// Valid reports whether the role is one of the two addressable roles.
func (r Role) Valid() bool { return r == RoleUser || r == RoleAgent }

// ---- Core objects ----

// Task is the core unit of action in A2A. It carries a current status, the
// artifacts produced so far, and the history of messages exchanged.
type Task struct {
	// ID is the server-generated unique identifier for the task. Required.
	ID string `json:"id"`
	// ContextID groups related tasks and messages into one logical conversation.
	ContextID string `json:"contextId,omitempty"`
	// Status is the current lifecycle status. Required.
	Status TaskStatus `json:"status"`
	// Artifacts are the outputs produced by the task.
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// History is the sequence of messages exchanged during the task. Servers
	// decide which messages are persisted; clients must not rely on all of them
	// being present (specification section 3.7).
	History []Message `json:"history,omitempty"`
	// Metadata is free-form custom data attached to the task.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TaskStatus is a task's state plus the optional message and timestamp that
// accompany it.
type TaskStatus struct {
	// State is the current lifecycle state. Required.
	State TaskState `json:"state"`
	// Message optionally carries an agent message describing the status. This is
	// where an INPUT_REQUIRED question travels.
	Message *Message `json:"message,omitempty"`
	// Timestamp records when the status was observed.
	Timestamp *Timestamp `json:"timestamp,omitempty"`
}

// Message is one unit of communication between client and server.
type Message struct {
	// MessageID is the creator-assigned unique identifier. Required.
	MessageID string `json:"messageId"`
	// ContextID associates the message with a conversational context.
	ContextID string `json:"contextId,omitempty"`
	// TaskID associates the message with a task. Resuming an interrupted task
	// means sending a new message carrying the same taskId and contextId.
	TaskID string `json:"taskId,omitempty"`
	// Role identifies the sender. Required.
	Role Role `json:"role"`
	// Parts is the message content. Required, and must be non-empty.
	Parts []Part `json:"parts"`
	// Metadata is free-form custom data attached to the message.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Extensions lists the URIs of extensions contributing to this message.
	Extensions []string `json:"extensions,omitempty"`
	// ReferenceTaskIDs lists other tasks this message refers to for context.
	ReferenceTaskIDs []string `json:"referenceTaskIds,omitempty"`
}

// Artifact is a set of Parts produced as task output. Outputs travel as
// artifacts, not as messages (specification section 3.7).
type Artifact struct {
	// ArtifactID is unique within the owning task. Required.
	ArtifactID string `json:"artifactId"`
	// Name is a human-readable label.
	Name string `json:"name,omitempty"`
	// Description is a human-readable description.
	Description string `json:"description,omitempty"`
	// Parts is the artifact content. Required, and must be non-empty.
	Parts []Part `json:"parts"`
	// Metadata is free-form custom data attached to the artifact.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Extensions lists the URIs of extensions contributing to this artifact.
	Extensions []string `json:"extensions,omitempty"`
}

// PartKind names which arm of a Part's content oneof is set.
type PartKind string

// Part content kinds.
const (
	PartKindUnset PartKind = ""
	PartKindText  PartKind = "text"
	PartKindRaw   PartKind = "raw"
	PartKindURL   PartKind = "url"
	PartKindData  PartKind = "data"
)

// Part is a section of communication content.
//
// A2A 1.0 flattened the 0.3-era TextPart/FilePart/DataPart hierarchy into this
// one struct. Exactly one of Text, Raw, URL or Data carries the content; the
// remaining fields (MediaType, Filename, Metadata) apply to every kind.
//
// Text and URL are pointers so that a deliberately empty string round-trips
// distinctly from an unset field, matching ProtoJSON oneof presence semantics.
// Use TextPart, RawPart, URLPart and DataPart to construct values.
//
// An explicitly-set but zero-length Raw is the one presence distinction this
// struct cannot preserve: it encodes the same as an unset Raw.
type Part struct {
	// Text is the string content of a text part.
	Text *string `json:"text,omitempty"`
	// Raw is inline binary content, base64-encoded in JSON.
	Raw []byte `json:"raw,omitempty"`
	// URL points at externally-hosted content.
	URL *string `json:"url,omitempty"`
	// Data is arbitrary structured JSON content.
	Data json.RawMessage `json:"data,omitempty"`
	// Metadata is free-form custom data attached to the part.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Filename names the content when it represents a file.
	Filename string `json:"filename,omitempty"`
	// MediaType is the MIME type of the content. Valid for every part kind.
	MediaType string `json:"mediaType,omitempty"`
}

// Kind reports which content arm is set. It returns PartKindUnset when none is,
// and reports only the first set arm when several are (which is itself invalid;
// use ValidatePart to detect that).
func (p Part) Kind() PartKind {
	switch {
	case p.Text != nil:
		return PartKindText
	case p.Raw != nil:
		return PartKindRaw
	case p.URL != nil:
		return PartKindURL
	case p.Data != nil:
		return PartKindData
	default:
		return PartKindUnset
	}
}

// TextValue returns the text content and whether this is a text part.
func (p Part) TextValue() (string, bool) {
	if p.Text == nil {
		return "", false
	}
	return *p.Text, true
}

// URLValue returns the URL content and whether this is a URL part.
func (p Part) URLValue() (string, bool) {
	if p.URL == nil {
		return "", false
	}
	return *p.URL, true
}

// ---- Streaming events ----

// TaskStatusUpdateEvent notifies a client that a task's status changed.
type TaskStatusUpdateEvent struct {
	// TaskID is the task that changed. Required.
	TaskID string `json:"taskId"`
	// ContextID is the context the task belongs to. Required.
	ContextID string `json:"contextId"`
	// Status is the new status. Required.
	Status TaskStatus `json:"status"`
	// Metadata is free-form custom data attached to the update.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TaskArtifactUpdateEvent delivers an artifact, or a chunk of one, for a task.
type TaskArtifactUpdateEvent struct {
	// TaskID is the task that produced the artifact. Required.
	TaskID string `json:"taskId"`
	// ContextID is the context the task belongs to. Required.
	ContextID string `json:"contextId"`
	// Artifact is the generated or updated artifact. Required.
	Artifact Artifact `json:"artifact"`
	// Append reports whether this content extends a previously sent artifact
	// carrying the same artifactId rather than replacing it.
	Append bool `json:"append,omitempty"`
	// LastChunk reports whether this is the final chunk of the artifact.
	LastChunk bool `json:"lastChunk,omitempty"`
	// Metadata is free-form custom data attached to the update.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ---- Push notification data types ----
//
// These exist because SendMessageConfiguration references them. No push
// notification operation is implemented by this package.

// AuthenticationInfo describes the credentials a server uses when POSTing to a
// push notification webhook.
type AuthenticationInfo struct {
	// Scheme is an HTTP authentication scheme name (Bearer, Basic, ...). Required.
	Scheme string `json:"scheme"`
	// Credentials is the scheme-dependent credential material.
	Credentials string `json:"credentials,omitempty"`
}

// TaskPushNotificationConfig is a webhook configuration for a task.
type TaskPushNotificationConfig struct {
	// Tenant is an opaque routing identifier.
	Tenant string `json:"tenant,omitempty"`
	// ID uniquely identifies this configuration.
	ID string `json:"id,omitempty"`
	// TaskID is the task this configuration applies to. Empty when the config is
	// supplied inline on a SendMessage request.
	TaskID string `json:"taskId,omitempty"`
	// URL is the webhook endpoint. Required.
	URL string `json:"url"`
	// Token is a caller-chosen value echoed back with each notification.
	Token string `json:"token,omitempty"`
	// Authentication describes how to authenticate to the webhook.
	Authentication *AuthenticationInfo `json:"authentication,omitempty"`
}

// ---- Operation parameter objects ----

// SendMessageConfiguration tunes a SendMessage or SendStreamingMessage call.
type SendMessageConfiguration struct {
	// AcceptedOutputModes lists media types the client can accept in response
	// parts.
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	// TaskPushNotificationConfig registers a webhook for the resulting task.
	TaskPushNotificationConfig *TaskPushNotificationConfig `json:"taskPushNotificationConfig,omitempty"`
	// HistoryLength caps how many recent history messages the response carries.
	// Unset means no client-imposed limit; zero means omit history entirely.
	HistoryLength *int `json:"historyLength,omitempty"`
	// ReturnImmediately makes the call non-blocking: it returns as soon as the
	// task exists rather than waiting for a terminal or interrupted state.
	// Blocking is the default (specification section 3.2.2).
	ReturnImmediately bool `json:"returnImmediately,omitempty"`
}

// SendMessageRequest is the parameter object for SendMessage and
// SendStreamingMessage.
type SendMessageRequest struct {
	// Tenant is an opaque routing identifier.
	Tenant string `json:"tenant,omitempty"`
	// Message is the message to send. Required.
	Message Message `json:"message"`
	// Configuration tunes the call.
	Configuration *SendMessageConfiguration `json:"configuration,omitempty"`
	// Metadata is free-form custom data attached to the call.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// GetTaskRequest is the parameter object for GetTask.
type GetTaskRequest struct {
	// Tenant is an opaque routing identifier.
	Tenant string `json:"tenant,omitempty"`
	// ID is the task to retrieve. Required.
	ID string `json:"id"`
	// HistoryLength caps how many recent history messages the response carries.
	HistoryLength *int `json:"historyLength,omitempty"`
}

// ListTasksRequest is the parameter object for ListTasks.
type ListTasksRequest struct {
	// Tenant is an opaque routing identifier.
	Tenant string `json:"tenant,omitempty"`
	// ContextID restricts the listing to one conversational context.
	ContextID string `json:"contextId,omitempty"`
	// Status restricts the listing to tasks in one state.
	Status TaskState `json:"status,omitempty"`
	// PageSize caps the page length. Unset means the server default of
	// DefaultListPageSize; the accepted range is 1..MaxListPageSize.
	PageSize *int `json:"pageSize,omitempty"`
	// PageToken is the cursor from a previous response's nextPageToken.
	PageToken string `json:"pageToken,omitempty"`
	// HistoryLength caps how many history messages each listed task carries.
	HistoryLength *int `json:"historyLength,omitempty"`
	// StatusTimestampAfter restricts the listing to tasks whose status changed at
	// or after this instant.
	StatusTimestampAfter *Timestamp `json:"statusTimestampAfter,omitempty"`
	// IncludeArtifacts requests artifacts in the listed tasks. Defaults to false
	// to keep payloads small.
	IncludeArtifacts *bool `json:"includeArtifacts,omitempty"`
}

// Pagination bounds for ListTasks, from specification section 3.2 /
// ListTasksRequest.page_size.
const (
	// DefaultListPageSize is the page size a server applies when the client
	// leaves PageSize unset.
	DefaultListPageSize = 50
	// MinListPageSize is the smallest page size a client may request.
	MinListPageSize = 1
	// MaxListPageSize is the largest page size a client may request.
	MaxListPageSize = 100
)

// ListTasksResponse is the result object for ListTasks.
type ListTasksResponse struct {
	// Tasks are the matching tasks. Required, though it may be empty.
	Tasks []Task `json:"tasks"`
	// NextPageToken is the cursor for the following page, empty when exhausted.
	NextPageToken string `json:"nextPageToken"`
	// PageSize is the page size actually applied.
	PageSize int `json:"pageSize"`
	// TotalSize is the total number of matching tasks before pagination.
	TotalSize int `json:"totalSize"`
}

// CancelTaskRequest is the parameter object for CancelTask.
type CancelTaskRequest struct {
	// Tenant is an opaque routing identifier.
	Tenant string `json:"tenant,omitempty"`
	// ID is the task to cancel. Required.
	ID string `json:"id"`
	// Metadata is free-form custom data attached to the call.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SubscribeToTaskRequest is the parameter object for SubscribeToTask.
type SubscribeToTaskRequest struct {
	// Tenant is an opaque routing identifier.
	Tenant string `json:"tenant,omitempty"`
	// ID is the task to subscribe to. Required.
	ID string `json:"id"`
}

// ---- Response payload objects ----

// SendMessageResponse is the result of SendMessage: either the task tracking the
// work, or a direct message reply for interactions that need no task.
type SendMessageResponse struct {
	// Task is the created or updated task.
	Task *Task `json:"task,omitempty"`
	// Message is a direct reply from the agent.
	Message *Message `json:"message,omitempty"`
}

// StreamPayloadKind names which arm of a StreamResponse payload oneof is set.
type StreamPayloadKind string

// StreamResponse payload kinds.
const (
	StreamPayloadUnset          StreamPayloadKind = ""
	StreamPayloadTask           StreamPayloadKind = "task"
	StreamPayloadMessage        StreamPayloadKind = "message"
	StreamPayloadStatusUpdate   StreamPayloadKind = "statusUpdate"
	StreamPayloadArtifactUpdate StreamPayloadKind = "artifactUpdate"
)

// StreamResponse wraps the several payload shapes a streaming operation emits.
// Exactly one field is set per frame.
type StreamResponse struct {
	// Task is a full task snapshot, typically the opening frame of a stream.
	Task *Task `json:"task,omitempty"`
	// Message is a direct message from the agent.
	Message *Message `json:"message,omitempty"`
	// StatusUpdate reports a task status transition.
	StatusUpdate *TaskStatusUpdateEvent `json:"statusUpdate,omitempty"`
	// ArtifactUpdate delivers an artifact or artifact chunk.
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

// Kind reports which payload arm is set, or StreamPayloadUnset when none is.
func (s StreamResponse) Kind() StreamPayloadKind {
	switch {
	case s.Task != nil:
		return StreamPayloadTask
	case s.Message != nil:
		return StreamPayloadMessage
	case s.StatusUpdate != nil:
		return StreamPayloadStatusUpdate
	case s.ArtifactUpdate != nil:
		return StreamPayloadArtifactUpdate
	default:
		return StreamPayloadUnset
	}
}

// TerminalState returns the task state carried by this frame and whether that
// state is terminal. A streaming operation must close its stream once a frame
// reporting a terminal state has been delivered (specification section 11.7).
func (s StreamResponse) TerminalState() (TaskState, bool) {
	var state TaskState
	switch s.Kind() {
	case StreamPayloadTask:
		state = s.Task.Status.State
	case StreamPayloadStatusUpdate:
		state = s.StatusUpdate.Status.State
	default:
		return TaskStateUnspecified, false
	}
	return state, state.IsTerminal()
}

// ---- Small helpers ----

// String renders the task state without its TASK_STATE_ prefix, for logs and
// error messages. It is not a wire format.
func (s TaskState) String() string {
	return strings.TrimPrefix(string(s), "TASK_STATE_")
}
