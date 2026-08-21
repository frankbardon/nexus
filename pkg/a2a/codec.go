package a2a

import (
	"encoding/json"
	"fmt"
)

// Encode serializes any A2A wire object to JSON. It exists so that call sites
// get uniform "a2a: ..." error wrapping rather than bare encoding/json errors.
func Encode(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode %T: %w", v, err)
	}
	return data, nil
}

// decode is the shared body of the typed decoders below.
func decode[T any](data []byte, what string) (*T, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("a2a: decode %s: %w", what, err)
	}
	return &v, nil
}

// DecodeTask parses a Task.
func DecodeTask(data []byte) (*Task, error) { return decode[Task](data, "task") }

// DecodeMessage parses a Message.
func DecodeMessage(data []byte) (*Message, error) { return decode[Message](data, "message") }

// DecodeArtifact parses an Artifact.
func DecodeArtifact(data []byte) (*Artifact, error) { return decode[Artifact](data, "artifact") }

// DecodeStatusUpdate parses a TaskStatusUpdateEvent.
func DecodeStatusUpdate(data []byte) (*TaskStatusUpdateEvent, error) {
	return decode[TaskStatusUpdateEvent](data, "task status update event")
}

// DecodeArtifactUpdate parses a TaskArtifactUpdateEvent.
func DecodeArtifactUpdate(data []byte) (*TaskArtifactUpdateEvent, error) {
	return decode[TaskArtifactUpdateEvent](data, "task artifact update event")
}

// DecodeSendMessageRequest parses a SendMessageRequest, the REST body of
// POST /message:send and POST /message:stream.
func DecodeSendMessageRequest(data []byte) (*SendMessageRequest, *Error) {
	if !json.Valid(data) {
		return nil, NewError(ErrorTypeJSONParse, "")
	}
	var v SendMessageRequest
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, Errorf(ErrorTypeInvalidParams, "decode SendMessageRequest: %v", err)
	}
	if err := ValidateSendMessageRequest(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

// DecodeCancelTaskRequest parses the REST body of POST /tasks/{id}:cancel. The
// body is optional; an empty body yields a request carrying only the path id.
func DecodeCancelTaskRequest(taskID string, body []byte) (*CancelTaskRequest, *Error) {
	if taskID == "" {
		return nil, ErrInvalidParams(FieldViolation{Field: "id", Description: "task id is required"})
	}
	v := CancelTaskRequest{ID: taskID}
	if len(body) > 0 {
		if !json.Valid(body) {
			return nil, NewError(ErrorTypeJSONParse, "")
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, Errorf(ErrorTypeInvalidParams, "decode CancelTaskRequest: %v", err)
		}
		// The path id is authoritative over anything in the body.
		v.ID = taskID
	}
	return &v, nil
}

// DecodeSubscribeToTaskRequest parses the REST body of
// POST /tasks/{id}:subscribe. The body is optional.
func DecodeSubscribeToTaskRequest(taskID string, body []byte) (*SubscribeToTaskRequest, *Error) {
	if taskID == "" {
		return nil, ErrInvalidParams(FieldViolation{Field: "id", Description: "task id is required"})
	}
	v := SubscribeToTaskRequest{ID: taskID}
	if len(body) > 0 {
		if !json.Valid(body) {
			return nil, NewError(ErrorTypeJSONParse, "")
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, Errorf(ErrorTypeInvalidParams, "decode SubscribeToTaskRequest: %v", err)
		}
		v.ID = taskID
	}
	return &v, nil
}

// DecodeSendMessageResponse parses a SendMessageResponse.
func DecodeSendMessageResponse(data []byte) (*SendMessageResponse, error) {
	return decode[SendMessageResponse](data, "send message response")
}

// DecodeStreamResponse parses one StreamResponse frame.
func DecodeStreamResponse(data []byte) (*StreamResponse, error) {
	return decode[StreamResponse](data, "stream response")
}

// DecodeListTasksResponse parses a ListTasksResponse.
func DecodeListTasksResponse(data []byte) (*ListTasksResponse, error) {
	return decode[ListTasksResponse](data, "list tasks response")
}
