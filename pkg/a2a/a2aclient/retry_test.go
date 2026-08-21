package a2aclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// countingServer answers with the handler, counting attempts.
func countingServer(t *testing.T, handler func(attempt int, w http.ResponseWriter, r *http.Request)) (string, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		handler(int(attempts.Add(1)), w, r)
	})
	return srv.URL, &attempts
}

func okTask(w http.ResponseWriter) {
	resp, _ := a2a.NewResultResponse([]byte(`"1"`), a2a.NewTask("t1", "c1"))
	body, _ := resp.Encode()
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}

func fastRetry() a2aclient.Option {
	return a2aclient.WithRetryPolicy(a2aclient.RetryPolicy{
		MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
	})
}

func TestRetriesTransportFailureForIdempotentRead(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt == 1 {
			// Abort the connection: a transport failure that proves nothing
			// about whether the server acted.
			panic(http.ErrAbortHandler)
		}
		okTask(w)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint), fastRetry())

	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestDoesNotRetryTransportFailureForSendMessage(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint), fastRetry())

	_, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "hi"),
	})
	if err == nil {
		t.Fatal("SendMessage should fail")
	}
	// A message send that may already have been delivered is never replayed.
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a send must not be replayed blind", got)
	}
}

func TestRetriesRateLimitForAnyOperation(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		resp, _ := a2a.NewResultResponse([]byte(`"1"`),
			a2a.MessageResponse(a2a.NewAgentMessage("m2", "ok")))
		body, _ := resp.Encode()
		w.Header().Set("Content-Type", a2a.ContentTypeJSON)
		_, _ = w.Write(body)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint), fastRetry())

	// 429 says the request was NOT processed, so replaying it is safe even for
	// a non-idempotent send.
	if _, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "hi"),
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestDoesNotRetryBadGatewayForSendMessage(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint), fastRetry())

	_, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "hi"),
	})
	var httpErr *a2aclient.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("error = %v, want a 502 HTTPError", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestRetriesBadGatewayForIdempotentRead(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		okTask(w)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint), fastRetry())

	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestRetryExhaustionReportsTheLastFailure(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint), fastRetry())

	_, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	var httpErr *a2aclient.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want a 503 HTTPError", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want the full 3", got)
	}
}

func TestNoRetryPolicyAttemptsOnce(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint),
		a2aclient.WithRetryPolicy(a2aclient.NoRetry()))

	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err == nil {
		t.Fatal("GetTask should fail")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestRetryStopsOnContextCancellation(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint),
		a2aclient.WithRetryPolicy(a2aclient.RetryPolicy{
			MaxAttempts: 10, BaseDelay: 200 * time.Millisecond, MaxDelay: time.Second,
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if _, err := client.GetTask(ctx, a2a.GetTaskRequest{ID: "t1"}); err == nil {
		t.Fatal("GetTask should fail")
	}
	// The first backoff outlives the deadline, so the retry loop must abandon
	// rather than sleep through a dead context.
	if got := attempts.Load(); got > 2 {
		t.Fatalf("attempts = %d, want the loop to stop at cancellation", got)
	}
}

func TestStreamOpenIsRetried(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		a2a.WriteSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		sw := a2a.NewSSEWriter(w)
		task := a2a.NewTask("t1", "c1")
		task.Status = a2a.NewTaskStatus(a2a.TaskStateCompleted)
		_ = sw.WriteTask(task)
	})
	client := restStreamClient(t, endpoint, fastRetry())

	result, err := streamOnce(t, client)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !result.Succeeded() {
		t.Fatalf("state = %s", result.State)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}
