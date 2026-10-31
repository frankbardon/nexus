package delegate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// installInterceptor intercepts every before:llm.request (as fanout and the
// test IO mock do) and, when answer is set, publishes from another goroutine
// after the publish returned: a fanout leg sharing the RequestID, then the
// merged response.
func installInterceptor(bus engine.EventBus, answer string) {
	bus.Subscribe("before:llm.request", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		req, _ := vp.Original.(*events.LLMRequest)
		if req == nil {
			return
		}
		engine.InterceptLLMRequest(vp, req, "test.interceptor", "intercepted")
		if answer == "" {
			return
		}
		id := req.RequestID
		go func() {
			time.Sleep(20 * time.Millisecond)
			engine.PublishLLMResponse(bus, events.LLMResponse{RequestID: id, Content: "leg",
				Metadata: map[string]any{"_source": "nexus.provider.fanout", engine.MetaFanoutProvider: "a"}})
			engine.PublishLLMResponse(bus, events.LLMResponse{RequestID: id, Content: answer,
				Metadata: map[string]any{"_source": "nexus.provider.fanout", "_fanout": true}})
		}()
	})
}

func TestSyncLLM_InterceptedRequest_WaitsForAsyncResponse(t *testing.T) {
	bus := engine.NewEventBus()
	installInterceptor(bus, "merged")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := SyncLLM(ctx, bus, events.LLMRequest{Role: "quick",
		Metadata: map[string]any{"_source": "delegate.s1"}})
	if err != nil {
		t.Fatalf("SyncLLM: %v", err)
	}
	if resp.Content != "merged" {
		t.Fatalf("content = %q, want the merged response (not the fanout leg)", resp.Content)
	}
}

func TestSyncLLM_InterceptedRequest_CtxCancelled(t *testing.T) {
	bus := engine.NewEventBus()
	installInterceptor(bus, "") // never answers

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := SyncLLM(ctx, bus, events.LLMRequest{Role: "quick"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestSyncLLM_BlockVeto_Errors(t *testing.T) {
	bus := engine.NewEventBus()
	bus.Subscribe("before:llm.request", func(e engine.Event[any]) {
		if vp, ok := e.Payload.(*engine.VetoablePayload); ok {
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: "budget exhausted"}
		}
	})

	start := time.Now()
	_, err := SyncLLM(context.Background(), bus, events.LLMRequest{Role: "quick"})
	if !errors.Is(err, engine.ErrLLMRequestBlocked) {
		t.Fatalf("err = %v, want ErrLLMRequestBlocked", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("blocked request waited %s", time.Since(start))
	}
}

func TestSyncLLM_PublishedWithoutProvider_NoResponse(t *testing.T) {
	bus := engine.NewEventBus()
	_, err := SyncLLM(context.Background(), bus, events.LLMRequest{Role: "quick"})
	if !errors.Is(err, engine.ErrNoLLMResponse) {
		t.Fatalf("err = %v, want ErrNoLLMResponse", err)
	}
}
