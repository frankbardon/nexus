package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

func interceptedResult() LLMRequestResult {
	return LLMRequestResult{
		Request: events.LLMRequest{Metadata: map[string]any{MetaInterceptedBy: "x"}},
		Veto:    VetoResult{Vetoed: true, Reason: "intercepted"},
	}
}

func TestAwaitLLMResponse_InterceptedWaitsForAsyncResponse(t *testing.T) {
	ch := make(chan string, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		ch <- "answer"
	}()
	got, err := AwaitLLMResponse(context.Background(), interceptedResult(), ch)
	if err != nil || got != "answer" {
		t.Fatalf("got %q, %v; want answer", got, err)
	}
}

func TestAwaitLLMResponse_InterceptedReturnsCtxError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := AwaitLLMResponse(ctx, interceptedResult(), make(chan string))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestAwaitLLMResponse_BlockedFailsAtOnce(t *testing.T) {
	res := LLMRequestResult{Veto: VetoResult{Vetoed: true, Reason: "budget"}}
	ch := make(chan string, 1)
	ch <- "must not be read"
	_, err := AwaitLLMResponse(context.Background(), res, ch)
	if !errors.Is(err, ErrLLMRequestBlocked) {
		t.Fatalf("err = %v, want ErrLLMRequestBlocked", err)
	}
}

func TestAwaitLLMResponse_Published(t *testing.T) {
	res := LLMRequestResult{Published: true}

	ch := make(chan string, 1)
	ch <- "sync"
	if got, err := AwaitLLMResponse(context.Background(), res, ch); err != nil || got != "sync" {
		t.Fatalf("got %q, %v; want sync response", got, err)
	}
	// Nothing delivered inside the emit: not coming, no wait.
	if _, err := AwaitLLMResponse(context.Background(), res, make(chan string)); !errors.Is(err, ErrNoLLMResponse) {
		t.Fatalf("err = %v, want ErrNoLLMResponse", err)
	}
	emitErr := errors.New("emit failed")
	if _, err := AwaitLLMResponse(context.Background(), LLMRequestResult{Published: true, Err: emitErr}, ch); !errors.Is(err, emitErr) {
		t.Fatalf("err = %v, want emit error", err)
	}
}

func TestIsFanoutLegResponse(t *testing.T) {
	if !IsFanoutLegResponse(map[string]any{MetaFanoutProvider: "anthropic"}) {
		t.Error("leg not recognised")
	}
	if IsFanoutLegResponse(map[string]any{"_fanout": true}) || IsFanoutLegResponse(nil) {
		t.Error("merged/untagged response treated as a leg")
	}
}
