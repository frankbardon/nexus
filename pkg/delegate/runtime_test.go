package delegate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
)

// fakeProvider answers a single llm.request with a canned llm.response that
// completes the loop immediately (no tool calls).
func newFakeProvider(bus engine.EventBus, content string) func() {
	return bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		_ = bus.Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       content,
			Metadata:      map[string]any{"_source": req.Metadata["_source"]},
			Usage:         events.Usage{TotalTokens: 100},
		})
	}, engine.WithPriority(10))
}

func TestRuntime_Run_HappyPath(t *testing.T) {
	bus := engine.NewEventBus()
	defer newFakeProvider(bus, "done")()

	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{
		Name:         "analyst",
		SystemPrompt: "Be analytical",
		DefaultBudget: posture.ResourceBudget{
			Timeout:      5 * time.Second,
			MaxTokens:    1000,
			MaxToolCalls: 0,
		},
	})

	r := &Runtime{
		Registry: reg,
		Bus:      bus,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxDepth: 3,
	}

	out, err := r.Run(context.Background(), Input{
		Posture: "analyst",
		Task:    "Summarize.",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != StatusSuccess {
		t.Errorf("Status = %v, want success", out.Status)
	}
	if out.Result != "done" {
		t.Errorf("Result = %q", out.Result)
	}
	if out.Depth != 1 {
		t.Errorf("Depth = %d, want 1", out.Depth)
	}
}

func TestRuntime_Run_UnknownPostureErrors(t *testing.T) {
	r := &Runtime{
		Registry: posture.NewRegistry(),
		Bus:      engine.NewEventBus(),
	}
	out, err := r.Run(context.Background(), Input{Posture: "nope", Task: "x"})
	if err == nil {
		t.Errorf("expected err")
	}
	if out.Status != StatusError {
		t.Errorf("status = %v", out.Status)
	}
}

func TestRuntime_Run_RecursionLimit(t *testing.T) {
	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{Name: "x"})
	r := &Runtime{
		Registry: reg,
		Bus:      engine.NewEventBus(),
		MaxDepth: 2,
	}
	_, err := r.Run(context.Background(), Input{
		Posture:     "x",
		Task:        "go",
		ParentDepth: 2, // depth becomes 3, > MaxDepth
	})
	if !errors.Is(err, ErrRecursionLimit) {
		t.Errorf("err = %v, want ErrRecursionLimit", err)
	}
}

func TestRuntime_Run_CacheHitSkipsLLM(t *testing.T) {
	bus := engine.NewEventBus()
	calls := 0
	defer bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		calls++
		req := ev.Payload.(events.LLMRequest)
		_ = bus.Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "fresh",
			Metadata:      map[string]any{"_source": req.Metadata["_source"]},
		})
	}, engine.WithPriority(10))()

	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{Name: "p"})
	r := &Runtime{
		Registry: reg,
		Bus:      bus,
		Cache:    NewMemoryCache(8),
	}

	in := Input{Posture: "p", Task: "same"}
	out1, _ := r.Run(context.Background(), in)
	if out1.Status != StatusSuccess {
		t.Fatalf("first call status = %v", out1.Status)
	}
	if calls != 1 {
		t.Fatalf("after first run: calls = %d, want 1", calls)
	}

	out2, _ := r.Run(context.Background(), in)
	if out2.Status != StatusCacheHit {
		t.Errorf("second call status = %v, want cache_hit", out2.Status)
	}
	if calls != 1 {
		t.Errorf("after second run: calls = %d, want still 1", calls)
	}
}

func TestRuntime_Run_BudgetMaxTokensPartial(t *testing.T) {
	bus := engine.NewEventBus()
	defer bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		req := ev.Payload.(events.LLMRequest)
		_ = bus.Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "huge",
			Metadata:      map[string]any{"_source": req.Metadata["_source"]},
			Usage:         events.Usage{TotalTokens: 5000},
		})
	}, engine.WithPriority(10))()

	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{
		Name:          "p",
		DefaultBudget: posture.ResourceBudget{MaxTokens: 1000},
	})
	r := &Runtime{Registry: reg, Bus: bus}
	out, _ := r.Run(context.Background(), Input{Posture: "p", Task: "x"})
	if out.Status != StatusPartial {
		t.Errorf("status = %v, want partial", out.Status)
	}
}

func TestMemoryCache_LRUEviction(t *testing.T) {
	c := NewMemoryCache(2)
	c.Put("a", Output{Result: "A"})
	c.Put("b", Output{Result: "B"})
	c.Put("c", Output{Result: "C"})
	if _, ok := c.Get("a"); ok {
		t.Errorf("a should be evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Errorf("b should remain")
	}
	if _, ok := c.Get("c"); !ok {
		t.Errorf("c should remain")
	}
}

// TestRuntime_Run_PropagatesParentSessionID pins the causation contract across
// the sub-session boundary: events emitted while a delegate call is in flight
// must carry the PARENT's SessionID.
//
// The regression it guards is silent and total. A pushed CausationContext
// replaces the active one whole — currentCausationContext returns the top of
// the stack with no per-field merge — so a frame that omitted SessionID blanked
// it rather than inheriting it, and every event a delegated sub-agent emitted
// carried Causation.SessionID == "". Nothing failed; the events were simply
// stranded outside the session they belonged to, which breaks any consumer that
// keys on the session (authorization gates, per-session projections, replay).
func TestRuntime_Run_PropagatesParentSessionID(t *testing.T) {
	const parentSession = "sess-parent-1"

	bus := engine.NewEventBus()
	defer newFakeProvider(bus, "done")()

	// StartSession's effect: the bus-wide default carries the session, which is
	// what the parent goroutine sees before the delegate pushes its own frame.
	cc, ok := bus.(engine.CausationController)
	if !ok {
		t.Fatalf("bus does not implement CausationController")
	}
	cc.SetDefaultCausationContext(engine.CausationContext{SessionID: parentSession})

	// Observe an event the SUB-AGENT causes, not one the runtime emits directly:
	// llm.request is published from inside the delegated loop, under the pushed
	// frame, which is exactly where the blanking occurred.
	var got []string
	defer bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		got = append(got, ev.Causation.SessionID)
	}, engine.WithPriority(1))()

	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{
		Name:          "analyst",
		SystemPrompt:  "Be analytical",
		DefaultBudget: posture.ResourceBudget{Timeout: 5 * time.Second, MaxTokens: 1000},
	})

	r := &Runtime{
		Registry: reg,
		Bus:      bus,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxDepth: 3,
	}

	if _, err := r.Run(context.Background(), Input{
		Posture: "analyst",
		Task:    "Summarize.",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(got) == 0 {
		t.Fatalf("no llm.request observed; the test cannot prove anything")
	}
	for i, s := range got {
		if s != parentSession {
			t.Errorf("llm.request[%d] Causation.SessionID = %q, want %q "+
				"(delegate must carry the parent's session across the sub-session boundary)",
				i, s, parentSession)
		}
	}
}
