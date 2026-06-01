package hitl

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestHandleCancelEmitsSyntheticResponse exercises the upstream hitl.cancel
// path. A request is persisted via the registry, hitl.cancel fires, and the
// plugin should remove the request file and emit a synthetic
// hitl.responded{Cancelled: true}.
func TestHandleCancelEmitsSyntheticResponse(t *testing.T) {
	dir := t.TempDir()
	bus := engine.NewEventBus()
	defer func() { _ = bus.Drain(context.TODO()) }()

	logger := newTestLogger()
	reg, err := newRegistry(dir, logger, bus)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	defer reg.Close()

	p := &Plugin{bus: bus, logger: logger, reg: reg, pending: map[string]chan events.HITLResponse{}}

	// Persist a request file so we can prove handleCancel deletes it.
	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion,
		ID: "hitl-cancel-1", Prompt: "x", ActionKind: "icm.stage.start"}
	if err := reg.persistRequest(req); err != nil {
		t.Fatalf("persistRequest: %v", err)
	}
	reqPath := filepath.Join(dir, req.ID+requestSuffix)
	if !fileExists(reqPath) {
		t.Fatalf("request file %s missing after persist", reqPath)
	}

	// Subscribe to hitl.responded to capture the synthetic response.
	var mu sync.Mutex
	var seen events.HITLResponse
	gotCh := make(chan struct{}, 1)
	bus.Subscribe("hitl.responded", func(ev engine.Event[any]) {
		mu.Lock()
		seen, _ = ev.Payload.(events.HITLResponse)
		mu.Unlock()
		select {
		case gotCh <- struct{}{}:
		default:
		}
	})

	// Fire the cancel.
	p.handleCancel(engine.Event[any]{Payload: events.HITLCancel{
		SchemaVersion: events.HITLCancelVersion, RequestID: req.ID, Reason: "test cancel",
	}})

	select {
	case <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for synthetic hitl.responded")
	}

	mu.Lock()
	defer mu.Unlock()
	if seen.RequestID != req.ID {
		t.Fatalf("RequestID = %q; want %q", seen.RequestID, req.ID)
	}
	if !seen.Cancelled {
		t.Fatal("Cancelled = false; want true")
	}
	if seen.CancelReason != "test cancel" {
		t.Fatalf("CancelReason = %q; want %q", seen.CancelReason, "test cancel")
	}

	// Give the registry fsnotify watcher a beat to settle (cancel removes
	// the request file synchronously; this is belt-and-suspenders).
	time.Sleep(50 * time.Millisecond)
	if fileExists(reqPath) {
		t.Fatalf("request file %s still present after cancel", reqPath)
	}
}

// TestHandleCancelAcceptsMapPayload verifies the permissive map[string]any
// fallback path. Some emitters (config-driven bridges, scripted IO) emit
// payloads as untyped maps.
func TestHandleCancelAcceptsMapPayload(t *testing.T) {
	bus := engine.NewEventBus()
	defer func() { _ = bus.Drain(context.TODO()) }()
	p := &Plugin{bus: bus, logger: newTestLogger(), pending: map[string]chan events.HITLResponse{}}

	gotCh := make(chan events.HITLResponse, 1)
	bus.Subscribe("hitl.responded", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLResponse); ok {
			select {
			case gotCh <- r:
			default:
			}
		}
	})

	p.handleCancel(engine.Event[any]{Payload: map[string]any{
		"request_id": "hitl-map-1",
		"reason":     "via map",
	}})

	select {
	case got := <-gotCh:
		if got.RequestID != "hitl-map-1" {
			t.Fatalf("RequestID = %q", got.RequestID)
		}
		if !got.Cancelled || got.CancelReason != "via map" {
			t.Fatalf("response = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

// TestHandleCancelIgnoresEmptyID is a quick safety check.
func TestHandleCancelIgnoresEmptyID(t *testing.T) {
	bus := engine.NewEventBus()
	defer func() { _ = bus.Drain(context.TODO()) }()
	p := &Plugin{bus: bus, logger: newTestLogger(), pending: map[string]chan events.HITLResponse{}}

	gotCh := make(chan struct{}, 1)
	bus.Subscribe("hitl.responded", func(ev engine.Event[any]) {
		select {
		case gotCh <- struct{}{}:
		default:
		}
	})

	p.handleCancel(engine.Event[any]{Payload: events.HITLCancel{
		SchemaVersion: events.HITLCancelVersion,
	}})

	select {
	case <-gotCh:
		t.Fatal("emitted hitl.responded for empty RequestID")
	case <-time.After(150 * time.Millisecond):
		// expected: nothing emitted
	}
}
