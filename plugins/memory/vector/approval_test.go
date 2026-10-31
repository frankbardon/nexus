package vector

import (
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin returns a Plugin wired to a fresh bus, with a fixed
// namespace and a stub embeddings.provider so storeDoc can complete.
func newTestPlugin(t *testing.T, namespace string, approvalCfg map[string]any) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.namespace = namespace
	if approvalCfg != nil {
		p.parseApprovalConfig(approvalCfg)
	}

	// Stub embeddings provider — return a 4-dim zero vector.
	bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(*events.EmbeddingsRequest)
		if !ok {
			return
		}
		req.Vectors = [][]float32{{0.0, 0.0, 0.0, 0.0}}
	}, engine.WithPriority(50))

	return p, bus
}

func autoRespond(bus engine.EventBus, choiceID string) {
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.HITLRequest)
		if !ok {
			return
		}
		go func() {
			_ = bus.Emit("hitl.responded", events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: req.ID,
				ChoiceID: choiceID,
			})
		}()
	}, engine.WithPriority(10))
}

// captureUpserts collects all vector.upsert payloads.
func captureUpserts(bus engine.EventBus) *[]events.VectorUpsert {
	var got []events.VectorUpsert
	bus.Subscribe("vector.upsert", func(ev engine.Event[any]) {
		if up, ok := ev.Payload.(*events.VectorUpsert); ok {
			got = append(got, *up)
		}
	}, engine.WithPriority(100))
	return &got
}

func TestVectorApprovalDisabled_NoGate(t *testing.T) {
	p, bus := newTestPlugin(t, "test-ns", nil)
	upserts := captureUpserts(bus)

	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	if err := p.storeDoc("hello", "test", nil, nil); err != nil {
		t.Fatalf("storeDoc: %v", err)
	}
	if requested {
		t.Error("hitl.requested should not fire when require_approval is disabled")
	}
	if len(*upserts) != 1 {
		t.Errorf("expected 1 upsert, got %d", len(*upserts))
	}
}

func TestVectorApprovalAllowed_Persists(t *testing.T) {
	p, bus := newTestPlugin(t, "test-ns", map[string]any{
		"enabled": true,
		"timeout": "1s",
	})
	upserts := captureUpserts(bus)
	autoRespond(bus, "allow")

	if err := p.storeDoc("approved content", "test", nil, nil); err != nil {
		t.Fatalf("storeDoc: %v", err)
	}
	if len(*upserts) != 1 {
		t.Errorf("expected 1 upsert after allow, got %d", len(*upserts))
	}
}

func TestVectorApprovalRejected_Aborts(t *testing.T) {
	p, bus := newTestPlugin(t, "test-ns", map[string]any{
		"enabled": true,
		"timeout": "1s",
	})
	upserts := captureUpserts(bus)
	autoRespond(bus, "reject")

	err := p.storeDoc("rejected content", "test", nil, nil)
	if err == nil {
		t.Fatal("expected error when operator rejects")
	}
	if len(*upserts) != 0 {
		t.Errorf("no upsert should occur after reject, got %d", len(*upserts))
	}
}

func TestVectorApprovalNamespaceGlob_NoMatchSkipsGate(t *testing.T) {
	p, bus := newTestPlugin(t, "private-foo", map[string]any{
		"enabled": true,
		"match": map[string]any{
			"namespace_glob": "shared/*",
		},
	})
	upserts := captureUpserts(bus)
	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	if err := p.storeDoc("private write", "test", nil, nil); err != nil {
		t.Fatalf("storeDoc: %v", err)
	}
	if requested {
		t.Error("non-matching namespace should bypass approval")
	}
	if len(*upserts) != 1 {
		t.Errorf("expected 1 upsert, got %d", len(*upserts))
	}
}

func TestVectorApprovalNamespaceGlob_MatchTriggersGate(t *testing.T) {
	p, bus := newTestPlugin(t, "shared/team-a", map[string]any{
		"enabled": true,
		"match": map[string]any{
			"namespace_glob": "shared/*",
		},
		"timeout": "1s",
	})
	autoRespond(bus, "reject")

	err := p.storeDoc("shared write", "test", nil, nil)
	if err == nil {
		t.Fatal("expected matching namespace to trigger approval and reject")
	}
}

func TestVectorApprovalSizeThreshold_SmallSkips(t *testing.T) {
	p, bus := newTestPlugin(t, "ns", map[string]any{
		"enabled": true,
		"match": map[string]any{
			"size_threshold_bytes": 1000,
		},
	})
	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	if err := p.storeDoc("tiny", "test", nil, nil); err != nil {
		t.Fatalf("storeDoc: %v", err)
	}
	if requested {
		t.Error("small content should bypass approval")
	}
}

func TestVectorApprovalActionRefSurfacesNamespace(t *testing.T) {
	p, bus := newTestPlugin(t, "shared/team", map[string]any{
		"enabled": true,
		"timeout": "1s",
	})

	var captured events.HITLRequest
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.HITLRequest)
		if !ok {
			return
		}
		captured = req
		go func() {
			_ = bus.Emit("hitl.responded", events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: req.ID,
				ChoiceID: "allow",
			})
		}()
	}, engine.WithPriority(10))

	if err := p.storeDoc("payload", "compaction", map[string]string{"backup_path": "/tmp/x"}, nil); err != nil {
		t.Fatalf("storeDoc: %v", err)
	}
	if captured.ActionKind != "memory.vector.write" {
		t.Errorf("ActionKind = %q", captured.ActionKind)
	}
	if captured.ActionRef["namespace"] != "shared/team" {
		t.Errorf("namespace = %v", captured.ActionRef["namespace"])
	}
	if captured.ActionRef["source"] != "compaction" {
		t.Errorf("source = %v", captured.ActionRef["source"])
	}
}

func TestVectorApprovalTimeoutDefaultRejects(t *testing.T) {
	p, _ := newTestPlugin(t, "ns", map[string]any{
		"enabled":        true,
		"default_choice": "reject",
		"timeout":        "30ms",
	})
	// No responder.

	start := time.Now()
	if err := p.storeDoc("x", "test", nil, nil); err == nil {
		t.Fatal("expected timeout to reject the write")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("returned too late: %v", elapsed)
	}
}

// TestVectorApprovalTimeout_Default pins require_approval.timeout's 30m
// default, the "0 disables" convention, and that a malformed value keeps the
// default rather than meaning "wait forever".
func TestVectorApprovalTimeout_Default(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want time.Duration
	}{
		{"unset", map[string]any{"enabled": true}, 30 * time.Minute},
		{"malformed", map[string]any{"enabled": true, "timeout": "bogus"}, 30 * time.Minute},
		{"explicit", map[string]any{"enabled": true, "timeout": "5m"}, 5 * time.Minute},
		{"zero disables", map[string]any{"enabled": true, "timeout": "0"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPlugin(t, "ns", tc.cfg)
			if p.approvalTimeout != tc.want {
				t.Errorf("approvalTimeout = %v, want %v", p.approvalTimeout, tc.want)
			}
		})
	}
}

// TestVectorApprovalTimeout_ExpiryDeniesAndCancels: with no default_choice
// an expired approval skips the upsert and retracts the prompt with
// hitl.cancel for the same request.
func TestVectorApprovalTimeout_ExpiryDeniesAndCancels(t *testing.T) {
	p, bus := newTestPlugin(t, "ns", map[string]any{"enabled": true, "timeout": "20ms"})
	upserts := captureUpserts(bus)

	var reqID, cancelID string
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLRequest); ok {
			reqID = r.ID
		}
	})
	bus.Subscribe("hitl.cancel", func(ev engine.Event[any]) {
		if c, ok := ev.Payload.(events.HITLCancel); ok {
			cancelID = c.RequestID
		}
	})

	if err := p.storeDoc("x", "test", nil, nil); err == nil {
		t.Fatal("expected expiry to deny the write")
	}
	if len(*upserts) != 0 {
		t.Errorf("denied write upserted %d docs", len(*upserts))
	}
	if reqID == "" || cancelID != reqID {
		t.Errorf("hitl.cancel request_id = %q, want %q", cancelID, reqID)
	}
}
