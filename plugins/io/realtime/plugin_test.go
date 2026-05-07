package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin wires a Plugin against a real engine.EventBus and a
// httptest.Server hosting the plugin's handler. It returns the plugin,
// the bus, the websocket dial URL, and a teardown that shuts everything
// down. Tests that need multiple steps share one teardown via t.Cleanup.
func newTestPlugin(t *testing.T) (*Plugin, engine.EventBus, string) {
	t.Helper()

	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := New().(*Plugin)
	p.bus = bus
	p.logger = logger
	p.listenAddr = ":0"
	p.path = "/ws"
	p.maxClients = 4
	p.server = NewServer(logger, p.listenAddr, p.path, p.maxClients, p.handleInbound)

	// Mirror Init's bus subscriptions. We do not call Init() because that
	// requires a full engine.PluginContext; this approximation keeps the
	// test focused on bus<->client wiring.
	p.unsubs = append(p.unsubs,
		bus.Subscribe("llm.stream.chunk", p.handleStreamChunk),
		bus.Subscribe("llm.stream.end", p.handleStreamEnd),
		bus.Subscribe("before:tool.invoke", p.handleBeforeToolInvoke,
			engine.WithPriority(100)),
		bus.Subscribe("voice.audio.output.chunk", p.handleVoiceAudioOutput),
		bus.Subscribe("cancel.complete", p.handleCancelComplete),
		bus.Subscribe("hitl.requested", p.handleHITLRequest),
	)

	srv := httptest.NewServer(p.server.Handler())
	// httptest gives us http://...; swap to ws://.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	t.Cleanup(func() {
		for _, unsub := range p.unsubs {
			unsub()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.server.Shutdown(shutdownCtx)
		srv.Close()
	})

	return p, bus, wsURL
}

// dialAndWait dials the websocket and waits until the server has
// registered the connection. Polling on ClientCount avoids a race where
// the test broadcasts before the read pump's accept finishes.
func dialAndWait(t *testing.T, ctx context.Context, p *Plugin, wsURL string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.server.ClientCount() >= 1 {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server never registered client")
	return conn
}

func TestPlugin_ConnectAndStreamDelta(t *testing.T) {
	p, bus, wsURL := newTestPlugin(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn := dialAndWait(t, ctx, p, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Emit a synthesized stream chunk and assert the client reads
	// stream.delta with the matching turn id and content.
	if err := bus.Emit("llm.stream.chunk", events.StreamChunk{
		SchemaVersion: events.StreamChunkVersion,
		Content:       "hello",
		TurnID:        "t-1",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	var got envelope
	if err := wsjson.Read(ctx, conn, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Type != "stream.delta" {
		t.Fatalf("expected stream.delta; got %s", got.Type)
	}
	if got.TurnID != "t-1" {
		t.Fatalf("expected turn_id=t-1; got %q", got.TurnID)
	}
	if got.Content != "hello" {
		t.Fatalf("expected content=hello; got %q", got.Content)
	}
}

func TestPlugin_InboundInputEmitsUserInput(t *testing.T) {
	p, bus, wsURL := newTestPlugin(t)

	// Subscribe before the connection is live so we don't miss the emit.
	var (
		mu   sync.Mutex
		seen events.UserInput
		got  atomic.Bool
	)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		ui, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		mu.Lock()
		seen = ui
		mu.Unlock()
		got.Store(true)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn := dialAndWait(t, ctx, p, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Send the input envelope.
	frame := envelope{Type: "input", Content: "hi"}
	data, _ := json.Marshal(frame)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	// io.input is dispatched from a goroutine on the read pump; poll.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !got.Load() {
		t.Fatalf("io.input never emitted")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen.Content != "hi" {
		t.Fatalf("expected content=hi; got %q", seen.Content)
	}
	if seen.SchemaVersion != events.UserInputVersion {
		t.Fatalf("expected schema_version=%d; got %d", events.UserInputVersion, seen.SchemaVersion)
	}
}

func TestPlugin_InboundCancelEmitsCancelRequest(t *testing.T) {
	p, bus, wsURL := newTestPlugin(t)

	var (
		mu   sync.Mutex
		seen events.CancelRequest
		got  atomic.Bool
	)
	bus.Subscribe("cancel.request", func(e engine.Event[any]) {
		cr, ok := e.Payload.(events.CancelRequest)
		if !ok {
			return
		}
		mu.Lock()
		seen = cr
		mu.Unlock()
		got.Store(true)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn := dialAndWait(t, ctx, p, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "")

	frame := envelope{Type: "cancel"}
	data, _ := json.Marshal(frame)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !got.Load() {
		t.Fatalf("cancel.request never emitted")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen.Source != "realtime" {
		t.Fatalf("expected source=realtime; got %q", seen.Source)
	}
}
