package a2aclient

import (
	"context"
	"net/http"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// The non-streaming operations. Each is available over both bindings and the
// binding is chosen once, at construction, so an operation body never branches
// on transport beyond assembling the request.

// SendMessage sends a message and waits for the single response the remote
// answers with: the task tracking the work, or a direct message reply for an
// interaction that needed no task.
//
// The call BLOCKS for the duration of the remote's work unless the request sets
// Configuration.ReturnImmediately (specification section 3.2.2). Its deadline
// is therefore the caller's context plus WithMessageTimeout, which defaults to
// no client-imposed bound; WithRequestTimeout deliberately does not apply.
//
// A message carrying a TaskID and ContextID resumes an existing task; see
// ResumeRequest.
func (c *Client) SendMessage(ctx context.Context, req a2a.SendMessageRequest) (*a2a.SendMessageResponse, error) {
	if err := a2a.ValidateSendMessageRequest(&req); err != nil {
		return nil, err
	}
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := withTimeout(ctx, c.messageTimeout)
	defer cancel()

	var out a2a.SendMessageResponse
	if c.binding == a2a.BindingHTTPJSON {
		body, encErr := a2a.Encode(req)
		if encErr != nil {
			return nil, encErr
		}
		if _, err := c.restJSON(callCtx, httpCall{
			operation: a2a.MethodSendMessage,
			method:    http.MethodPost,
			url:       endpoint + a2a.PathSendMessage,
			body:      body,
		}, &out); err != nil {
			return nil, err
		}
	} else if _, err := c.rpc(callCtx, endpoint, a2a.MethodSendMessage, req, &out, false); err != nil {
		return nil, err
	}

	if err := validateSendMessageResponse(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTask reads a task back from the remote. It is idempotent and is retried
// per the client's RetryPolicy.
func (c *Client) GetTask(ctx context.Context, req a2a.GetTaskRequest) (*a2a.Task, error) {
	if strings.TrimSpace(req.ID) == "" {
		return nil, a2a.ErrInvalidParams(a2a.FieldViolation{Field: "id", Description: "task id is required"})
	}
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := withTimeout(ctx, c.requestTimeout)
	defer cancel()

	var task a2a.Task
	if c.binding == a2a.BindingHTTPJSON {
		path, perr := a2a.TaskPath(a2a.PathGetTask, req.ID)
		if perr != nil {
			return nil, perr
		}
		callURL := endpoint + path
		if q := req.Query().Encode(); q != "" {
			callURL += "?" + q
		}
		if _, err := c.restJSON(callCtx, httpCall{
			operation:  a2a.MethodGetTask,
			method:     http.MethodGet,
			url:        callURL,
			idempotent: true,
		}, &task); err != nil {
			return nil, err
		}
	} else if _, err := c.rpc(callCtx, endpoint, a2a.MethodGetTask, req, &task, true); err != nil {
		return nil, err
	}

	if err := validateTask(&task, a2a.MethodGetTask); err != nil {
		return nil, err
	}
	return &task, nil
}

// CancelTask asks the remote to cancel a task and returns the task as it stands
// afterwards. A task already in a terminal state answers with
// TaskNotCancelableError, which arrives as an *a2a.Error.
//
// It is NOT retried on a transport failure: a cancel that was delivered and
// whose response was lost has already happened, and re-sending it would race a
// task that has since moved on. A 429 or 503 is retried, since those say the
// request was not processed.
func (c *Client) CancelTask(ctx context.Context, req a2a.CancelTaskRequest) (*a2a.Task, error) {
	if strings.TrimSpace(req.ID) == "" {
		return nil, a2a.ErrInvalidParams(a2a.FieldViolation{Field: "id", Description: "task id is required"})
	}
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := withTimeout(ctx, c.requestTimeout)
	defer cancel()

	var task a2a.Task
	if c.binding == a2a.BindingHTTPJSON {
		path, perr := a2a.TaskPath(a2a.PathCancelTask, req.ID)
		if perr != nil {
			return nil, perr
		}
		body, encErr := a2a.Encode(req)
		if encErr != nil {
			return nil, encErr
		}
		if _, err := c.restJSON(callCtx, httpCall{
			operation: a2a.MethodCancelTask,
			method:    http.MethodPost,
			url:       endpoint + path,
			body:      body,
		}, &task); err != nil {
			return nil, err
		}
	} else if _, err := c.rpc(callCtx, endpoint, a2a.MethodCancelTask, req, &task, false); err != nil {
		return nil, err
	}

	if err := validateTask(&task, a2a.MethodCancelTask); err != nil {
		return nil, err
	}
	return &task, nil
}

// validateSendMessageResponse rejects a response that sets neither arm or both.
// The oneof is the whole content of the answer; a remote that gets it wrong has
// told the client nothing, and reporting that as InvalidAgentResponseError is
// better than handing back a zero value that reads as an empty reply.
func validateSendMessageResponse(resp *a2a.SendMessageResponse) error {
	switch {
	case resp.Task == nil && resp.Message == nil:
		return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
			"%s: response carries neither a task nor a message", a2a.MethodSendMessage)
	case resp.Task != nil && resp.Message != nil:
		return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
			"%s: response carries both a task and a message", a2a.MethodSendMessage)
	case resp.Task != nil:
		return validateTask(resp.Task, a2a.MethodSendMessage)
	default:
		if verr := a2a.ValidateMessage(resp.Message, "message"); verr != nil {
			return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
				"%s: %s", a2a.MethodSendMessage, verr.Message)
		}
		return nil
	}
}

// validateTask checks the minimum a returned task must satisfy to be usable: an
// id and a known, specified state. Anything less and the client cannot track
// the task or decide whether it is still live.
func validateTask(task *a2a.Task, operation string) error {
	if task == nil {
		return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse, "%s: response carries no task", operation)
	}
	if strings.TrimSpace(task.ID) == "" {
		return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse, "%s: task carries no id", operation)
	}
	if !task.Status.State.Valid() {
		return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
			"%s: task %s reports unknown state %q", operation, task.ID, string(task.Status.State))
	}
	return nil
}
