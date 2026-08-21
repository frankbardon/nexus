package a2a

import "fmt"

// Validation here enforces the fields the specification marks
// [(google.api.field_behavior) = REQUIRED] (section 5.7), plus the oneof
// exclusivity rules the Go structs cannot express in their types. Unrecognized
// fields are always ignored, per the forward-compatibility rule in section 5.7.

// ValidatePart checks a Part: exactly one content arm must be set.
func ValidatePart(p Part, field string) *Error {
	set := 0
	if p.Text != nil {
		set++
	}
	if p.Raw != nil {
		set++
	}
	if p.URL != nil {
		set++
	}
	if p.Data != nil {
		set++
	}
	switch {
	case set == 0:
		return ErrInvalidParams(FieldViolation{
			Field:       field,
			Description: "part must set exactly one of text, raw, url or data",
		})
	case set > 1:
		return ErrInvalidParams(FieldViolation{
			Field:       field,
			Description: fmt.Sprintf("part sets %d content fields; exactly one of text, raw, url or data is allowed", set),
		})
	}
	return nil
}

// ValidateParts checks a required, non-empty slice of parts.
func ValidateParts(parts []Part, field string) *Error {
	if len(parts) == 0 {
		return ErrInvalidParams(FieldViolation{Field: field, Description: "at least one part is required"})
	}
	for i, p := range parts {
		if err := ValidatePart(p, fmt.Sprintf("%s[%d]", field, i)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateMessage checks a Message's required fields: messageId, a valid role,
// and at least one well-formed part.
func ValidateMessage(m *Message, field string) *Error {
	if m == nil {
		return ErrInvalidParams(FieldViolation{Field: field, Description: "message is required"})
	}
	var violations []FieldViolation
	if m.MessageID == "" {
		violations = append(violations, FieldViolation{Field: field + ".messageId", Description: "message id is required"})
	}
	if !m.Role.Valid() {
		violations = append(violations, FieldViolation{
			Field:       field + ".role",
			Description: fmt.Sprintf("role must be %s or %s, got %q", RoleUser, RoleAgent, string(m.Role)),
		})
	}
	if len(violations) > 0 {
		return ErrInvalidParams(violations...)
	}
	return ValidateParts(m.Parts, field+".parts")
}

// ValidateArtifact checks an Artifact's required fields: artifactId and at least
// one well-formed part.
func ValidateArtifact(a *Artifact, field string) *Error {
	if a == nil {
		return ErrInvalidParams(FieldViolation{Field: field, Description: "artifact is required"})
	}
	if a.ArtifactID == "" {
		return ErrInvalidParams(FieldViolation{Field: field + ".artifactId", Description: "artifact id is required"})
	}
	return ValidateParts(a.Parts, field+".parts")
}

// ValidateTaskStatus checks that a status carries a valid, specified state and
// that any attached message is well-formed.
func ValidateTaskStatus(s TaskStatus, field string) *Error {
	if !s.State.Valid() {
		return ErrInvalidParams(FieldViolation{
			Field:       field + ".state",
			Description: fmt.Sprintf("unknown task state %q", string(s.State)),
		})
	}
	if s.Message != nil {
		return ValidateMessage(s.Message, field+".message")
	}
	return nil
}

// ValidateTask checks a Task's required fields: id, a valid status, and
// well-formed artifacts and history.
func ValidateTask(t *Task) *Error {
	if t == nil {
		return ErrInvalidParams(FieldViolation{Field: "task", Description: "task is required"})
	}
	if t.ID == "" {
		return ErrInvalidParams(FieldViolation{Field: "task.id", Description: "task id is required"})
	}
	if err := ValidateTaskStatus(t.Status, "task.status"); err != nil {
		return err
	}
	for i := range t.Artifacts {
		if err := ValidateArtifact(&t.Artifacts[i], fmt.Sprintf("task.artifacts[%d]", i)); err != nil {
			return err
		}
	}
	for i := range t.History {
		if err := ValidateMessage(&t.History[i], fmt.Sprintf("task.history[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateStatusUpdate checks a TaskStatusUpdateEvent's required fields.
func ValidateStatusUpdate(e *TaskStatusUpdateEvent) *Error {
	if e == nil {
		return ErrInvalidParams(FieldViolation{Field: "statusUpdate", Description: "status update is required"})
	}
	var violations []FieldViolation
	if e.TaskID == "" {
		violations = append(violations, FieldViolation{Field: "statusUpdate.taskId", Description: "task id is required"})
	}
	if e.ContextID == "" {
		violations = append(violations, FieldViolation{Field: "statusUpdate.contextId", Description: "context id is required"})
	}
	if len(violations) > 0 {
		return ErrInvalidParams(violations...)
	}
	return ValidateTaskStatus(e.Status, "statusUpdate.status")
}

// ValidateArtifactUpdate checks a TaskArtifactUpdateEvent's required fields.
func ValidateArtifactUpdate(e *TaskArtifactUpdateEvent) *Error {
	if e == nil {
		return ErrInvalidParams(FieldViolation{Field: "artifactUpdate", Description: "artifact update is required"})
	}
	var violations []FieldViolation
	if e.TaskID == "" {
		violations = append(violations, FieldViolation{Field: "artifactUpdate.taskId", Description: "task id is required"})
	}
	if e.ContextID == "" {
		violations = append(violations, FieldViolation{Field: "artifactUpdate.contextId", Description: "context id is required"})
	}
	if len(violations) > 0 {
		return ErrInvalidParams(violations...)
	}
	return ValidateArtifact(&e.Artifact, "artifactUpdate.artifact")
}

// ValidateSendMessageRequest checks the parameter object for SendMessage and
// SendStreamingMessage.
func ValidateSendMessageRequest(r *SendMessageRequest) *Error {
	if r == nil {
		return ErrInvalidParams(FieldViolation{Field: "params", Description: "SendMessage requires a parameter object"})
	}
	if err := ValidateMessage(&r.Message, "message"); err != nil {
		return err
	}
	if r.Configuration != nil {
		if err := validateHistoryLength(r.Configuration.HistoryLength, "configuration.historyLength"); err != nil {
			return err
		}
		if cfg := r.Configuration.TaskPushNotificationConfig; cfg != nil && cfg.URL == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       "configuration.taskPushNotificationConfig.url",
				Description: "push notification url is required",
			})
		}
	}
	return nil
}

// ValidateListTasksRequest checks the parameter object for ListTasks, including
// the specification's page-size bounds and status filter.
func ValidateListTasksRequest(r *ListTasksRequest) *Error {
	if r == nil {
		return ErrInvalidParams(FieldViolation{Field: "params", Description: "ListTasks requires a parameter object"})
	}
	if r.Status != "" && !r.Status.Valid() {
		return ErrInvalidParams(FieldViolation{
			Field:       "status",
			Description: fmt.Sprintf("unknown task state %q", string(r.Status)),
		})
	}
	if r.PageSize != nil && (*r.PageSize < MinListPageSize || *r.PageSize > MaxListPageSize) {
		return ErrInvalidParams(FieldViolation{
			Field:       "pageSize",
			Description: fmt.Sprintf("must be between %d and %d, got %d", MinListPageSize, MaxListPageSize, *r.PageSize),
		})
	}
	return validateHistoryLength(r.HistoryLength, "historyLength")
}

// validateHistoryLength enforces the non-negative bound on a history length.
// Unset means no client-imposed limit; zero means omit history entirely
// (specification section 3.2.4).
func validateHistoryLength(n *int, field string) *Error {
	if n != nil && *n < 0 {
		return ErrInvalidParams(FieldViolation{
			Field:       field,
			Description: fmt.Sprintf("must not be negative, got %d", *n),
		})
	}
	return nil
}

// ValidateSendMessageResponse checks that exactly one payload arm is set and
// that it is well-formed.
func ValidateSendMessageResponse(r *SendMessageResponse) *Error {
	if r == nil {
		return ErrInvalidParams(FieldViolation{Field: "result", Description: "response is required"})
	}
	set := 0
	if r.Task != nil {
		set++
	}
	if r.Message != nil {
		set++
	}
	if set != 1 {
		return NewError(ErrorTypeInvalidAgentResponse,
			fmt.Sprintf("SendMessageResponse must set exactly one of task or message, got %d", set))
	}
	if r.Task != nil {
		return ValidateTask(r.Task)
	}
	return ValidateMessage(r.Message, "message")
}

// ValidateStreamResponse checks that exactly one payload arm is set and that it
// is well-formed.
func ValidateStreamResponse(s *StreamResponse) *Error {
	if s == nil {
		return NewError(ErrorTypeInvalidAgentResponse, "stream response is required")
	}
	set := 0
	for _, present := range []bool{s.Task != nil, s.Message != nil, s.StatusUpdate != nil, s.ArtifactUpdate != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return NewError(ErrorTypeInvalidAgentResponse,
			fmt.Sprintf("StreamResponse must set exactly one payload field, got %d", set))
	}
	switch s.Kind() {
	case StreamPayloadTask:
		return ValidateTask(s.Task)
	case StreamPayloadMessage:
		return ValidateMessage(s.Message, "message")
	case StreamPayloadStatusUpdate:
		return ValidateStatusUpdate(s.StatusUpdate)
	case StreamPayloadArtifactUpdate:
		return ValidateArtifactUpdate(s.ArtifactUpdate)
	}
	return nil
}
