package a2a

import (
	"testing"
	"time"
)

// TestTypedDecoders covers the object decoders a transport uses to read wire
// payloads. Each case asserts both a well-formed decode and that malformed JSON
// is rejected rather than silently yielding a zero value.
func TestTypedDecoders(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		call  func(data []byte) (any, error)
		check func(t *testing.T, v any)
	}{
		{
			name: "task",
			json: `{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_WORKING"}}`,
			call: func(d []byte) (any, error) { return DecodeTask(d) },
			check: func(t *testing.T, v any) {
				task := v.(*Task)
				if task.ID != "t-1" || task.Status.State != TaskStateWorking {
					t.Fatalf("task = %+v", task)
				}
			},
		},
		{
			name: "message",
			json: `{"messageId":"m-1","role":"ROLE_AGENT","parts":[{"text":"pong"}]}`,
			call: func(d []byte) (any, error) { return DecodeMessage(d) },
			check: func(t *testing.T, v any) {
				m := v.(*Message)
				if m.Role != RoleAgent {
					t.Fatalf("role = %q", m.Role)
				}
			},
		},
		{
			name: "artifact",
			json: `{"artifactId":"a-1","name":"report","parts":[{"text":"body"}]}`,
			call: func(d []byte) (any, error) { return DecodeArtifact(d) },
			check: func(t *testing.T, v any) {
				a := v.(*Artifact)
				if a.ArtifactID != "a-1" || a.Name != "report" {
					t.Fatalf("artifact = %+v", a)
				}
			},
		},
		{
			name: "status update",
			json: `{"taskId":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}`,
			call: func(d []byte) (any, error) { return DecodeStatusUpdate(d) },
			check: func(t *testing.T, v any) {
				e := v.(*TaskStatusUpdateEvent)
				if !e.Status.State.IsTerminal() {
					t.Fatal("expected a terminal state")
				}
			},
		},
		{
			name: "artifact update",
			json: `{"taskId":"t-1","contextId":"c-1","artifact":{"artifactId":"a-1","parts":[{"text":"x"}]},"lastChunk":true}`,
			call: func(d []byte) (any, error) { return DecodeArtifactUpdate(d) },
			check: func(t *testing.T, v any) {
				e := v.(*TaskArtifactUpdateEvent)
				if !e.LastChunk {
					t.Fatal("lastChunk lost")
				}
			},
		},
		{
			name: "send message response",
			json: `{"task":{"id":"t-1","status":{"state":"TASK_STATE_COMPLETED"}}}`,
			call: func(d []byte) (any, error) { return DecodeSendMessageResponse(d) },
			check: func(t *testing.T, v any) {
				r := v.(*SendMessageResponse)
				if r.Task == nil || r.Message != nil {
					t.Fatalf("response = %+v", r)
				}
			},
		},
		{
			name: "stream response",
			json: `{"statusUpdate":{"taskId":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_WORKING"}}}`,
			call: func(d []byte) (any, error) { return DecodeStreamResponse(d) },
			check: func(t *testing.T, v any) {
				s := v.(*StreamResponse)
				if s.Kind() != StreamPayloadStatusUpdate {
					t.Fatalf("kind = %q", s.Kind())
				}
			},
		},
		{
			name: "list tasks response",
			json: `{"tasks":[],"nextPageToken":"","pageSize":50,"totalSize":0}`,
			call: func(d []byte) (any, error) { return DecodeListTasksResponse(d) },
			check: func(t *testing.T, v any) {
				r := v.(*ListTasksResponse)
				if r.PageSize != 50 || len(r.Tasks) != 0 {
					t.Fatalf("response = %+v", r)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.call([]byte(tc.json))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			tc.check(t, got)

			if _, err := tc.call([]byte(`"not an object"`)); err == nil {
				t.Fatal("expected an error decoding a non-object")
			}
		})
	}
}

// TestEncodeWrapsErrors checks Encode reports unmarshalable values rather than
// panicking or returning an empty document.
func TestEncodeWrapsErrors(t *testing.T) {
	if _, err := Encode(make(chan int)); err == nil {
		t.Fatal("expected an error encoding a channel")
	}
	data, err := Encode(TextPart("hi"))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(data) != `{"text":"hi"}` {
		t.Fatalf("= %s", data)
	}
}

// TestNewTaskStatusAt covers the deterministic status constructor used by tests
// and replayed histories.
func TestNewTaskStatusAt(t *testing.T) {
	at := time.Date(2025, 10, 28, 10, 30, 0, 0, time.UTC)
	status := NewTaskStatusAt(TaskStateWorking, at)
	if status.State != TaskStateWorking {
		t.Fatalf("state = %q", status.State)
	}
	if status.Timestamp == nil || !status.Timestamp.Equal(at) {
		t.Fatalf("timestamp = %v, want %v", status.Timestamp, at)
	}
}

// TestTaskPath covers the task-scoped path builder.
func TestTaskPath(t *testing.T) {
	got, err := TaskPath(PathSubscribeTask, "task-uuid")
	if err != nil {
		t.Fatalf("TaskPath: %v", err)
	}
	if got != "/tasks/task-uuid:subscribe" {
		t.Fatalf("= %q", got)
	}
	if _, err := TaskPath(PathListTasks, "task-uuid"); err == nil {
		t.Fatal("expected an error for a template with no id placeholder")
	}
}

// TestErrUnsupportedOperation covers the convenience constructor a server uses
// when a client asks for a capability the agent card does not advertise.
func TestErrUnsupportedOperation(t *testing.T) {
	err := ErrUnsupportedOperation(MethodSubscribeToTask)
	if err.Type != ErrorTypeUnsupportedOperation {
		t.Fatalf("type = %q", err.Type)
	}
	if err.Code() != CodeUnsupportedOperation {
		t.Fatalf("code = %d", err.Code())
	}
}

// TestRouteForUnknownOperation covers the negative lookup path.
func TestRouteForUnknownOperation(t *testing.T) {
	if _, ok := RouteFor("Teleport"); ok {
		t.Fatal("expected no route for an unknown operation")
	}
}

// TestPartAccessorsOnWrongKind checks the typed accessors report absence rather
// than returning a misleading zero value.
func TestPartAccessorsOnWrongKind(t *testing.T) {
	p := URLPart("https://example.com", "text/html")
	if text, ok := p.TextValue(); ok || text != "" {
		t.Fatalf("TextValue on a URL part = (%q, %v)", text, ok)
	}
	if u, ok := p.URLValue(); !ok || u != "https://example.com" {
		t.Fatalf("URLValue = (%q, %v)", u, ok)
	}
	empty := Part{}
	if _, ok := empty.URLValue(); ok {
		t.Fatal("URLValue on an empty part reported present")
	}
	if empty.Kind() != PartKindUnset {
		t.Fatalf("Kind = %q", empty.Kind())
	}
}

// TestNewErrorUnmappedType covers the defensive branch for a type absent from
// the mapping table, which is a programming error rather than a wire condition.
func TestNewErrorUnmappedType(t *testing.T) {
	err := NewError(ErrorType("NotARealError"), "something happened")
	if err.Type != ErrorTypeInternal {
		t.Fatalf("type = %q, want %q", err.Type, ErrorTypeInternal)
	}
	if err.Code() != CodeInternal {
		t.Fatalf("code = %d", err.Code())
	}
}

// TestNewRequestRejectsUnmarshalableValues covers the encode error paths.
func TestNewRequestRejectsUnmarshalableValues(t *testing.T) {
	if _, err := NewRequest(make(chan int), MethodGetTask, nil); err == nil {
		t.Fatal("expected an error for an unmarshalable id")
	}
	if _, err := NewRequest("id", MethodGetTask, make(chan int)); err == nil {
		t.Fatal("expected an error for unmarshalable params")
	}
	if _, err := NewResultResponse(nil, make(chan int)); err == nil {
		t.Fatal("expected an error for an unmarshalable result")
	}
}

// TestNewErrorResponseNilError never produces a response with neither arm set.
func TestNewErrorResponseNilError(t *testing.T) {
	resp := NewErrorResponse(nil, nil)
	if resp.Error == nil {
		t.Fatal("expected a synthesized internal error")
	}
	if resp.Error.Code != CodeInternal {
		t.Fatalf("code = %d, want %d", resp.Error.Code, CodeInternal)
	}
}

// TestCallIDOnEmptyCall covers the defensive default in Call.ID.
func TestCallIDOnEmptyCall(t *testing.T) {
	if got := string((&Call{}).ID()); got != "null" {
		t.Fatalf("= %s, want null", got)
	}
}
