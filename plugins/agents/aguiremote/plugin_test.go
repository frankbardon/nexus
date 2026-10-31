package aguiremote

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func baseConfig(endpoint string) map[string]any {
	return map[string]any{
		"agents": []any{
			map[string]any{
				"name":     "researcher",
				"endpoint": endpoint,
			},
		},
	}
}

func TestContract_DeclaredSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(baseConfig("http://127.0.0.1:1/agui")))
	h.AssertSubscribesTo("tool.invoke")
}

func TestContract_DeclaredEmissions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(baseConfig("http://127.0.0.1:1/agui")))
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"tool.register", "tool.result", "before:tool.result",
		"io.output", "subagent.started", "subagent.complete",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}

func TestReady_RegistersToolPerAgent(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(baseConfig("http://127.0.0.1:1/agui")))
	var found bool
	for _, ce := range h.PluginEmissions() {
		if ce.Type != "tool.register" {
			continue
		}
		td, ok := ce.Payload.(events.ToolDef)
		if ok && td.Name == "delegate_agui_researcher" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tool.register for delegate_agui_researcher")
	}
}

// sseServer streams the given AG-UI events as an SSE response.
func sseServer(t *testing.T, evs ...agui.Event) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agui.WriteHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		sw := agui.NewSSEWriter(w)
		for _, e := range evs {
			if err := sw.Write(e); err != nil {
				return
			}
		}
	}))
}

// runInvoke boots the plugin against endpoint, invokes its tool, and returns the
// terminal tool.result.
func runInvoke(t *testing.T, cfg map[string]any) events.ToolResult {
	t.Helper()
	bus := engine.NewEventBus()
	p := New()
	if err := p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    bus,
		Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	var (
		mu     sync.Mutex
		result events.ToolResult
		got    bool
	)
	done := make(chan struct{})
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		tr, ok := ev.Payload.(events.ToolResult)
		if !ok {
			return
		}
		mu.Lock()
		if !got {
			result = tr
			got = true
			close(done)
		}
		mu.Unlock()
	})

	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "delegate_agui_researcher",
		Arguments:     map[string]any{"task": "summarize the docs"},
		TurnID:        "turn-1",
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tool.result")
	}
	return result
}

func TestDelegate_RemoteTextResult(t *testing.T) {
	srv := sseServer(t,
		agui.NewRunStarted("t", "r"),
		agui.NewTextMessageStart("m1", "assistant"),
		agui.NewTextMessageContent("m1", "The docs cover "),
		agui.NewTextMessageContent("m1", "everything."),
		agui.NewTextMessageEnd("m1"),
		agui.NewRunFinished("t", "r"),
	)
	defer srv.Close()

	res := runInvoke(t, baseConfig(srv.URL))
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if res.Output != "The docs cover everything." {
		t.Fatalf("unexpected output: %q", res.Output)
	}
}

func TestDelegate_RemoteRunError(t *testing.T) {
	srv := sseServer(t,
		agui.NewRunStarted("t", "r"),
		agui.NewRunError("upstream model unavailable"),
	)
	defer srv.Close()

	res := runInvoke(t, baseConfig(srv.URL))
	if res.Error == "" {
		t.Fatal("expected a clean delegate error, got none")
	}
	if want := "upstream model unavailable"; !strings.Contains(res.Error, want) {
		t.Fatalf("error %q should mention %q", res.Error, want)
	}
}

func TestDelegate_AuthRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := runInvoke(t, baseConfig(srv.URL))
	if res.Error == "" {
		t.Fatal("expected a clean error on 401 rejection")
	}
	if !strings.Contains(res.Error, "401") {
		t.Fatalf("error %q should mention HTTP 401", res.Error)
	}
}

func TestDelegate_TransportError(t *testing.T) {
	// Point at a closed port to force a dial failure.
	res := runInvoke(t, baseConfig("http://127.0.0.1:1/agui"))
	if res.Error == "" {
		t.Fatal("expected a clean transport error")
	}
	if !strings.Contains(res.Error, "transport error") {
		t.Fatalf("error %q should mention transport error", res.Error)
	}
}

func TestDelegate_MissingTask(t *testing.T) {
	bus := engine.NewEventBus()
	p := New()
	if err := p.Init(engine.PluginContext{
		Config: baseConfig("http://127.0.0.1:1/agui"),
		Bus:    bus,
		Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = p.Ready()

	done := make(chan events.ToolResult, 1)
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if tr, ok := ev.Payload.(events.ToolResult); ok {
			select {
			case done <- tr:
			default:
			}
		}
	})
	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "delegate_agui_researcher",
		Arguments:     map[string]any{},
		TurnID:        "turn-1",
	})
	select {
	case res := <-done:
		if res.Error == "" {
			t.Fatal("expected task-required error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

// TestDelegate_NestedLoopEventsCarryBindingTurn pins #224: invoked from a
// subagent/delegate loop the call carries a private TurnID plus ParentTurnID;
// subagent.started/iteration/complete must carry the binding (parent) turn,
// consistent with subagent/delegate, while tool.result keeps the private
// TurnID for result matching.
func TestDelegate_NestedLoopEventsCarryBindingTurn(t *testing.T) {
	srv := sseServer(t,
		agui.NewRunStarted("t", "r"),
		agui.NewTextMessageStart("m1", "assistant"),
		agui.NewTextMessageContent("m1", "done"),
		agui.NewTextMessageEnd("m1"),
		agui.NewRunFinished("t", "r"),
	)
	defer srv.Close()

	bus := engine.NewEventBus()
	p := New()
	if err := p.Init(engine.PluginContext{
		Config: baseConfig(srv.URL),
		Bus:    bus,
		Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	var (
		mu     sync.Mutex
		turns  = map[string][]string{}
		result events.ToolResult
	)
	done := make(chan struct{})
	bus.Subscribe("subagent.started", func(ev engine.Event[any]) {
		if e, ok := ev.Payload.(events.SubagentStarted); ok {
			mu.Lock()
			turns["started"] = append(turns["started"], e.ParentTurnID)
			mu.Unlock()
		}
	})
	bus.Subscribe("subagent.iteration", func(ev engine.Event[any]) {
		if e, ok := ev.Payload.(events.SubagentIteration); ok {
			mu.Lock()
			turns["iteration"] = append(turns["iteration"], e.ParentTurnID)
			mu.Unlock()
		}
	})
	bus.Subscribe("subagent.complete", func(ev engine.Event[any]) {
		if e, ok := ev.Payload.(events.SubagentComplete); ok {
			mu.Lock()
			turns["complete"] = append(turns["complete"], e.ParentTurnID)
			mu.Unlock()
		}
	})
	var once sync.Once
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if tr, ok := ev.Payload.(events.ToolResult); ok {
			once.Do(func() {
				mu.Lock()
				result = tr
				mu.Unlock()
				close(done)
			})
		}
	})

	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "delegate_agui_researcher",
		Arguments:     map[string]any{"task": "summarize"},
		TurnID:        "subagent_xyz",
		ParentTurnID:  "turn-parent",
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tool.result")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, kind := range []string{"started", "iteration", "complete"} {
		got := turns[kind]
		if len(got) == 0 {
			t.Errorf("no subagent.%s observed", kind)
			continue
		}
		for _, tid := range got {
			if tid != "turn-parent" {
				t.Errorf("subagent.%s ParentTurnID = %q, want turn-parent", kind, tid)
			}
		}
	}
	if result.TurnID != "subagent_xyz" {
		t.Errorf("tool.result TurnID = %q, want subagent_xyz (result matching)", result.TurnID)
	}
}

func TestInit_RequiresAgents(t *testing.T) {
	p := New()
	err := p.Init(engine.PluginContext{
		Config: map[string]any{},
		Bus:    engine.NewEventBus(),
		Logger: discardLogger(),
	})
	if err == nil {
		t.Fatal("expected error when 'agents' is missing")
	}
}
