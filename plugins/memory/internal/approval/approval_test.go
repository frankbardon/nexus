package approval

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

var testLogger = slog.Default()

// observeRequest hooks the bus to capture the next hitl.requested payload
// and reply with the supplied response after the request is captured.
// Returns a getter for the captured request.
func observeRequest(bus engine.EventBus, reply events.HITLResponse) func() events.HITLRequest {
	var got events.HITLRequest
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.HITLRequest)
		if !ok {
			return
		}
		got = req
		reply.RequestID = req.ID
		// Emit the response asynchronously so RequestApproval has a chance
		// to enter its select. A direct nested Emit() works too because
		// the bus is synchronous and the response handler will accept the
		// match before we return — but goroutine matches the production
		// shape (out-of-band responder).
		go func() {
			_ = bus.Emit("hitl.responded", reply)
		}()
	}, engine.WithPriority(10))
	return func() events.HITLRequest { return got }
}

func TestRequestApproval_AllowResolvesToTrue(t *testing.T) {
	bus := engine.NewEventBus()
	getReq := observeRequest(bus, events.HITLResponse{ChoiceID: "allow"})

	resp, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Errorf("expected allowed=true, got false (resp=%+v)", resp)
	}
	if resp.ChoiceID != "allow" {
		t.Errorf("expected choice_id=allow, got %q", resp.ChoiceID)
	}
	captured := getReq()
	if captured.ID == "" {
		t.Error("expected request to have generated ID")
	}
	if captured.ActionKind != "memory.test.write" {
		t.Errorf("ActionKind not forwarded: %q", captured.ActionKind)
	}
	if captured.Mode != events.HITLModeChoices {
		t.Errorf("expected Mode=choices, got %q", captured.Mode)
	}
	if len(captured.Choices) != 2 {
		t.Errorf("expected 2 default choices, got %d", len(captured.Choices))
	}
	if captured.Deadline.IsZero() {
		t.Error("expected non-zero deadline when timeout > 0")
	}
}

func TestRequestApproval_RejectResolvesToFalse(t *testing.T) {
	bus := engine.NewEventBus()
	observeRequest(bus, events.HITLResponse{ChoiceID: "reject"})

	resp, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected allowed=false, got true (resp=%+v)", resp)
	}
}

func TestRequestApproval_TimeoutWithDefaultChoice(t *testing.T) {
	bus := engine.NewEventBus()
	// Don't subscribe — no response will arrive.

	start := time.Now()
	resp, allowed, err := RequestApproval(context.Background(), Request{
		Bus:             bus,
		Logger:          testLogger,
		PluginID:        "test.plugin",
		ActionKind:      "memory.test.write",
		Prompt:          "Approve?",
		Timeout:         50 * time.Millisecond,
		DefaultChoiceID: "reject",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("returned too early: %v", elapsed)
	}
	if allowed {
		t.Error("expected allowed=false on timeout with reject default")
	}
	if !resp.Cancelled && resp.ChoiceID != "reject" {
		t.Errorf("expected ChoiceID=reject (or cancelled), got %+v", resp)
	}
}

func TestRequestApproval_TimeoutDefaultAllowResolvesAllowed(t *testing.T) {
	bus := engine.NewEventBus()

	_, allowed, err := RequestApproval(context.Background(), Request{
		Bus:             bus,
		Logger:          testLogger,
		PluginID:        "test.plugin",
		ActionKind:      "memory.test.write",
		Prompt:          "Approve?",
		Timeout:         25 * time.Millisecond,
		DefaultChoiceID: "allow",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true when DefaultChoiceID=allow on timeout")
	}
}

func TestRequestApproval_TimeoutNoDefaultIsCancelled(t *testing.T) {
	bus := engine.NewEventBus()

	resp, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
		Timeout:    25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false when timeout fires with no default")
	}
	if !resp.Cancelled {
		t.Errorf("expected Cancelled=true, got %+v", resp)
	}
}

func TestRequestApproval_ContextCancelled(t *testing.T) {
	bus := engine.NewEventBus()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	resp, allowed, err := RequestApproval(ctx, Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false when context cancelled")
	}
	if !resp.Cancelled {
		t.Error("expected Cancelled=true on context cancel")
	}
}

func TestRequestApproval_CancelledResponseNotAllowed(t *testing.T) {
	bus := engine.NewEventBus()
	observeRequest(bus, events.HITLResponse{Cancelled: true, CancelReason: "operator quit"})

	_, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("cancelled response should never be allowed")
	}
}

func TestRequestApproval_EditedPayloadLogsAndProceeds(t *testing.T) {
	bus := engine.NewEventBus()
	observeRequest(bus, events.HITLResponse{
		ChoiceID:      "reject",
		EditedPayload: map[string]any{"limit": 50},
	})

	// No assertion on the warning text itself — slog default writes to
	// stderr and we don't want to capture it for this case. We only
	// assert that an edited_payload alongside reject still surfaces as
	// not-allowed (i.e., edit semantics are explicitly out of scope).
	_, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("reject + edited_payload should not allow")
	}
}

func TestRequestApproval_NilBusErrors(t *testing.T) {
	_, _, err := RequestApproval(context.Background(), Request{
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
	})
	if err == nil {
		t.Fatal("expected error when bus is nil")
	}
}

func TestRequestApproval_PassesCustomChoices(t *testing.T) {
	bus := engine.NewEventBus()
	custom := []events.HITLChoice{
		{ID: "yes", Label: "Yes", Kind: events.ChoiceAllow},
		{ID: "no", Label: "No", Kind: events.ChoiceReject},
	}
	observeRequest(bus, events.HITLResponse{ChoiceID: "yes"})

	_, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
		Choices:    custom,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("custom 'yes' choice with Kind=allow should resolve allowed")
	}
}

func TestRequestApproval_UnknownChoiceIDNotAllowed(t *testing.T) {
	bus := engine.NewEventBus()
	observeRequest(bus, events.HITLResponse{ChoiceID: "mystery"})

	_, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("unknown choice id should never be allowed")
	}
}

func TestRequestApproval_MapPayloadResponseDecoded(t *testing.T) {
	bus := engine.NewEventBus()
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.HITLRequest)
		if !ok {
			return
		}
		go func() {
			// Simulate journal-replay shape: bus carries map[string]any.
			_ = bus.Emit("hitl.responded", map[string]any{
				"request_id": req.ID,
				"choice_id":  "allow",
			})
		}()
	}, engine.WithPriority(10))

	_, allowed, err := RequestApproval(context.Background(), Request{
		Bus:        bus,
		Logger:     testLogger,
		PluginID:   "test.plugin",
		ActionKind: "memory.test.write",
		Prompt:     "Approve?",
		Timeout:    1 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true with map[string]any response shape")
	}
}
