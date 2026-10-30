package engine

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// captureLLMRequests records every llm.request that reaches the bus.
func captureLLMRequests(bus EventBus) (func() []events.LLMRequest, func()) {
	var mu sync.Mutex
	var got []events.LLMRequest
	unsub := bus.Subscribe("llm.request", func(e Event[any]) {
		if req, ok := e.Payload.(events.LLMRequest); ok {
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
		}
	})
	return func() []events.LLMRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]events.LLMRequest(nil), got...)
	}, unsub
}

func TestPublishLLMRequest_NoHandlers_Publishes(t *testing.T) {
	bus := NewEventBus()
	seen, unsub := captureLLMRequests(bus)
	defer unsub()

	res := PublishLLMRequest(bus, events.LLMRequest{Role: "quick"})
	if !res.Published || res.Veto.Vetoed || res.Err != nil {
		t.Fatalf("result = %+v, want published", res)
	}
	if !res.AwaitResponse() || res.Blocked() || res.Intercepted() {
		t.Fatalf("predicates wrong for a published request: %+v", res)
	}
	got := seen()
	if len(got) != 1 || got[0].Role != "quick" {
		t.Fatalf("llm.request = %+v, want one with role quick", got)
	}
	if got[0].SchemaVersion != events.LLMRequestVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got[0].SchemaVersion, events.LLMRequestVersion)
	}
}

func TestPublishLLMRequest_MutationIsPublished(t *testing.T) {
	bus := NewEventBus()
	seen, unsub := captureLLMRequests(bus)
	defer unsub()
	bus.Subscribe("before:llm.request", func(e Event[any]) {
		req := e.Payload.(*VetoablePayload).Original.(*events.LLMRequest)
		req.Role = "rerouted"
		req.Tags["gate"] = "seen"
	})

	res := PublishLLMRequest(bus, events.LLMRequest{Role: "quick", Tags: map[string]string{}})
	got := seen()
	if len(got) != 1 || got[0].Role != "rerouted" || got[0].Tags["gate"] != "seen" {
		t.Fatalf("published %+v, want the handler's mutation", got)
	}
	if res.Request.Role != "rerouted" {
		t.Fatalf("result.Request.Role = %q, want the mutated request", res.Request.Role)
	}
}

func TestPublishLLMRequest_VetoBlocks(t *testing.T) {
	bus := NewEventBus()
	seen, unsub := captureLLMRequests(bus)
	defer unsub()
	bus.Subscribe("before:llm.request", func(e Event[any]) {
		e.Payload.(*VetoablePayload).Veto = VetoResult{Vetoed: true, Reason: "budget"}
	})

	res := PublishLLMRequest(bus, events.LLMRequest{})
	if res.Published || !res.Blocked() || res.Intercepted() || res.AwaitResponse() {
		t.Fatalf("result = %+v, want blocked", res)
	}
	if res.Veto.Reason != "budget" {
		t.Fatalf("veto reason = %q", res.Veto.Reason)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("vetoed request emitted %d llm.request", n)
	}
}

func TestPublishLLMRequest_InterceptIsNotABlock(t *testing.T) {
	bus := NewEventBus()
	seen, unsub := captureLLMRequests(bus)
	defer unsub()
	bus.Subscribe("before:llm.request", func(e Event[any]) {
		vp := e.Payload.(*VetoablePayload)
		InterceptLLMRequest(vp, vp.Original.(*events.LLMRequest), "nexus.test.interceptor", "mocked")
	})

	res := PublishLLMRequest(bus, events.LLMRequest{})
	if res.Published || !res.Intercepted() || res.Blocked() || !res.AwaitResponse() {
		t.Fatalf("result = %+v, want intercepted", res)
	}
	if by := res.Request.Metadata[MetaInterceptedBy]; by != "nexus.test.interceptor" {
		t.Fatalf("%s = %v", MetaInterceptedBy, by)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("intercepted request emitted %d llm.request", n)
	}
}

func TestPublishLLMRequest_SeedsSessionTags(t *testing.T) {
	bus := NewEventBus()
	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	eng := &Engine{Bus: bus, Session: ws, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	eng.installTagSeeder()
	seen, unsub := captureLLMRequests(bus)
	defer unsub()

	PublishLLMRequest(bus, events.LLMRequest{Tags: map[string]string{"source_plugin": "nexus.test.producer"}})
	got := seen()
	if len(got) != 1 {
		t.Fatalf("got %d llm.request, want 1", len(got))
	}
	if got[0].Tags["session_id"] != ws.ID || got[0].Tags["source_plugin"] != "nexus.test.producer" {
		t.Fatalf("tags = %v, want session_id %q and the producer's own tag", got[0].Tags, ws.ID)
	}

	// A request with no Tags map at all is seeded too.
	PublishLLMRequest(bus, events.LLMRequest{})
	if got := seen(); len(got) != 2 || got[1].Tags["session_id"] != ws.ID {
		t.Fatalf("nil-Tags request not seeded: %+v", got)
	}
}

func TestPublishLLMRequestAsync_HopIsSynchronous(t *testing.T) {
	bus := NewEventBus()
	arrived := make(chan events.LLMRequest, 1)
	bus.Subscribe("llm.request", func(e Event[any]) {
		arrived <- e.Payload.(events.LLMRequest)
	})
	var hopRan bool
	bus.Subscribe("before:llm.request", func(e Event[any]) {
		hopRan = true
		e.Payload.(*VetoablePayload).Original.(*events.LLMRequest).Role = "mutated"
	})

	res := PublishLLMRequestAsync(bus, events.LLMRequest{})
	if !hopRan || !res.Published {
		t.Fatalf("hop ran=%v result=%+v; want hop done before return", hopRan, res)
	}
	select {
	case req := <-arrived:
		if req.Role != "mutated" {
			t.Fatalf("async llm.request role = %q, want mutated", req.Role)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("async llm.request never arrived")
	}

	bus2 := NewEventBus()
	bus2.Subscribe("before:llm.request", func(e Event[any]) {
		e.Payload.(*VetoablePayload).Veto = VetoResult{Vetoed: true, Reason: "no"}
	})
	if res := PublishLLMRequestAsync(bus2, events.LLMRequest{}); !res.Blocked() {
		t.Fatalf("async veto result = %+v, want blocked", res)
	}
}

func TestPublishLLMRequest_NilBus(t *testing.T) {
	if res := PublishLLMRequest(nil, events.LLMRequest{}); res.Published {
		t.Fatal("nil bus reported published")
	}
}

func TestIsInternalLLMRequest(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{"nil", nil, false},
		{"untagged", map[string]any{"task_kind": "summarise"}, false},
		{"react main", map[string]any{"_source": "nexus.agent.react", "task_kind": "react_main"}, false},
		{"planexec step", map[string]any{"_source": "nexus.agent.planexec", "task_kind": "planexec_step"}, false},
		{"orchestrator synth", map[string]any{"_source": "x", "task_kind": "orchestrator_synthesize"}, false},
		{"subagent", map[string]any{"_source": "x", "task_kind": "subagent"}, false},
		{"delegate", map[string]any{"_source": "x", "task_kind": "delegate"}, false},
		{"compaction", map[string]any{"_source": "nexus.memory.compaction", "task_kind": "compaction"}, true},
		{"gate retry", map[string]any{"_source": "nexus.gate.json_schema", "task_kind": "gate_retry"}, true},
		{"fanout judge", map[string]any{"_source": "nexus.provider.fanout", "task_kind": "fanout_judge"}, true},
		{"tagged no kind", map[string]any{"_source": "third.party"}, true},
	}
	for _, tc := range cases {
		if got := IsInternalLLMRequest(tc.meta); got != tc.want {
			t.Errorf("%s: IsInternalLLMRequest=%v, want %v", tc.name, got, tc.want)
		}
	}
}
