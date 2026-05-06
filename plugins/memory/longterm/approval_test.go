package longterm

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin returns a Plugin wired to a fresh bus, a tmp write path,
// and the supplied require_approval block applied via parseApprovalConfig.
func newTestPlugin(t *testing.T, approvalCfg map[string]any) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.writePath = t.TempDir()
	p.paths = []string{p.writePath}
	if approvalCfg != nil {
		p.parseApprovalConfig(approvalCfg)
	}
	return p, bus
}

// autoRespond subscribes to hitl.requested and replies with the supplied
// response shape.
func autoRespond(bus engine.EventBus, choiceID string, cancelled bool) {
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.HITLRequest)
		if !ok {
			return
		}
		go func() {
			_ = bus.Emit("hitl.responded", events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: req.ID,
				ChoiceID:  choiceID,
				Cancelled: cancelled,
			})
		}()
	}, engine.WithPriority(10))
}

func TestApprovalDisabled_NoGate(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	if err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "test-key",
		Content: "hello world",
	}); err != nil {
		t.Fatalf("doStore: %v", err)
	}
	if requested {
		t.Error("hitl.requested should not fire when require_approval is disabled")
	}
}

func TestApprovalEnabled_AllowProceeds(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"timeout": "1s",
	})
	autoRespond(bus, "allow", false)

	if err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "k1",
		Content: "approved content",
	}); err != nil {
		t.Fatalf("doStore allowed should succeed: %v", err)
	}

	// File should exist.
	matches, _ := filepath.Glob(filepath.Join(p.writePath, "*.md"))
	if len(matches) == 0 {
		t.Error("expected memory file written after allow")
	}
}

func TestApprovalEnabled_RejectAborts(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"timeout": "1s",
	})
	autoRespond(bus, "reject", false)

	err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "k2",
		Content: "should not persist",
	})
	if err == nil {
		t.Fatal("expected error from rejected write")
	}

	matches, _ := filepath.Glob(filepath.Join(p.writePath, "*.md"))
	if len(matches) != 0 {
		t.Errorf("no files should be written after reject, got %d", len(matches))
	}
}

func TestApprovalKeyGlob_NonMatchingKeySkipsGate(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"match": map[string]any{
			"key_glob": "secret-*",
		},
	})
	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	// "ordinary-key" does not match "secret-*" — write should proceed.
	if err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "ordinary-key",
		Content: "harmless",
	}); err != nil {
		t.Fatalf("doStore: %v", err)
	}
	if requested {
		t.Error("non-matching key should bypass approval")
	}
}

func TestApprovalKeyGlob_MatchingKeyTriggersGate(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"match": map[string]any{
			"key_glob": "secret-*",
		},
		"timeout": "1s",
	})
	autoRespond(bus, "reject", false)

	err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "secret-credentials",
		Content: "high stakes",
	})
	if err == nil {
		t.Fatal("expected matching key to trigger approval and reject")
	}
}

func TestApprovalSizeThreshold_SmallSkips(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"match": map[string]any{
			"size_threshold_bytes": 100,
		},
	})
	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	if err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "small",
		Content: "tiny note",
	}); err != nil {
		t.Fatalf("doStore: %v", err)
	}
	if requested {
		t.Error("small write should bypass approval when size_threshold > content len")
	}
}

func TestApprovalSizeThreshold_LargeTriggers(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"match": map[string]any{
			"size_threshold_bytes": 10,
		},
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

	if err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "big",
		Content: "this is more than ten bytes",
	}); err != nil {
		t.Fatalf("doStore: %v", err)
	}
	if captured.ActionKind != "memory.longterm.write" {
		t.Errorf("expected ActionKind=memory.longterm.write, got %q", captured.ActionKind)
	}
	if captured.ActionRef["key"] != "big" {
		t.Errorf("ActionRef[key] = %v, want 'big'", captured.ActionRef["key"])
	}
}

func TestApprovalContentTruncatedInActionRef(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"enabled": true,
		"timeout": "1s",
	})

	// Build content well over 2000 bytes.
	big := make([]byte, 5000)
	for i := range big {
		big[i] = 'a'
	}

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

	if err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "big-blob",
		Content: string(big),
	}); err != nil {
		t.Fatalf("doStore: %v", err)
	}
	if captured.ActionRef["_truncated"] != true {
		t.Error("expected _truncated marker on oversized content")
	}
	preview, _ := captured.ActionRef["content"].(string)
	if len(preview) > 2000 {
		t.Errorf("content preview should be capped at 2000 chars, got %d", len(preview))
	}
	if size, _ := captured.ActionRef["size"].(int); size != 5000 {
		t.Errorf("size field should report full length 5000, got %d", size)
	}
}

func TestApprovalDefaultChoice_RejectOnTimeout(t *testing.T) {
	p, _ := newTestPlugin(t, map[string]any{
		"enabled":        true,
		"default_choice": "reject",
		"timeout":        "30ms",
	})
	// No responder.

	start := time.Now()
	err := p.doStore(events.LongTermMemoryStoreRequest{SchemaVersion: events.LongTermMemoryStoreRequestVersion, Key: "k",
		Content: "x",
	})
	if err == nil {
		t.Fatal("expected timeout to abort write when default_choice=reject")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("returned too late: %v", elapsed)
	}
}
