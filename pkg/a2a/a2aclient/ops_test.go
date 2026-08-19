package a2aclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// bindings drives every operation test over both HTTP bindings, since the whole
// point of the binding option is that the operations behave identically either
// way.
var bindings = []struct {
	name    string
	binding a2a.ProtocolBinding
}{
	{"jsonrpc", a2a.BindingJSONRPC},
	{"rest", a2a.BindingHTTPJSON},
}

func TestSendMessageBothBindings(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent(t, agentConfig{
				sendMessage: func(req *a2a.SendMessageRequest) (a2a.SendMessageResponse, *a2a.Error) {
					text, _ := req.Message.Parts[0].TextValue()
					return a2a.MessageResponse(a2a.NewAgentMessage("m-reply", "echo: "+text)), nil
				},
			})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			resp, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
				Message: a2a.NewUserMessage("m1", "hello"),
			})
			if err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			if resp.Message == nil {
				t.Fatal("want a direct message reply")
			}
			text, _ := resp.Message.Parts[0].TextValue()
			if text != "echo: hello" {
				t.Fatalf("reply = %q", text)
			}
		})
	}
}

func TestSendMessageReturnsTask(t *testing.T) {
	agent := newAgent(t, agentConfig{
		sendMessage: func(*a2a.SendMessageRequest) (a2a.SendMessageResponse, *a2a.Error) {
			task := a2a.NewTask("task-9", "ctx-9")
			task.Status = a2a.NewTaskStatus(a2a.TaskStateCompleted)
			return a2a.TaskResponse(task), nil
		},
	})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "work"),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.Task == nil || resp.Task.ID != "task-9" {
		t.Fatalf("task = %+v", resp.Task)
	}
}

func TestSendMessageValidatesLocally(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// No parts: the codec rejects it before a byte is sent.
	_, err = client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.Message{MessageID: "m1", Role: a2a.RoleUser},
	})
	var protoErr *a2a.Error
	if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeInvalidParams {
		t.Fatalf("error = %v, want InvalidParamsError", err)
	}
	if len(agent.seen()) != 0 {
		t.Fatal("an invalid request reached the wire")
	}
}

func TestGetTaskBothBindings(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			var gotHistoryLength *int
			agent := newAgent(t, agentConfig{
				getTask: func(req *a2a.GetTaskRequest) (a2a.Task, *a2a.Error) {
					gotHistoryLength = req.HistoryLength
					task := a2a.NewTask(req.ID, "ctx-1")
					task.Status = a2a.NewTaskStatus(a2a.TaskStateWorking)
					return task, nil
				},
			})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			task, err := client.GetTask(context.Background(), a2a.GetTaskRequest{
				ID: "task-7", HistoryLength: a2a.HistoryLength(3),
			})
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.ID != "task-7" || task.Status.State != a2a.TaskStateWorking {
				t.Fatalf("task = %+v", task)
			}
			// The parameter survives both the JSON body and the query string.
			if gotHistoryLength == nil || *gotHistoryLength != 3 {
				t.Fatalf("historyLength = %v, want 3", gotHistoryLength)
			}
		})
	}
}

func TestCancelTaskBothBindings(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent(t, agentConfig{
				cancelTask: func(req *a2a.CancelTaskRequest) (a2a.Task, *a2a.Error) {
					task := a2a.NewTask(req.ID, "ctx-1")
					task.Status = a2a.NewTaskStatus(a2a.TaskStateCanceled)
					return task, nil
				},
			})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			task, err := client.CancelTask(context.Background(), a2a.CancelTaskRequest{ID: "task-3"})
			if err != nil {
				t.Fatalf("CancelTask: %v", err)
			}
			if task.Status.State != a2a.TaskStateCanceled {
				t.Fatalf("state = %s, want CANCELED", task.Status.State)
			}
		})
	}
}

func TestCancelTaskProtocolErrorBothBindings(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent(t, agentConfig{
				cancelTask: func(req *a2a.CancelTaskRequest) (a2a.Task, *a2a.Error) {
					return a2a.Task{}, a2a.ErrTaskNotCancelable(req.ID, a2a.TaskStateCompleted)
				},
			})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = client.CancelTask(context.Background(), a2a.CancelTaskRequest{ID: "task-3"})
			var protoErr *a2a.Error
			if !errors.As(err, &protoErr) {
				t.Fatalf("error = %v (%T), want *a2a.Error", err, err)
			}
			// The taxonomy survives both framings, which is the whole point of
			// routing errors back through the codec rather than the status code.
			if protoErr.Type != a2a.ErrorTypeTaskNotCancelable {
				t.Fatalf("type = %s, want TaskNotCancelableError", protoErr.Type)
			}
		})
	}
}

func TestGetTaskNotFoundBothBindings(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent(t, agentConfig{
				getTask: func(req *a2a.GetTaskRequest) (a2a.Task, *a2a.Error) {
					return a2a.Task{}, a2a.ErrTaskNotFound(req.ID)
				},
			})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "nope"})
			var protoErr *a2a.Error
			if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeTaskNotFound {
				t.Fatalf("error = %v, want TaskNotFoundError", err)
			}
		})
	}
}

func TestOperationsRejectEmptyTaskID(t *testing.T) {
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint("http://example.invalid/a2a"))
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{}); err == nil {
		t.Fatal("GetTask with no id should fail locally")
	}
	if _, err := client.CancelTask(context.Background(), a2a.CancelTaskRequest{}); err == nil {
		t.Fatal("CancelTask with no id should fail locally")
	}
	if _, err := client.SubscribeToTask(context.Background(), a2a.SubscribeToTaskRequest{}); err == nil {
		t.Fatal("SubscribeToTask with no id should fail locally")
	}
}

func TestServiceParametersAreSent(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	client, err := a2aclient.New(agent.URL(),
		a2aclient.WithExtensions("https://example.invalid/ext/one"),
		a2aclient.WithUserAgent("test-ua/9"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	seen := agent.seen()
	last := seen[len(seen)-1]
	// Section 3.6.1: omitting A2A-Version means 0.3 to a conforming agent, and
	// the test agent refuses that, so this assertion has teeth.
	if got := last.Header.Get(a2a.HeaderVersion); got != a2a.ProtocolVersion {
		t.Fatalf("%s = %q, want %q", a2a.HeaderVersion, got, a2a.ProtocolVersion)
	}
	if got := last.Header.Get(a2a.HeaderExtensions); got != "https://example.invalid/ext/one" {
		t.Fatalf("%s = %q", a2a.HeaderExtensions, got)
	}
	if got := last.Header.Get("User-Agent"); got != "test-ua/9" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := last.Header.Get("Content-Type"); got != "" && got != a2a.ContentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", got, a2a.ContentTypeJSON)
	}
}

func TestVersionHeaderRefusalSurfacesAsProtocolError(t *testing.T) {
	// A server that refuses the negotiated version answers with a protocol
	// error, and it must reach the caller as one.
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		protoErr := a2a.Errorf(a2a.ErrorTypeVersionNotSupported, "only 0.3 here")
		body, _ := a2a.NewErrorResponse([]byte(`"1"`), protoErr).Encode()
		w.Header().Set("Content-Type", a2a.ContentTypeJSON)
		_, _ = w.Write(body)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(srv.URL))

	_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	var protoErr *a2a.Error
	if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeVersionNotSupported {
		t.Fatalf("error = %v, want VersionNotSupportedError", err)
	}
}

func TestNonA2AHTTPErrorIsTyped(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway is sad</html>"))
	})
	client := mustClient(t,
		a2aclient.WithJSONRPCEndpoint(srv.URL),
		a2aclient.WithRetryPolicy(a2aclient.NoRetry()),
	)

	_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	var httpErr *a2aclient.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v (%T), want *HTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", httpErr.StatusCode)
	}
	if !strings.Contains(httpErr.Body, "gateway is sad") {
		t.Fatalf("body snippet = %q, want the intermediary's page", httpErr.Body)
	}
	if httpErr.ContentType != "text/html" {
		t.Fatalf("content type = %q", httpErr.ContentType)
	}
}

func TestJSONRPCResponseIDMismatchIsRejected(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		task := a2a.NewTask("task-1", "ctx-1")
		// Answers a request that was never made.
		resp, _ := a2a.NewResultResponse([]byte(`"999"`), task)
		body, _ := resp.Encode()
		w.Header().Set("Content-Type", a2a.ContentTypeJSON)
		_, _ = w.Write(body)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(srv.URL))

	_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	var protoErr *a2a.Error
	if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeInvalidAgentResponse {
		t.Fatalf("error = %v, want InvalidAgentResponseError for a mismatched id", err)
	}
}

func TestMalformedUnaryResponseIsTyped(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", "definitely not json"},
		{"not jsonrpc", `{"result":{"id":"t1"}}`},
		{"neither result nor error", `{"jsonrpc":"2.0","id":"1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", a2a.ContentTypeJSON)
				_, _ = w.Write([]byte(tc.body))
			})
			client := mustClient(t, a2aclient.WithJSONRPCEndpoint(srv.URL))

			_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
			var protoErr *a2a.Error
			if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeInvalidAgentResponse {
				t.Fatalf("error = %v (%T), want InvalidAgentResponseError", err, err)
			}
		})
	}
}

func TestUnusableTaskInResponseIsRejected(t *testing.T) {
	cases := []struct {
		name string
		task a2a.Task
	}{
		{"no id", a2a.Task{Status: a2a.NewTaskStatus(a2a.TaskStateWorking)}},
		{"unknown state", a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: "TASK_STATE_ASCENDED"}}},
		{"unspecified state", a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateUnspecified}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := tc.task
			srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
				resp, _ := a2a.NewResultResponse([]byte(`"1"`), task)
				body, _ := resp.Encode()
				w.Header().Set("Content-Type", a2a.ContentTypeJSON)
				_, _ = w.Write(body)
			})
			client := mustClient(t, a2aclient.WithJSONRPCEndpoint(srv.URL))

			_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
			var protoErr *a2a.Error
			if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeInvalidAgentResponse {
				t.Fatalf("error = %v, want InvalidAgentResponseError", err)
			}
		})
	}
}

func TestSendMessageResponseOneofIsEnforced(t *testing.T) {
	cases := []struct {
		name string
		body a2a.SendMessageResponse
	}{
		{"neither arm", a2a.SendMessageResponse{}},
		{"both arms", a2a.SendMessageResponse{
			Task:    &a2a.Task{ID: "t1", Status: a2a.NewTaskStatus(a2a.TaskStateWorking)},
			Message: &a2a.Message{MessageID: "m1", Role: a2a.RoleAgent, Parts: []a2a.Part{a2a.TextPart("hi")}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.body
			srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
				resp, _ := a2a.NewResultResponse([]byte(`"1"`), result)
				body, _ := resp.Encode()
				w.Header().Set("Content-Type", a2a.ContentTypeJSON)
				_, _ = w.Write(body)
			})
			client := mustClient(t, a2aclient.WithJSONRPCEndpoint(srv.URL))

			_, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
				Message: a2a.NewUserMessage("m1", "hi"),
			})
			var protoErr *a2a.Error
			if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeInvalidAgentResponse {
				t.Fatalf("error = %v, want InvalidAgentResponseError", err)
			}
		})
	}
}

func TestContextCancellationAbortsUnaryCall(t *testing.T) {
	release := make(chan struct{})
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The body must be drained before net/http will notice the client
		// hanging up and cancel the request context; without it this handler
		// blocks forever and the test measures the harness, not the client.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	client := mustClient(t,
		a2aclient.WithJSONRPCEndpoint(srv.URL),
		a2aclient.WithHTTPClient(noKeepAlive()),
	)

	// Sampled after the test server exists so its accept loop is not counted as
	// this package's leak.
	baseline := goroutineBaseline()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(50 * time.Millisecond)
		cancel()
	}()
	_, err := client.GetTask(ctx, a2a.GetTaskRequest{ID: "t1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	cancel()
	assertNoLeakedGoroutines(t, baseline)
}

func TestRequestTimeoutBoundsControlPlaneCall(t *testing.T) {
	release := make(chan struct{})
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The body must be drained before net/http will notice the client
		// hanging up and cancel the request context; without it this handler
		// blocks forever and the test measures the harness, not the client.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	client := mustClient(t,
		a2aclient.WithJSONRPCEndpoint(srv.URL),
		a2aclient.WithRequestTimeout(80*time.Millisecond),
		a2aclient.WithRetryPolicy(a2aclient.NoRetry()),
	)

	_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
}
