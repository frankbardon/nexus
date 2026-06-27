package client

// These tests exercise the inprocess transport end-to-end: a trivial
// official-SDK *mcp.Server exposing one tool AND one resource is handed to the
// plugin via RegisterInProcessServer, the plugin connects over the SDK's
// in-memory transport (no subprocess), and the existing tool/resource bridge
// surfaces both to the agent through tool.register / tool.invoke / tool.result
// — mirroring the in-memory server+client pattern in lattice
// mcp/gosdk/gosdk_test.go. The SDK lives only in this _test file and the
// plugin's transport layer, never leaking into the engine core.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const inprocessKey = "test-host-server"

// echoInput is the typed argument for the trivial echo tool the test server
// exposes; the SDK derives its JSON schema from this struct.
type echoInput struct {
	Text string `json:"text"`
}

// newTestInProcessServer builds a bare official-SDK server exposing exactly one
// tool (echo) and one static resource (test://greeting), then registers it
// under inprocessKey so the inprocess transport can find it. Registration is
// torn down via t.Cleanup so it never leaks into another test.
func newTestInProcessServer(t *testing.T) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-host", Version: "v0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "Echo the input text back.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}},
		}, nil, nil
	})

	srv.AddResource(&mcp.Resource{
		URI:      "test://greeting",
		Name:     "greeting",
		MIMEType: "text/plain",
	}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      "test://greeting",
				MIMEType: "text/plain",
				Text:     "hello from the host",
			}},
		}, nil
	})

	RegisterInProcessServer(inprocessKey, srv)
	t.Cleanup(func() { UnregisterInProcessServer(inprocessKey) })
}

// busRecorder collects tool.register and tool.result events emitted by the
// bridge so the test can assert what the agent would see.
type busRecorder struct {
	mu      sync.Mutex
	tools   map[string]events.ToolDef
	results []events.ToolResult
}

func newBusRecorder(bus engine.EventBus) *busRecorder {
	r := &busRecorder{tools: map[string]events.ToolDef{}}
	bus.Subscribe("tool.register", func(ev engine.Event[any]) {
		if def, ok := ev.Payload.(events.ToolDef); ok {
			r.mu.Lock()
			r.tools[def.Name] = def
			r.mu.Unlock()
		}
	}, engine.WithPriority(100))
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if res, ok := ev.Payload.(events.ToolResult); ok {
			r.mu.Lock()
			r.results = append(r.results, res)
			r.mu.Unlock()
		}
	}, engine.WithPriority(100))
	return r
}

func (r *busRecorder) toolNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	return out
}

func (r *busRecorder) hasTool(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tools[name]
	return ok
}

func (r *busRecorder) lastResult() (events.ToolResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return events.ToolResult{}, false
	}
	return r.results[len(r.results)-1], true
}

// newInProcessPlugin builds the plugin bound to one inprocess server and
// connects it via Ready (engine lifecycle), exactly as the engine would at
// boot. Returns the live plugin, the bus recorder, and a cleanup func.
func newInProcessPlugin(t *testing.T) (*Plugin, *busRecorder) {
	t.Helper()

	bus := engine.NewEventBus()
	recorder := newBusRecorder(bus)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := New().(*Plugin)
	ctx := engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{
			"servers": []any{
				map[string]any{
					"name":      "host",
					"transport": "inprocess",
					"server":    inprocessKey,
					"lifecycle": "engine",
				},
			},
		},
	}
	if err := p.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, recorder
}

// TestInProcess_BridgesToolAndResource is the headline acceptance check: the
// agent sees the host server's tool AND resource over the in-memory transport,
// with no subprocess.
func TestInProcess_BridgesToolAndResource(t *testing.T) {
	newTestInProcessServer(t)
	_, recorder := newInProcessPlugin(t)

	// Tool: the echo tool surfaces under the mcp__<server>__<raw> namespace.
	const echoTool = "mcp__host__echo"
	if !recorder.hasTool(echoTool) {
		t.Fatalf("expected tool %q to be registered; got %v", echoTool, recorder.toolNames())
	}

	// Resource: the bridge always registers the generic read tool, plus an
	// auto-registered static-resource tool for test://greeting.
	const readResource = "mcp__host__read_resource"
	if !recorder.hasTool(readResource) {
		t.Fatalf("expected generic resource read tool %q; got %v", readResource, recorder.toolNames())
	}
	var staticResource string
	for _, name := range recorder.toolNames() {
		if len(name) > len("mcp__host__resource__") && name[:len("mcp__host__resource__")] == "mcp__host__resource__" {
			staticResource = name
		}
	}
	if staticResource == "" {
		t.Fatalf("expected an auto-registered static resource tool; got %v", recorder.toolNames())
	}
}

// TestInProcess_InvokeToolRoutesToServer drives tool.invoke -> CallTool ->
// tool.result through the bridge over the in-memory transport.
func TestInProcess_InvokeToolRoutesToServer(t *testing.T) {
	newTestInProcessServer(t)
	p, recorder := newInProcessPlugin(t)

	p.handleToolInvoke(engine.Event[any]{
		Type: "tool.invoke",
		Payload: events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "call-1",
			Name:          "mcp__host__echo",
			Arguments:     map[string]any{"text": "ping"},
		},
	})

	res, ok := recorder.lastResult()
	if !ok {
		t.Fatal("no tool.result emitted for echo invoke")
	}
	if res.Error != "" {
		t.Fatalf("echo invoke returned error: %q", res.Error)
	}
	if res.Output != "echo: ping" {
		t.Fatalf("echo output = %q, want %q", res.Output, "echo: ping")
	}
}

// TestInProcess_ReadResourceRoutesToServer drives the generic read_resource
// tool -> ReadResource -> tool.result over the in-memory transport.
func TestInProcess_ReadResourceRoutesToServer(t *testing.T) {
	newTestInProcessServer(t)
	p, recorder := newInProcessPlugin(t)

	p.handleToolInvoke(engine.Event[any]{
		Type: "tool.invoke",
		Payload: events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "call-2",
			Name:          "mcp__host__read_resource",
			Arguments:     map[string]any{"uri": "test://greeting"},
		},
	})

	res, ok := recorder.lastResult()
	if !ok {
		t.Fatal("no tool.result emitted for read_resource invoke")
	}
	if res.Error != "" {
		t.Fatalf("read_resource returned error: %q", res.Error)
	}
	if res.Output != "hello from the host" {
		t.Fatalf("read_resource output = %q, want %q", res.Output, "hello from the host")
	}
}

// TestInProcess_MissingServerKeyFailsConnect asserts a config referencing an
// unregistered server key fails to connect cleanly (no panic, server left
// disconnected) rather than silently appearing healthy.
func TestInProcess_MissingServerKeyFailsConnect(t *testing.T) {
	// Deliberately do NOT register a server under this key.
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{
			"servers": []any{
				map[string]any{
					"name":      "host",
					"transport": "inprocess",
					"server":    "no-such-key",
					"lifecycle": "engine",
				},
			},
		},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// Ready does not surface per-server connect failures (a broken server must
	// not block boot); assert the server ended up disconnected instead.
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	s := p.servers["host"]
	if s == nil {
		t.Fatal("server not constructed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.connect(ctx); err == nil {
		t.Fatal("expected connect to fail for an unregistered inprocess server key")
	}
}
