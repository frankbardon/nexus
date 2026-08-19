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

func TestStreamHappyPathBothBindings(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent(t, agentConfig{})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			stream, err := client.SendStreamingMessage(context.Background(), a2a.SendMessageRequest{
				Message: a2a.NewUserMessage("m1", "go"),
			})
			if err != nil {
				t.Fatalf("SendStreamingMessage: %v", err)
			}
			defer stream.Close()

			var kinds []a2a.StreamPayloadKind
			for frame := range stream.Frames() {
				kinds = append(kinds, frame.Kind())
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream error: %v", err)
			}

			want := []a2a.StreamPayloadKind{
				a2a.StreamPayloadTask,
				a2a.StreamPayloadStatusUpdate,
				a2a.StreamPayloadArtifactUpdate,
				a2a.StreamPayloadStatusUpdate,
			}
			if len(kinds) != len(want) {
				t.Fatalf("frames = %v, want %v", kinds, want)
			}
			for i := range want {
				if kinds[i] != want[i] {
					t.Fatalf("frame %d = %s, want %s", i, kinds[i], want[i])
				}
			}

			result := stream.Result()
			if !result.Terminal || !result.Succeeded() {
				t.Fatalf("result state = %s, terminal = %v", result.State, result.Terminal)
			}
			if result.TaskID != "task-1" || result.ContextID != "ctx-1" {
				t.Fatalf("identity = %s/%s", result.TaskID, result.ContextID)
			}
			if got := result.ArtifactText(); got != "done" {
				t.Fatalf("artifact text = %q, want %q", got, "done")
			}
			if got := result.Header.Get("Content-Type"); !strings.Contains(got, a2a.ContentTypeSSE) {
				t.Fatalf("content type = %q", got)
			}
		})
	}
}

func TestRunDrainsToResult(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := client.Run(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Frames) != 4 || !result.Succeeded() {
		t.Fatalf("result = %+v", result)
	}
}

func TestStreamReassemblesChunkedArtifacts(t *testing.T) {
	agent := newAgent(t, agentConfig{
		streamFrames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			first := a2a.NewArtifact("art-1", a2a.TextPart("Hello, "))
			first.Name = "answer"
			second := a2a.NewArtifact("art-1", a2a.TextPart("world"))
			other := a2a.NewTextArtifact("art-2", "sidecar", "extra")
			return []a2a.StreamResponse{
				a2a.StreamTask(a2a.NewTask("task-1", "ctx-1")),
				a2a.StreamArtifactUpdate(a2a.NewArtifactChunk("task-1", "ctx-1", first, false, false)),
				a2a.StreamArtifactUpdate(a2a.NewArtifactChunk("task-1", "ctx-1", second, true, true)),
				a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate("task-1", "ctx-1", other)),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate("task-1", "ctx-1",
					a2a.NewTaskStatus(a2a.TaskStateCompleted))),
			}
		},
	})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := client.Run(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(result.Artifacts))
	}
	assembled := result.Artifacts[0]
	if assembled.ArtifactID != "art-1" || len(assembled.Parts) != 2 {
		t.Fatalf("assembled = %+v", assembled)
	}
	if assembled.Name != "answer" {
		t.Fatalf("name = %q, want it carried over from the opening chunk", assembled.Name)
	}
	if got := result.ArtifactText(); got != "Hello, \nworld\nextra" {
		t.Fatalf("artifact text = %q", got)
	}
}

func TestStreamResultIsACopy(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := client.SendStreamingMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer stream.Close()
	for range stream.Frames() {
	}

	// A caller that mutates what it was handed must not corrupt the stream's
	// own accumulated state; Result returns a copy, not the live struct.
	result := stream.Result()
	result.Artifacts[0].Parts[0] = a2a.TextPart("tampered")
	result.Artifacts = append(result.Artifacts, a2a.NewTextArtifact("a9", "x", "y"))

	again := stream.Result()
	if got := again.ArtifactText(); got != "done" {
		t.Fatalf("artifact text = %q after caller mutation, want %q", got, "done")
	}
	if len(again.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(again.Artifacts))
	}
}

func TestStreamParkedOnInputRequiredEndsCleanly(t *testing.T) {
	agent := newAgent(t, agentConfig{
		streamFrames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return interruptedRun("task-2", "ctx-2", "which environment?")
		},
	})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := client.Run(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "deploy"),
	})
	// A parked stream that the remote then closes is an answerable
	// interruption, not a truncation.
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	question, interrupted := result.Interrupt()
	if !interrupted {
		t.Fatalf("Interrupt() = false, state %s", result.State)
	}
	if question == nil {
		t.Fatal("no question attached to the interruption")
	}
	if got := result.StatusText(); got != "which environment?" {
		t.Fatalf("status text = %q", got)
	}
	if result.Terminal {
		t.Fatal("an interrupted task is not terminal")
	}
}

func TestSubscribeToTask(t *testing.T) {
	for _, tc := range bindings {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent(t, agentConfig{
				subscribeFrames: func(req *a2a.SubscribeToTaskRequest) []a2a.StreamResponse {
					// A subscribe stream opens on a task already in flight.
					task := a2a.NewTask(req.ID, "ctx-5")
					task.Status = a2a.NewTaskStatus(a2a.TaskStateWorking)
					return []a2a.StreamResponse{
						a2a.StreamTask(task),
						a2a.StreamStatusUpdate(a2a.NewStatusUpdate(req.ID, "ctx-5",
							a2a.NewTaskStatus(a2a.TaskStateCompleted))),
					}
				},
			})
			client, err := a2aclient.New(agent.URL(), a2aclient.WithBinding(tc.binding))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			stream, err := client.SubscribeToTask(context.Background(), a2a.SubscribeToTaskRequest{ID: "task-live"})
			if err != nil {
				t.Fatalf("SubscribeToTask: %v", err)
			}
			defer stream.Close()
			for range stream.Frames() {
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream error: %v", err)
			}
			if got := stream.Result().State; got != a2a.TaskStateCompleted {
				t.Fatalf("state = %s", got)
			}
		})
	}
}

func TestStreamingRefusedWhenCardDeniesIt(t *testing.T) {
	agent := newAgent(t, agentConfig{noStreaming: true})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.SendStreamingMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	var bindErr *a2aclient.BindingError
	if !errors.As(err, &bindErr) {
		t.Fatalf("error = %v (%T), want *BindingError", err, err)
	}
	if bindErr.Operation != a2a.MethodSendStreamingMessage {
		t.Fatalf("operation = %q", bindErr.Operation)
	}
	// Nothing was sent: the refusal is local.
	for _, seen := range agent.seen() {
		if seen.Path != a2a.AgentCardPath {
			t.Fatalf("a streaming request reached %s despite the card", seen.Path)
		}
	}
}

// ---- Non-conformant remotes ----

// sseServer answers every request with a fixed SSE body.
func sseServer(t *testing.T, body string) string {
	t.Helper()
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a2a.WriteSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	return srv.URL
}

// restStreamClient pins the REST binding at a raw server, so a test can put
// arbitrary bytes on the wire without the codec's writer sanitizing them.
func restStreamClient(t *testing.T, endpoint string, opts ...a2aclient.Option) *a2aclient.Client {
	t.Helper()
	all := append([]a2aclient.Option{
		a2aclient.WithBinding(a2a.BindingHTTPJSON),
		a2aclient.WithRESTEndpoint(endpoint),
	}, opts...)
	return mustClient(t, all...)
}

func streamOnce(t *testing.T, client *a2aclient.Client) (a2aclient.StreamResult, error) {
	t.Helper()
	return client.Run(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
}

func TestStreamMalformedFrame(t *testing.T) {
	endpoint := sseServer(t, sseBody(`{"task":{"id":"t1","status":{"state":"TASK_STATE_SUBMITTED"}}}`, `{not json`))
	client := restStreamClient(t, endpoint)

	result, err := streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonMalformed {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonMalformed)
	}
	// The frames decoded before the failure survive, so a caller can see how
	// far the remote got.
	if len(result.Frames) != 1 || streamErr.Frames != 1 {
		t.Fatalf("frames = %d / %d, want 1", len(result.Frames), streamErr.Frames)
	}
}

func TestStreamContractViolations(t *testing.T) {
	task := `{"task":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_SUBMITTED"}}}`

	cases := []struct {
		name string
		body string
	}{
		{
			name: "opens with a status update",
			body: sseBody(`{"statusUpdate":{"taskId":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"}}}`),
		},
		{
			name: "second task frame mid-stream",
			body: sseBody(task, task),
		},
		{
			name: "update names a different task",
			body: sseBody(task,
				`{"statusUpdate":{"taskId":"OTHER","contextId":"c1","status":{"state":"TASK_STATE_WORKING"}}}`),
		},
		{
			name: "illegal state transition",
			body: sseBody(task,
				`{"statusUpdate":{"taskId":"t1","contextId":"c1","status":{"state":"TASK_STATE_WORKING"}}}`,
				`{"statusUpdate":{"taskId":"t1","contextId":"c1","status":{"state":"TASK_STATE_SUBMITTED"}}}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := restStreamClient(t, sseServer(t, tc.body))
			_, err := streamOnce(t, client)
			var streamErr *a2aclient.StreamError
			if !errors.As(err, &streamErr) {
				t.Fatalf("error = %v (%T), want *StreamError", err, err)
			}
			if streamErr.Reason != a2aclient.StreamReasonProtocol {
				t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonProtocol)
			}
			var protoErr *a2a.Error
			if !errors.As(err, &protoErr) {
				t.Fatalf("cause = %v, want an *a2a.Error to be recoverable", streamErr.Err)
			}
		})
	}
}

func TestStreamUnexpectedTerminalStateIsRejected(t *testing.T) {
	// A state nobody has ever heard of, reported as terminal by a remote that
	// invented it.
	body := sseBody(
		`{"task":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_SUBMITTED"}}}`,
		`{"statusUpdate":{"taskId":"t1","contextId":"c1","status":{"state":"TASK_STATE_ASCENDED"}}}`,
	)
	client := restStreamClient(t, sseServer(t, body))

	_, err := streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonMalformed && streamErr.Reason != a2aclient.StreamReasonProtocol {
		t.Fatalf("reason = %s, want a malformed or protocol failure", streamErr.Reason)
	}
}

func TestStreamTruncated(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no frames at all", ""},
		{"stops while working", sseBody(
			`{"task":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_SUBMITTED"}}}`,
			`{"statusUpdate":{"taskId":"t1","contextId":"c1","status":{"state":"TASK_STATE_WORKING"}}}`,
		)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := restStreamClient(t, sseServer(t, tc.body))
			_, err := streamOnce(t, client)
			var streamErr *a2aclient.StreamError
			if !errors.As(err, &streamErr) {
				t.Fatalf("error = %v (%T), want *StreamError", err, err)
			}
			if streamErr.Reason != a2aclient.StreamReasonTruncated {
				t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonTruncated)
			}
		})
	}
}

func TestStreamRemoteErrorFrame(t *testing.T) {
	protoErr := a2a.ErrTaskNotFound("t1")
	_, restBody := protoErr.RESTError()
	encoded, err := a2a.Encode(restBody)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	client := restStreamClient(t, sseServer(t, sseBody(string(encoded))))

	_, err = streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonRemoteError {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonRemoteError)
	}
	var recovered *a2a.Error
	if !errors.As(err, &recovered) || recovered.Type != a2a.ErrorTypeTaskNotFound {
		t.Fatalf("cause = %v, want TaskNotFoundError", streamErr.Err)
	}
}

func TestStreamHTTPErrorWhereSSEWasPromised(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeRESTError(w, a2a.ErrTaskNotFound("t1"))
	})
	client := restStreamClient(t, srv.URL, a2aclient.WithRetryPolicy(a2aclient.NoRetry()))

	_, err := streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonNotSSE {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonNotSSE)
	}
	// The protocol error the remote sent is still recoverable.
	var protoErr *a2a.Error
	if !errors.As(err, &protoErr) || protoErr.Type != a2a.ErrorTypeTaskNotFound {
		t.Fatalf("cause = %v, want TaskNotFoundError", streamErr.Err)
	}
}

func TestStreamNonSSEContentType(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"task":{"id":"t1"}}`)
	})
	client := restStreamClient(t, srv.URL)

	_, err := streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonNotSSE {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonNotSSE)
	}
	if !strings.Contains(streamErr.Detail, "application/json") {
		t.Fatalf("detail = %q, want it to name the content type", streamErr.Detail)
	}
}

// ---- Cancellation, idleness and leaks ----

// blockingStreamServer opens a stream, writes an opening frame, then holds the
// connection until the request context is cancelled.
func blockingStreamServer(t *testing.T, keepalive time.Duration) string {
	t.Helper()
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a2a.WriteSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		sw := a2a.NewSSEWriter(w)
		if err := sw.WriteTask(a2a.NewTask("t1", "c1")); err != nil {
			return
		}
		for {
			if keepalive <= 0 {
				<-r.Context().Done()
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(keepalive):
				if err := sw.Ping(); err != nil {
					return
				}
			}
		}
	})
	return srv.URL
}

func TestStreamContextCancellationLeaksNothing(t *testing.T) {
	endpoint := blockingStreamServer(t, 0)
	client := restStreamClient(t, endpoint, a2aclient.WithHTTPClient(noKeepAlive()))
	baseline := goroutineBaseline()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.SendStreamingMessage(ctx, a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	if err != nil {
		cancel()
		t.Fatalf("SendStreamingMessage: %v", err)
	}

	// Consume the opening frame so the stream is genuinely live, then cancel.
	if _, ok := <-stream.Frames(); !ok {
		cancel()
		t.Fatal("stream closed before the opening frame")
	}
	cancel()

	for range stream.Frames() {
	}
	var streamErr *a2aclient.StreamError
	if !errors.As(stream.Err(), &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", stream.Err(), stream.Err())
	}
	if streamErr.Reason != a2aclient.StreamReasonCanceled {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonCanceled)
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("error %v does not wrap context.Canceled", stream.Err())
	}
	_ = stream.Close()
	assertNoLeakedGoroutines(t, baseline)
}

func TestStreamCloseAbandonsWithoutLeaking(t *testing.T) {
	endpoint := blockingStreamServer(t, 0)
	client := restStreamClient(t, endpoint, a2aclient.WithHTTPClient(noKeepAlive()))
	baseline := goroutineBaseline()

	stream, err := client.SendStreamingMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	if _, ok := <-stream.Frames(); !ok {
		t.Fatal("stream closed before the opening frame")
	}

	// Abandon without draining: Close must both release the response and
	// unblock the reader.
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent.
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	for range stream.Frames() {
	}
	var streamErr *a2aclient.StreamError
	if !errors.As(stream.Err(), &streamErr) {
		t.Fatalf("error = %v, want *StreamError", stream.Err())
	}
	if streamErr.Reason != a2aclient.StreamReasonCanceled {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonCanceled)
	}
	assertNoLeakedGoroutines(t, baseline)
}

func TestStreamIdleTimeout(t *testing.T) {
	endpoint := blockingStreamServer(t, 0)
	client := restStreamClient(t, endpoint,
		a2aclient.WithHTTPClient(noKeepAlive()),
		a2aclient.WithStreamIdleTimeout(120*time.Millisecond),
	)
	baseline := goroutineBaseline()

	result, err := streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonIdle {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonIdle)
	}
	if len(result.Frames) != 1 {
		t.Fatalf("frames = %d, want the opening frame to be retained", len(result.Frames))
	}
	assertNoLeakedGoroutines(t, baseline)
}

func TestStreamKeepaliveCommentsDeferIdleTimeout(t *testing.T) {
	// The remote sends nothing but SSE comments. Because idleness is measured
	// in bytes rather than frames, the stream survives.
	endpoint := blockingStreamServer(t, 30*time.Millisecond)
	client := restStreamClient(t, endpoint,
		a2aclient.WithHTTPClient(noKeepAlive()),
		a2aclient.WithStreamIdleTimeout(150*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	stream, err := client.SendStreamingMessage(ctx, a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer stream.Close()
	for range stream.Frames() {
	}

	var streamErr *a2aclient.StreamError
	if !errors.As(stream.Err(), &streamErr) {
		t.Fatalf("error = %v, want *StreamError", stream.Err())
	}
	// The context deadline ended it, not the idle watchdog.
	if streamErr.Reason != a2aclient.StreamReasonCanceled {
		t.Fatalf("reason = %s, want the deadline to win over the idle timeout", streamErr.Reason)
	}
}

func TestStreamOpenTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	client := restStreamClient(t, srv.URL,
		a2aclient.WithHTTPClient(noKeepAlive()),
		a2aclient.WithStreamOpenTimeout(100*time.Millisecond),
		a2aclient.WithRetryPolicy(a2aclient.NoRetry()),
	)
	baseline := goroutineBaseline()

	_, err := streamOnce(t, client)
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonOpenTimeout {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonOpenTimeout)
	}
	assertNoLeakedGoroutines(t, baseline)
}

func TestStreamCancelledBeforeOpening(t *testing.T) {
	release := make(chan struct{})
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	client := restStreamClient(t, srv.URL,
		a2aclient.WithHTTPClient(noKeepAlive()),
		a2aclient.WithRetryPolicy(a2aclient.NoRetry()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	_, err := client.SendStreamingMessage(ctx, a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "go"),
	})
	var streamErr *a2aclient.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if streamErr.Reason != a2aclient.StreamReasonCanceled {
		t.Fatalf("reason = %s, want %s", streamErr.Reason, a2aclient.StreamReasonCanceled)
	}
}

func TestStreamEndsAtTerminalFrameWithoutWaitingForClose(t *testing.T) {
	// A remote that reaches a terminal state and then forgets to close the
	// connection. The client must not sit there.
	agent := newAgent(t, agentConfig{holdOpen: true})
	client, err := a2aclient.New(agent.URL(),
		// No idle timeout, so a hang here would hang the test rather than be
		// rescued by a watchdog.
		a2aclient.WithStreamIdleTimeout(0),
		a2aclient.WithHTTPClient(noKeepAlive()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	baseline := goroutineBaseline()

	done := make(chan a2aclient.StreamResult, 1)
	go func() {
		result, runErr := client.Run(context.Background(), a2a.SendMessageRequest{
			Message: a2a.NewUserMessage("m1", "go"),
		})
		if runErr != nil {
			t.Errorf("Run: %v", runErr)
		}
		done <- result
	}()

	select {
	case result := <-done:
		if !result.Succeeded() {
			t.Fatalf("state = %s, want COMPLETED", result.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end at the terminal frame")
	}
	assertNoLeakedGoroutines(t, baseline)
}

func TestStreamEndsAtTerminalOpeningSnapshot(t *testing.T) {
	// SubscribeToTask against a task that is already finished: the opening
	// snapshot is itself terminal and closes the stream at once.
	agent := newAgent(t, agentConfig{
		holdOpen: true,
		subscribeFrames: func(req *a2a.SubscribeToTaskRequest) []a2a.StreamResponse {
			task := a2a.NewTask(req.ID, "ctx-1")
			task.Status = a2a.NewTaskStatus(a2a.TaskStateFailed)
			return []a2a.StreamResponse{a2a.StreamTask(task)}
		},
	})
	client, err := a2aclient.New(agent.URL(),
		a2aclient.WithStreamIdleTimeout(0),
		a2aclient.WithHTTPClient(noKeepAlive()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := client.SubscribeToTask(context.Background(), a2a.SubscribeToTaskRequest{ID: "task-old"})
	if err != nil {
		t.Fatalf("SubscribeToTask: %v", err)
	}
	defer stream.Close()

	done := make(chan struct{})
	go func() {
		for range stream.Frames() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end on a terminal opening snapshot")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	result := stream.Result()
	if !result.Failed() || !result.Terminal {
		t.Fatalf("result state = %s, terminal = %v", result.State, result.Terminal)
	}
}

func TestStreamIgnoresFramesAfterATerminalState(t *testing.T) {
	// The exchange ends at the terminal frame, so trailing garbage the remote
	// keeps sending is never read and cannot turn a completed run into a
	// failure. This is deliberate: the answer was already delivered.
	body := sseBody(
		`{"task":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_SUBMITTED"}}}`,
		`{"statusUpdate":{"taskId":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"}}}`,
		`{"artifactUpdate":{"taskId":"t1","contextId":"c1","artifact":{"artifactId":"a1","parts":[{"text":"late"}]}}}`,
	)
	client := restStreamClient(t, sseServer(t, body))

	result, err := streamOnce(t, client)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(result.Frames) != 2 {
		t.Fatalf("frames = %d, want the stream to stop at the terminal frame", len(result.Frames))
	}
	if len(result.Artifacts) != 0 {
		t.Fatalf("artifacts = %d, want the post-terminal frame ignored", len(result.Artifacts))
	}
}
