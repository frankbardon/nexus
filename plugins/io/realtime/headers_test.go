package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// A WebSocket has headers only at the upgrade, so every frame on a connection
// carries the upgrade's X-Nexus-* context — and nothing outside that namespace.
func TestPlugin_InboundInputCarriesUpgradeHeaders(t *testing.T) {
	p, bus, wsURL := newTestPlugin(t)

	var (
		mu   sync.Mutex
		seen events.UserInput
		got  atomic.Bool
	)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		in, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		mu.Lock()
		seen = in
		mu.Unlock()
		got.Store(true)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Nexus-Tenant-Id": []string{"acme"},
			"X-Trace-Id":        []string{"should-not-appear"},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClient(t, p)

	data, _ := json.Marshal(envelope{Type: "input", Content: "hi"})
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !got.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !got.Load() {
		t.Fatal("io.input never emitted")
	}

	mu.Lock()
	defer mu.Unlock()
	if seen.Headers["tenant-id"] != "acme" {
		t.Errorf("io.input Headers = %v, want tenant-id=acme", seen.Headers)
	}
	if len(seen.Headers) != 1 {
		t.Errorf("io.input Headers = %v, want nothing outside the X-Nexus-* namespace", seen.Headers)
	}
}

func waitForClient(t *testing.T, p *Plugin) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.server.ClientCount() >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server never registered client")
}
