package fanout

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// mockBus is a minimal EventBus implementation for testing.
type mockBus struct {
	mu     sync.Mutex
	events []emittedEvent
}

type emittedEvent struct {
	Type    string
	Payload any
}

func (b *mockBus) Emit(eventType string, payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, emittedEvent{Type: eventType, Payload: payload})
	return nil
}

func (b *mockBus) EmitEvent(_ engine.Event[any]) error { return nil }

func (b *mockBus) EmitAsync(eventType string, payload any) <-chan error {
	ch := make(chan error, 1)
	ch <- b.Emit(eventType, payload)
	close(ch)
	return ch
}

func (b *mockBus) Subscribe(_ string, _ engine.HandlerFunc, _ ...engine.SubscribeOption) func() {
	return func() {}
}

func (b *mockBus) SubscribeAll(_ engine.HandlerFunc) func() {
	return func() {}
}

func (b *mockBus) SubscribeAllReplay(_ engine.HandlerFunc) func() {
	return func() {}
}

func (b *mockBus) EmitVetoable(_ string, _ any) (engine.VetoResult, error) {
	return engine.VetoResult{}, nil
}

func (b *mockBus) Drain(_ context.Context) error { return nil }

func (b *mockBus) emitted() []emittedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]emittedEvent, len(b.events))
	copy(cp, b.events)
	return cp
}

// helper to build test responses with fanout metadata.
func makeResponse(provider, model, content string, cost float64) events.LLMResponse {
	return events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: content,
		Model:   model,
		CostUSD: cost,
		Usage: events.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
		Metadata: map[string]any{
			"_fanout_id":       "test-fanout",
			"_fanout_provider": provider,
			"_target_provider": provider,
			"_source":          pluginID,
		},
	}
}

func newTestPlugin() (*Plugin, *mockBus) {
	bus := &mockBus{}
	p := &Plugin{
		bus:            bus,
		logger:         slog.Default(),
		cfg:            config{deadline: 5 * time.Second},
		inflight:       make(map[string]*fanoutState),
		pendingChoices: make(map[string]chan int),
	}
	return p, bus
}

func TestPresentToUser_BuildsCorrectOptions(t *testing.T) {
	p, bus := newTestPlugin()

	responses := []events.LLMResponse{
		makeResponse("nexus.llm.anthropic", "claude-3", "Hello from Claude", 0.01),
		makeResponse("nexus.llm.openai", "gpt-4", "Hello from GPT", 0.02),
	}

	state := &fanoutState{
		role:     "compare",
		strategy: StrategyUser,
	}

	// Use a very short deadline so the goroutine cleans up quickly.
	p.cfg.deadline = 50 * time.Millisecond
	origNewTimer := newTimer
	newTimer = func(d time.Duration) *time.Timer { return time.NewTimer(10 * time.Millisecond) }
	defer func() { newTimer = origNewTimer }()

	p.presentToUser("test-fanout", state, responses)

	// Give the goroutine time to emit the choose event.
	time.Sleep(5 * time.Millisecond)

	emitted := bus.emitted()

	// Find the provider.fanout.choose event.
	var chooseEvent *events.ProviderFanoutChoose
	for _, e := range emitted {
		if e.Type == "provider.fanout.choose" {
			if c, ok := e.Payload.(events.ProviderFanoutChoose); ok {
				chooseEvent = &c
				break
			}
		}
	}

	if chooseEvent == nil {
		t.Fatal("expected provider.fanout.choose event to be emitted")
	}

	if chooseEvent.FanoutID != "test-fanout" {
		t.Errorf("expected fanoutID %q, got %q", "test-fanout", chooseEvent.FanoutID)
	}
	if chooseEvent.Role != "compare" {
		t.Errorf("expected role %q, got %q", "compare", chooseEvent.Role)
	}
	if len(chooseEvent.Responses) != 2 {
		t.Fatalf("expected 2 options, got %d", len(chooseEvent.Responses))
	}

	opt0 := chooseEvent.Responses[0]
	if opt0.Index != 0 || opt0.Provider != "nexus.llm.anthropic" || opt0.Model != "claude-3" {
		t.Errorf("option 0 mismatch: %+v", opt0)
	}
	if opt0.Content != "Hello from Claude" {
		t.Errorf("option 0 content mismatch: got %q", opt0.Content)
	}
	if opt0.CostUSD != 0.01 {
		t.Errorf("option 0 cost mismatch: got %f", opt0.CostUSD)
	}

	opt1 := chooseEvent.Responses[1]
	if opt1.Index != 1 || opt1.Provider != "nexus.llm.openai" || opt1.Model != "gpt-4" {
		t.Errorf("option 1 mismatch: %+v", opt1)
	}

	// Wait for timeout fallback to complete.
	time.Sleep(30 * time.Millisecond)
}

func TestUserChoice_ValidIndex(t *testing.T) {
	p, bus := newTestPlugin()

	responses := []events.LLMResponse{
		makeResponse("nexus.llm.anthropic", "claude-3", "Hello from Claude", 0.01),
		makeResponse("nexus.llm.openai", "gpt-4", "Hello from GPT", 0.02),
		makeResponse("nexus.llm.google", "gemini", "Hello from Gemini", 0.005),
	}

	state := &fanoutState{
		role:     "compare",
		strategy: StrategyUser,
	}

	// Use a long deadline so it doesn't fire during the test.
	p.cfg.deadline = 10 * time.Second
	origNewTimer := newTimer
	newTimer = func(d time.Duration) *time.Timer { return time.NewTimer(d) }
	defer func() { newTimer = origNewTimer }()

	p.presentToUser("test-fanout", state, responses)

	// Simulate user choosing index 1 (GPT-4).
	p.mu.Lock()
	ch, exists := p.pendingChoices["test-fanout"]
	p.mu.Unlock()

	if !exists {
		t.Fatal("expected pending choice channel to exist")
	}

	ch <- 1

	// Wait for the goroutine to process.
	time.Sleep(20 * time.Millisecond)

	emitted := bus.emitted()

	// Find the llm.response event.
	var finalResp *events.LLMResponse
	for _, e := range emitted {
		if e.Type == "llm.response" {
			if r, ok := e.Payload.(events.LLMResponse); ok {
				finalResp = &r
				break
			}
		}
	}

	if finalResp == nil {
		t.Fatal("expected llm.response event to be emitted")
	}

	// The chosen response (index 1, GPT-4) should be primary.
	if finalResp.Model != "gpt-4" {
		t.Errorf("expected primary model %q, got %q", "gpt-4", finalResp.Model)
	}
	if finalResp.Content != "Hello from GPT" {
		t.Errorf("expected primary content %q, got %q", "Hello from GPT", finalResp.Content)
	}

	// Alternatives should contain the other two.
	if len(finalResp.Alternatives) != 2 {
		t.Fatalf("expected 2 alternatives, got %d", len(finalResp.Alternatives))
	}
	if finalResp.Alternatives[0].Model != "claude-3" {
		t.Errorf("expected first alternative model %q, got %q", "claude-3", finalResp.Alternatives[0].Model)
	}
	if finalResp.Alternatives[1].Model != "gemini" {
		t.Errorf("expected second alternative model %q, got %q", "gemini", finalResp.Alternatives[1].Model)
	}

	// Verify pending choice was cleaned up.
	p.mu.Lock()
	_, stillPending := p.pendingChoices["test-fanout"]
	p.mu.Unlock()
	if stillPending {
		t.Error("expected pending choice to be cleaned up")
	}
}

func TestUserChoice_NegativeIndex_FallsBack(t *testing.T) {
	p, bus := newTestPlugin()

	responses := []events.LLMResponse{
		makeResponse("nexus.llm.anthropic", "claude-3", "Hello from Claude", 0.01),
		makeResponse("nexus.llm.openai", "gpt-4", "Hello from GPT", 0.02),
	}

	state := &fanoutState{
		role:     "compare",
		strategy: StrategyUser,
	}

	p.cfg.deadline = 10 * time.Second
	origNewTimer := newTimer
	newTimer = func(d time.Duration) *time.Timer { return time.NewTimer(d) }
	defer func() { newTimer = origNewTimer }()

	p.presentToUser("test-fanout", state, responses)

	// Simulate user declining (index -1).
	p.mu.Lock()
	ch := p.pendingChoices["test-fanout"]
	p.mu.Unlock()

	ch <- -1

	// Wait for the goroutine to process.
	time.Sleep(20 * time.Millisecond)

	emitted := bus.emitted()

	// Find the llm.response event.
	var finalResp *events.LLMResponse
	for _, e := range emitted {
		if e.Type == "llm.response" {
			if r, ok := e.Payload.(events.LLMResponse); ok {
				finalResp = &r
				break
			}
		}
	}

	if finalResp == nil {
		t.Fatal("expected llm.response event to be emitted")
	}

	// With fallback to "all" strategy, first response stays primary.
	if finalResp.Model != "claude-3" {
		t.Errorf("expected primary model %q (fallback), got %q", "claude-3", finalResp.Model)
	}
	if len(finalResp.Alternatives) != 1 {
		t.Fatalf("expected 1 alternative, got %d", len(finalResp.Alternatives))
	}
	if finalResp.Alternatives[0].Model != "gpt-4" {
		t.Errorf("expected alternative model %q, got %q", "gpt-4", finalResp.Alternatives[0].Model)
	}
}

func TestUserChoice_Timeout_FallsBack(t *testing.T) {
	p, bus := newTestPlugin()

	responses := []events.LLMResponse{
		makeResponse("nexus.llm.anthropic", "claude-3", "Hello from Claude", 0.01),
		makeResponse("nexus.llm.openai", "gpt-4", "Hello from GPT", 0.02),
	}

	state := &fanoutState{
		role:     "compare",
		strategy: StrategyUser,
	}

	// Use a very short deadline to trigger timeout quickly.
	p.cfg.deadline = 20 * time.Millisecond
	origNewTimer := newTimer
	newTimer = func(_ time.Duration) *time.Timer { return time.NewTimer(10 * time.Millisecond) }
	defer func() { newTimer = origNewTimer }()

	p.presentToUser("test-fanout", state, responses)

	// Don't send any choice — let it time out.
	time.Sleep(50 * time.Millisecond)

	emitted := bus.emitted()

	// Find the llm.response event.
	var finalResp *events.LLMResponse
	for _, e := range emitted {
		if e.Type == "llm.response" {
			if r, ok := e.Payload.(events.LLMResponse); ok {
				finalResp = &r
				break
			}
		}
	}

	if finalResp == nil {
		t.Fatal("expected llm.response event after timeout")
	}

	// Timeout should fall back to "all" strategy — first response is primary.
	if finalResp.Model != "claude-3" {
		t.Errorf("expected primary model %q (fallback), got %q", "claude-3", finalResp.Model)
	}
	if len(finalResp.Alternatives) != 1 {
		t.Fatalf("expected 1 alternative, got %d", len(finalResp.Alternatives))
	}

	// Verify pending choice was cleaned up.
	p.mu.Lock()
	_, stillPending := p.pendingChoices["test-fanout"]
	p.mu.Unlock()
	if stillPending {
		t.Error("expected pending choice to be cleaned up after timeout")
	}
}

func TestHandleUserChoice_DeliversToChannel(t *testing.T) {
	p, _ := newTestPlugin()

	// Set up a pending choice.
	ch := make(chan int, 1)
	p.mu.Lock()
	p.pendingChoices["test-fanout"] = ch
	p.mu.Unlock()

	// Simulate receiving a provider.fanout.chosen event.
	p.handleUserChoice(engine.Event[any]{
		Type: "provider.fanout.chosen",
		Payload: events.ProviderFanoutChosen{SchemaVersion: events.ProviderFanoutChosenVersion, FanoutID: "test-fanout",
			ChosenIndex: 2,
		},
	})

	select {
	case idx := <-ch:
		if idx != 2 {
			t.Errorf("expected chosen index 2, got %d", idx)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for choice delivery")
	}
}

func TestHandleUserChoice_UnknownFanoutID(t *testing.T) {
	p, _ := newTestPlugin()

	// Should not panic when fanout ID is not found.
	p.handleUserChoice(engine.Event[any]{
		Type: "provider.fanout.chosen",
		Payload: events.ProviderFanoutChosen{SchemaVersion: events.ProviderFanoutChosenVersion, FanoutID: "nonexistent",
			ChosenIndex: 0,
		},
	})
}
