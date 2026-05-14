package oneshot

import (
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"prompt": "hello",
	}))
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}

// TestOneshot_DefersInputUntilCoreReady locks in the race fix: io.input must
// not fire from Ready() — it has to wait for the core.ready event so that
// plugins with slow Ready() (e.g. mcp.client's stdio handshake) finish
// registering catalog entries first. Without the deferral, the agent's
// first LLM request sees an empty tool catalog.
func TestOneshot_DefersInputUntilCoreReady(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"input":      "hello",
		"read_stdin": false,
	}))

	var (
		mu        sync.Mutex
		inputs    []events.UserInput
		gotInput  = make(chan struct{}, 1)
	)
	h.Bus().Subscribe("io.input", func(ev engine.Event[any]) {
		ui, ok := ev.Payload.(events.UserInput)
		if !ok {
			return
		}
		mu.Lock()
		inputs = append(inputs, ui)
		mu.Unlock()
		select {
		case gotInput <- struct{}{}:
		default:
		}
	}, engine.WithSource("test"))

	// Give any (incorrect) Ready-time goroutine a chance to fire. The race
	// fix should keep this window silent.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	prematureFires := len(inputs)
	mu.Unlock()
	if prematureFires != 0 {
		t.Fatalf("io.input fired before core.ready (%d times) — race fix regressed", prematureFires)
	}

	h.Inject("core.ready", nil)

	select {
	case <-gotInput:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for io.input after core.ready")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 1 {
		t.Fatalf("expected exactly 1 io.input, got %d", len(inputs))
	}
	if inputs[0].Content != "hello" {
		t.Fatalf("io.input content = %q, want %q", inputs[0].Content, "hello")
	}
}
