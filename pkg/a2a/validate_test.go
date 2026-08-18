package a2a

import "testing"

// TestValidatePart covers the flattened Part's oneof exclusivity rule.
func TestValidatePart(t *testing.T) {
	empty := ""
	url := "https://example.com/a.pdf"

	tests := []struct {
		name    string
		part    Part
		wantErr bool
	}{
		{name: "text", part: TextPart("hi")},
		{name: "empty text is still a set arm", part: Part{Text: &empty}},
		{name: "url", part: URLPart(url, "application/pdf")},
		{name: "raw", part: RawPart([]byte("x"), "text/plain", "x.txt")},
		{name: "data", part: Part{Data: []byte(`{"a":1}`)}},
		{name: "no content arm", part: Part{MediaType: "text/plain"}, wantErr: true},
		{name: "text and url", part: Part{Text: &empty, URL: &url}, wantErr: true},
		{name: "raw and data", part: Part{Raw: []byte("x"), Data: []byte(`1`)}, wantErr: true},
		{name: "all four arms", part: Part{Text: &empty, URL: &url, Raw: []byte("x"), Data: []byte(`1`)}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePart(tc.part, "part")
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidatePart = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && err.Type != ErrorTypeInvalidParams {
				t.Fatalf("error type = %q, want %q", err.Type, ErrorTypeInvalidParams)
			}
		})
	}
}

// TestValidateMessage covers Message's required fields.
func TestValidateMessage(t *testing.T) {
	tests := []struct {
		name    string
		message *Message
		wantErr bool
	}{
		{name: "valid user message", message: ptr(NewUserMessage("m-1", "hi"))},
		{name: "valid agent message", message: ptr(NewAgentMessage("m-1", "hi"))},
		{name: "nil", message: nil, wantErr: true},
		{name: "missing message id", message: &Message{Role: RoleUser, Parts: []Part{TextPart("hi")}}, wantErr: true},
		{name: "missing role", message: &Message{MessageID: "m-1", Parts: []Part{TextPart("hi")}}, wantErr: true},
		{
			name:    "0.3-era lowercase role",
			message: &Message{MessageID: "m-1", Role: Role("user"), Parts: []Part{TextPart("hi")}},
			wantErr: true,
		},
		{name: "nil parts", message: &Message{MessageID: "m-1", Role: RoleUser}, wantErr: true},
		{name: "empty parts", message: &Message{MessageID: "m-1", Role: RoleUser, Parts: []Part{}}, wantErr: true},
		{
			name:    "invalid part",
			message: &Message{MessageID: "m-1", Role: RoleUser, Parts: []Part{{MediaType: "text/plain"}}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMessage(tc.message, "message")
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateMessage = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateArtifact covers Artifact's required fields.
func TestValidateArtifact(t *testing.T) {
	tests := []struct {
		name     string
		artifact *Artifact
		wantErr  bool
	}{
		{name: "valid", artifact: ptr(NewTextArtifact("a-1", "report", "body"))},
		{name: "nil", artifact: nil, wantErr: true},
		{name: "missing artifact id", artifact: &Artifact{Parts: []Part{TextPart("x")}}, wantErr: true},
		{name: "no parts", artifact: &Artifact{ArtifactID: "a-1"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateArtifact(tc.artifact, "artifact")
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateArtifact = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateTask covers Task's required fields, including nested artifacts and
// history.
func TestValidateTask(t *testing.T) {
	tests := []struct {
		name    string
		task    *Task
		wantErr bool
	}{
		{name: "minimal valid", task: &Task{ID: "t-1", Status: TaskStatus{State: TaskStateWorking}}},
		{
			name: "with artifacts and history",
			task: &Task{
				ID:        "t-1",
				Status:    TaskStatus{State: TaskStateCompleted},
				Artifacts: []Artifact{NewTextArtifact("a-1", "report", "body")},
				History:   []Message{NewUserMessage("m-1", "hi")},
			},
		},
		{name: "nil", task: nil, wantErr: true},
		{name: "missing id", task: &Task{Status: TaskStatus{State: TaskStateWorking}}, wantErr: true},
		{name: "missing status", task: &Task{ID: "t-1"}, wantErr: true},
		{name: "unspecified status", task: &Task{ID: "t-1", Status: TaskStatus{State: TaskStateUnspecified}}, wantErr: true},
		{name: "unknown status", task: &Task{ID: "t-1", Status: TaskStatus{State: TaskState("nope")}}, wantErr: true},
		{
			name: "invalid artifact",
			task: &Task{
				ID:        "t-1",
				Status:    TaskStatus{State: TaskStateWorking},
				Artifacts: []Artifact{{ArtifactID: "a-1"}},
			},
			wantErr: true,
		},
		{
			name: "invalid history message",
			task: &Task{
				ID:      "t-1",
				Status:  TaskStatus{State: TaskStateWorking},
				History: []Message{{MessageID: "m-1", Role: RoleUser}},
			},
			wantErr: true,
		},
		{
			name: "invalid status message",
			task: &Task{
				ID:     "t-1",
				Status: TaskStatus{State: TaskStateInputRequired, Message: &Message{Role: RoleAgent}},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTask(tc.task)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateTask = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateUpdateEvents covers the two streaming event types, both of which
// require taskId and contextId.
func TestValidateUpdateEvents(t *testing.T) {
	t.Run("status update", func(t *testing.T) {
		tests := []struct {
			name    string
			event   *TaskStatusUpdateEvent
			wantErr bool
		}{
			{name: "valid", event: ptr(NewStatusUpdate("t-1", "c-1", TaskStatus{State: TaskStateWorking}))},
			{name: "nil", event: nil, wantErr: true},
			{name: "missing task id", event: ptr(NewStatusUpdate("", "c-1", TaskStatus{State: TaskStateWorking})), wantErr: true},
			{name: "missing context id", event: ptr(NewStatusUpdate("t-1", "", TaskStatus{State: TaskStateWorking})), wantErr: true},
			{name: "unspecified state", event: ptr(NewStatusUpdate("t-1", "c-1", TaskStatus{})), wantErr: true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidateStatusUpdate(tc.event)
				if tc.wantErr != (err != nil) {
					t.Fatalf("ValidateStatusUpdate = %v, wantErr %v", err, tc.wantErr)
				}
			})
		}
	})

	t.Run("artifact update", func(t *testing.T) {
		artifact := NewTextArtifact("a-1", "report", "body")
		tests := []struct {
			name    string
			event   *TaskArtifactUpdateEvent
			wantErr bool
		}{
			{name: "valid", event: ptr(NewArtifactUpdate("t-1", "c-1", artifact))},
			{name: "chunked", event: ptr(NewArtifactChunk("t-1", "c-1", artifact, true, false))},
			{name: "nil", event: nil, wantErr: true},
			{name: "missing task id", event: ptr(NewArtifactUpdate("", "c-1", artifact)), wantErr: true},
			{name: "missing context id", event: ptr(NewArtifactUpdate("t-1", "", artifact)), wantErr: true},
			{name: "invalid artifact", event: ptr(NewArtifactUpdate("t-1", "c-1", Artifact{ArtifactID: "a-1"})), wantErr: true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidateArtifactUpdate(tc.event)
				if tc.wantErr != (err != nil) {
					t.Fatalf("ValidateArtifactUpdate = %v, wantErr %v", err, tc.wantErr)
				}
			})
		}
	})
}

// TestValidateSendMessageRequest covers the configuration bounds.
func TestValidateSendMessageRequest(t *testing.T) {
	valid := NewUserMessage("m-1", "hi")
	tests := []struct {
		name    string
		request *SendMessageRequest
		wantErr bool
	}{
		{name: "minimal", request: &SendMessageRequest{Message: valid}},
		{
			name: "with configuration",
			request: &SendMessageRequest{Message: valid, Configuration: &SendMessageConfiguration{
				AcceptedOutputModes: []string{"text/plain"},
				HistoryLength:       HistoryLength(0),
			}},
		},
		{name: "nil", request: nil, wantErr: true},
		{name: "no message", request: &SendMessageRequest{}, wantErr: true},
		{
			name: "negative history length",
			request: &SendMessageRequest{Message: valid, Configuration: &SendMessageConfiguration{
				HistoryLength: HistoryLength(-1),
			}},
			wantErr: true,
		},
		{
			name: "push config without a url",
			request: &SendMessageRequest{Message: valid, Configuration: &SendMessageConfiguration{
				TaskPushNotificationConfig: &TaskPushNotificationConfig{Token: "t"},
			}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSendMessageRequest(tc.request)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateSendMessageRequest = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateListTasksRequest covers the pagination and filter bounds.
func TestValidateListTasksRequest(t *testing.T) {
	tests := []struct {
		name    string
		request *ListTasksRequest
		wantErr bool
	}{
		{name: "empty", request: &ListTasksRequest{}},
		{name: "minimum page size", request: &ListTasksRequest{PageSize: PageSize(MinListPageSize)}},
		{name: "maximum page size", request: &ListTasksRequest{PageSize: PageSize(MaxListPageSize)}},
		{name: "valid status filter", request: &ListTasksRequest{Status: TaskStateWorking}},
		{name: "nil", request: nil, wantErr: true},
		{name: "page size zero", request: &ListTasksRequest{PageSize: PageSize(0)}, wantErr: true},
		{name: "page size too large", request: &ListTasksRequest{PageSize: PageSize(MaxListPageSize + 1)}, wantErr: true},
		{name: "unknown status filter", request: &ListTasksRequest{Status: TaskState("nope")}, wantErr: true},
		{name: "unspecified status filter", request: &ListTasksRequest{Status: TaskStateUnspecified}, wantErr: true},
		{name: "negative history length", request: &ListTasksRequest{HistoryLength: HistoryLength(-1)}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateListTasksRequest(tc.request)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateListTasksRequest = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateResponsePayloads covers the two response oneofs, which must set
// exactly one arm.
func TestValidateResponsePayloads(t *testing.T) {
	task := Task{ID: "t-1", Status: TaskStatus{State: TaskStateCompleted}}
	message := NewAgentMessage("m-1", "pong")

	t.Run("send message response", func(t *testing.T) {
		tests := []struct {
			name     string
			response *SendMessageResponse
			wantErr  bool
		}{
			{name: "task arm", response: ptr(TaskResponse(task))},
			{name: "message arm", response: ptr(MessageResponse(message))},
			{name: "nil", response: nil, wantErr: true},
			{name: "no arm", response: &SendMessageResponse{}, wantErr: true},
			{name: "both arms", response: &SendMessageResponse{Task: &task, Message: &message}, wantErr: true},
			{name: "invalid task", response: &SendMessageResponse{Task: &Task{}}, wantErr: true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidateSendMessageResponse(tc.response)
				if tc.wantErr != (err != nil) {
					t.Fatalf("ValidateSendMessageResponse = %v, wantErr %v", err, tc.wantErr)
				}
			})
		}
	})

	t.Run("stream response", func(t *testing.T) {
		statusUpdate := NewStatusUpdate("t-1", "c-1", TaskStatus{State: TaskStateWorking})
		artifactUpdate := NewArtifactUpdate("t-1", "c-1", NewTextArtifact("a-1", "r", "b"))

		tests := []struct {
			name     string
			response *StreamResponse
			wantErr  bool
		}{
			{name: "task arm", response: ptr(StreamTask(task))},
			{name: "message arm", response: ptr(StreamMessage(message))},
			{name: "status update arm", response: ptr(StreamStatusUpdate(statusUpdate))},
			{name: "artifact update arm", response: ptr(StreamArtifactUpdate(artifactUpdate))},
			{name: "nil", response: nil, wantErr: true},
			{name: "no arm", response: &StreamResponse{}, wantErr: true},
			{name: "two arms", response: &StreamResponse{Task: &task, StatusUpdate: &statusUpdate}, wantErr: true},
			{name: "invalid status update", response: &StreamResponse{StatusUpdate: &TaskStatusUpdateEvent{}}, wantErr: true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidateStreamResponse(tc.response)
				if tc.wantErr != (err != nil) {
					t.Fatalf("ValidateStreamResponse = %v, wantErr %v", err, tc.wantErr)
				}
			})
		}
	})
}

// TestValidationErrorsCarryFieldViolations checks that validation failures name
// the offending field, as specification section 3.3.2 requires.
func TestValidationErrorsCarryFieldViolations(t *testing.T) {
	err := ValidateMessage(&Message{Role: RoleUser, Parts: []Part{TextPart("hi")}}, "message")
	if err == nil {
		t.Fatal("expected an error")
	}
	rpc := err.RPCError()
	if len(rpc.Data) != 1 {
		t.Fatalf("data = %v, want one BadRequest detail", rpc.Data)
	}
	detail := rpc.Data[0]
	if detail["@type"] != TypeBadRequest {
		t.Fatalf("@type = %v, want %q", detail["@type"], TypeBadRequest)
	}
	violations, ok := detail["fieldViolations"].([]FieldViolation)
	if !ok || len(violations) == 0 {
		t.Fatalf("fieldViolations = %v", detail["fieldViolations"])
	}
	if violations[0].Field != "message.messageId" {
		t.Fatalf("field = %q, want message.messageId", violations[0].Field)
	}
}

// TestConstructorsProduceValidObjects checks the constructors never hand back
// something the validators reject.
func TestConstructorsProduceValidObjects(t *testing.T) {
	if err := ValidateMessage(ptr(NewUserMessage("m-1", "hi")), "message"); err != nil {
		t.Errorf("NewUserMessage: %v", err)
	}
	if err := ValidateMessage(ptr(NewAgentMessage("m-1", "hi")), "message"); err != nil {
		t.Errorf("NewAgentMessage: %v", err)
	}
	if err := ValidateMessage(ptr(NewMessage("m-1", RoleUser, TextPart("a"), TextPart("b"))), "message"); err != nil {
		t.Errorf("NewMessage: %v", err)
	}
	if err := ValidateArtifact(ptr(NewTextArtifact("a-1", "n", "b")), "artifact"); err != nil {
		t.Errorf("NewTextArtifact: %v", err)
	}
	if err := ValidateArtifact(ptr(NewArtifact("a-1", TextPart("x"))), "artifact"); err != nil {
		t.Errorf("NewArtifact: %v", err)
	}
	if err := ValidateTask(ptr(NewTask("t-1", "c-1"))); err != nil {
		t.Errorf("NewTask: %v", err)
	}

	task := NewTask("t-1", "c-1")
	if task.Status.State != TaskStateSubmitted {
		t.Errorf("NewTask state = %q, want submitted", task.Status.State)
	}
	if task.Status.Timestamp == nil {
		t.Error("NewTask must stamp a timestamp")
	}
}

// TestMessageChaining covers the resume helpers: a client resumes an interrupted
// task by sending a new message carrying the same taskId and contextId.
func TestMessageChaining(t *testing.T) {
	m := NewUserMessage("m-2", "From San Francisco to New York").
		InContext("context-uuid").
		ForTask("task-uuid")
	if m.ContextID != "context-uuid" || m.TaskID != "task-uuid" {
		t.Fatalf("message = %+v", m)
	}
	if err := ValidateMessage(&m, "message"); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestTaskStatusWithMessage covers attaching an INPUT_REQUIRED question.
func TestTaskStatusWithMessage(t *testing.T) {
	status := NewTaskStatus(TaskStateInputRequired).
		WithMessage(NewAgentMessage("q-1", "Where would you like to fly from?"))
	if status.Message == nil {
		t.Fatal("status message missing")
	}
	if err := ValidateTaskStatus(status, "status"); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// ptr is a test helper returning a pointer to its argument.
func ptr[T any](v T) *T { return &v }
