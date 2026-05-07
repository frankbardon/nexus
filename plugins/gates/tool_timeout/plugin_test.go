package tooltimeout

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin builds a wired plugin against a fresh bus. Subscriptions
// match Init() so tests can exercise the real handlers without the
// PluginContext plumbing.
func newTestPlugin(t *testing.T, defaultTimeout time.Duration, perTool map[string]time.Duration) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:            bus,
		logger:         slog.Default(),
		defaultTimeout: defaultTimeout,
		perTool:        perTool,
		inflight:       map[string]*tracker{},
		timedOut:       map[string]struct{}{},
		synthesizing:   map[string]struct{}{},
	}
	if p.perTool == nil {
		p.perTool = map[string]time.Duration{}
	}
	p.unsubs = append(p.unsubs,
		bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(10)),
		bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(5)),
		bus.Subscribe("before:tool.result", p.handleBeforeToolResult, engine.WithPriority(5)),
	)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, bus
}

func TestToolTimeout_NormalCompletion_NoInterference(t *testing.T) {
	_, bus := newTestPlugin(t, 200*time.Millisecond, nil)

	var (
		mu      sync.Mutex
		timeout int
	)
	bus.Subscribe("tool.timeout", func(_ engine.Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		timeout++
	})

	// Tool fires invoke then result well before the deadline.
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c1", Name: "fast"})
	time.Sleep(20 * time.Millisecond)
	veto, _ := bus.EmitVetoable("before:tool.result", &events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "c1", Name: "fast"})
	if veto.Vetoed {
		t.Fatalf("normal result must not be vetoed, got %q", veto.Reason)
	}
	_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "c1", Name: "fast"})

	// Wait past the original deadline; no timeout should fire.
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if timeout != 0 {
		t.Fatalf("expected 0 tool.timeout events, got %d", timeout)
	}
}

func TestToolTimeout_DefaultTimeoutFires(t *testing.T) {
	_, bus := newTestPlugin(t, 50*time.Millisecond, nil)

	var (
		mu        sync.Mutex
		timeouts  []events.ToolTimeout
		results   []events.ToolResult
		seenReady = make(chan struct{}, 1)
	)
	bus.Subscribe("tool.timeout", func(e engine.Event[any]) {
		if tt, ok := e.Payload.(events.ToolTimeout); ok {
			mu.Lock()
			timeouts = append(timeouts, tt)
			mu.Unlock()
		}
	})
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
			select {
			case seenReady <- struct{}{}:
			default:
			}
		}
	})

	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "hang", Name: "stuck", TurnID: "turn-1"})

	select {
	case <-seenReady:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for synthesized result")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(timeouts) != 1 {
		t.Fatalf("expected 1 tool.timeout event, got %d", len(timeouts))
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 tool.result event, got %d", len(results))
	}
	tt := timeouts[0]
	if tt.ToolName != "stuck" || tt.CallID != "hang" {
		t.Fatalf("unexpected ToolTimeout %+v", tt)
	}
	if tt.Override != "gates.tool_timeout.per_tool.stuck" {
		t.Fatalf("unexpected override hint: %q", tt.Override)
	}
	if tt.Timeout != 50*time.Millisecond {
		t.Fatalf("unexpected timeout duration: %s", tt.Timeout)
	}
	if tt.TurnID != "turn-1" {
		t.Fatalf("turn id not propagated: %q", tt.TurnID)
	}

	// Synthesized result error must include the override hint verbatim.
	want := "tool stuck exceeded timeout 50ms; raise via gates.tool_timeout.per_tool.stuck: <duration>"
	if results[0].Error != want {
		t.Fatalf("error mismatch:\n got: %q\nwant: %q", results[0].Error, want)
	}
}

func TestToolTimeout_PerToolOverride_Longer(t *testing.T) {
	per := map[string]time.Duration{"slow": 200 * time.Millisecond}
	_, bus := newTestPlugin(t, 30*time.Millisecond, per)

	var fired int
	var mu sync.Mutex
	bus.Subscribe("tool.timeout", func(_ engine.Event[any]) {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c1", Name: "slow"})

	// Past the default but inside the override.
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	got := fired
	mu.Unlock()
	if got != 0 {
		t.Fatalf("override should have suppressed default-timer fire, saw %d", got)
	}

	// Provide a real result before the override expires.
	_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "c1", Name: "slow"})

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if fired != 0 {
		t.Fatalf("expected no timeout when result arrived in override window, got %d", fired)
	}
}

func TestToolTimeout_PerToolOverride_Shorter(t *testing.T) {
	per := map[string]time.Duration{"flaky": 30 * time.Millisecond}
	_, bus := newTestPlugin(t, 5*time.Second, per)

	done := make(chan events.ToolTimeout, 1)
	bus.Subscribe("tool.timeout", func(e engine.Event[any]) {
		if tt, ok := e.Payload.(events.ToolTimeout); ok {
			select {
			case done <- tt:
			default:
			}
		}
	})

	start := time.Now()
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c1", Name: "flaky"})

	select {
	case tt := <-done:
		elapsed := time.Since(start)
		if elapsed > 250*time.Millisecond {
			t.Fatalf("override fired too late: %s", elapsed)
		}
		if tt.Timeout != 30*time.Millisecond {
			t.Fatalf("expected 30ms timeout, got %s", tt.Timeout)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("override timeout did not fire")
	}
}

func TestToolTimeout_GlobMatch(t *testing.T) {
	per := map[string]time.Duration{"mcp.*": 30 * time.Millisecond}
	p, _ := newTestPlugin(t, 5*time.Second, per)

	got := p.resolveTimeout("mcp.search")
	if got != 30*time.Millisecond {
		t.Fatalf("glob mcp.* should match mcp.search, got %s", got)
	}

	got = p.resolveTimeout("other.tool")
	if got != 5*time.Second {
		t.Fatalf("non-matching name should fall back to default, got %s", got)
	}
}

func TestToolTimeout_ExactBeatsGlob(t *testing.T) {
	per := map[string]time.Duration{
		"mcp.*":      30 * time.Millisecond,
		"mcp.search": 90 * time.Millisecond,
	}
	p, _ := newTestPlugin(t, 5*time.Second, per)

	got := p.resolveTimeout("mcp.search")
	if got != 90*time.Millisecond {
		t.Fatalf("exact key must beat glob, got %s", got)
	}
	got = p.resolveTimeout("mcp.fetch")
	if got != 30*time.Millisecond {
		t.Fatalf("glob should match mcp.fetch, got %s", got)
	}
}

func TestToolTimeout_LongestGlobWins(t *testing.T) {
	per := map[string]time.Duration{
		"mcp.*":     30 * time.Millisecond,
		"mcp.sub.*": 90 * time.Millisecond,
	}
	p, _ := newTestPlugin(t, 5*time.Second, per)

	got := p.resolveTimeout("mcp.sub.alpha")
	if got != 90*time.Millisecond {
		t.Fatalf("longest glob (mcp.sub.*) should win, got %s", got)
	}
}

func TestToolTimeout_ConcurrentCallsIndependent(t *testing.T) {
	per := map[string]time.Duration{"slow": 200 * time.Millisecond}
	_, bus := newTestPlugin(t, 50*time.Millisecond, per)

	var (
		mu       sync.Mutex
		results  = map[string]events.ToolResult{}
		timeouts = map[string]events.ToolTimeout{}
		ready    = make(chan struct{}, 4)
	)
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			mu.Lock()
			results[r.ID] = r
			mu.Unlock()
			ready <- struct{}{}
		}
	})
	bus.Subscribe("tool.timeout", func(e engine.Event[any]) {
		if tt, ok := e.Payload.(events.ToolTimeout); ok {
			mu.Lock()
			timeouts[tt.CallID] = tt
			mu.Unlock()
		}
	})

	// Fire two quick + one defaulting + one with override; each gets its
	// own timer.
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "fast1", Name: "fast"})
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "fast2", Name: "fast"})
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "hang", Name: "hang"})
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "slow", Name: "slow"})

	// Quick ones complete immediately.
	_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "fast1", Name: "fast"})
	_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "fast2", Name: "fast"})

	// Wait for default-timeout firing of "hang".
	deadline := time.After(500 * time.Millisecond)
	for {
		mu.Lock()
		_, hangFired := timeouts["hang"]
		mu.Unlock()
		if hangFired {
			break
		}
		select {
		case <-deadline:
			t.Fatal("hang call did not time out")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Slow tool should still be in-flight (override is 200ms; we've only
	// passed ~50ms so far). Provide its real result before the override.
	_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "slow", Name: "slow"})

	// Drain remaining ready signals.
	for i := 0; i < 4; i++ {
		select {
		case <-ready:
		case <-time.After(300 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if _, ok := timeouts["hang"]; !ok {
		t.Fatal("hang must have timed out")
	}
	if _, ok := timeouts["fast1"]; ok {
		t.Fatal("fast1 must not time out")
	}
	if _, ok := timeouts["slow"]; ok {
		t.Fatal("slow must not time out (real result arrived first)")
	}
}

func TestToolTimeout_LateResultSuppressed(t *testing.T) {
	_, bus := newTestPlugin(t, 30*time.Millisecond, nil)

	timeoutCh := make(chan struct{}, 1)
	bus.Subscribe("tool.timeout", func(_ engine.Event[any]) {
		select {
		case timeoutCh <- struct{}{}:
		default:
		}
	})

	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c1", Name: "stuck"})

	select {
	case <-timeoutCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout did not fire")
	}

	// Late real result. before:tool.result must veto it.
	veto, err := bus.EmitVetoable("before:tool.result", &events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "c1", Name: "stuck"})
	if err != nil {
		t.Fatalf("emit veto: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected late tool.result to be vetoed")
	}
	if !strings.Contains(veto.Reason, "already timed out") {
		t.Fatalf("unexpected veto reason: %q", veto.Reason)
	}
}

func TestToolTimeout_ErrorMessageContainsExactHint(t *testing.T) {
	_, bus := newTestPlugin(t, 25*time.Millisecond, nil)

	got := make(chan events.ToolResult, 1)
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			select {
			case got <- r:
			default:
			}
		}
	})

	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "x1", Name: "web_fetch"})

	select {
	case r := <-got:
		want := "tool web_fetch exceeded timeout 25ms; raise via gates.tool_timeout.per_tool.web_fetch: <duration>"
		if r.Error != want {
			t.Fatalf("error message mismatch:\n got: %q\nwant: %q", r.Error, want)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("did not receive synthesized result")
	}
}
