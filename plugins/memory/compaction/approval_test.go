package compaction

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newApprovalTestPlugin(t *testing.T, approvalCfg map[string]any) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.persist = false
	// Seed messages so finishCompaction has something to compact.
	p.messages = []events.Message{
		{Role: "user", Content: "first user message"},
		{Role: "assistant", Content: "first assistant reply"},
		{Role: "user", Content: "second user message"},
		{Role: "assistant", Content: "second assistant reply"},
		{Role: "user", Content: "third user message"},
	}
	if approvalCfg != nil {
		p.parseApprovalConfig(approvalCfg)
	}
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

func captureCompacted(bus engine.EventBus) *[]events.CompactionComplete {
	var got []events.CompactionComplete
	bus.Subscribe("memory.compacted", func(ev engine.Event[any]) {
		if c, ok := ev.Payload.(events.CompactionComplete); ok {
			got = append(got, c)
		}
	}, engine.WithPriority(100))
	return &got
}

func TestCompactionApprovalDisabled_NoGate(t *testing.T) {
	p, bus := newApprovalTestPlugin(t, nil)
	compacted := captureCompacted(bus)

	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	p.finishCompaction("summary text")

	if requested {
		t.Error("hitl.requested should not fire when require_approval is disabled")
	}
	if len(*compacted) != 1 {
		t.Errorf("expected 1 memory.compacted, got %d", len(*compacted))
	}
}

func TestCompactionApprovalAllowed_Commits(t *testing.T) {
	p, bus := newApprovalTestPlugin(t, map[string]any{
		"enabled": true,
		"timeout": "1s",
	})
	compacted := captureCompacted(bus)
	autoRespond(bus, "allow")

	p.finishCompaction("approved summary")

	if len(*compacted) != 1 {
		t.Errorf("expected memory.compacted after allow, got %d", len(*compacted))
	}
	// First message of the compacted set must be the system-summary stub
	// produced by finishCompaction.
	if len(p.messages) == 0 || p.messages[0].Role != "system" {
		t.Errorf("expected first message to be system summary, got %+v", p.messages)
	}
	if !strings.Contains(p.messages[0].Content, "approved summary") {
		t.Errorf("expected summary text in first message, got %q", p.messages[0].Content)
	}
}

func TestCompactionApprovalRejected_LeavesMessagesIntact(t *testing.T) {
	p, bus := newApprovalTestPlugin(t, map[string]any{
		"enabled": true,
		"timeout": "1s",
	})
	compacted := captureCompacted(bus)
	autoRespond(bus, "reject")

	// Mark compacting=true the way startCompaction would.
	p.mu.Lock()
	p.compacting = true
	prevCount := len(p.messages)
	prevSnapshot := make([]events.Message, len(p.messages))
	copy(prevSnapshot, p.messages)
	p.mu.Unlock()

	p.finishCompaction("rejected summary")

	if len(*compacted) != 0 {
		t.Errorf("memory.compacted should not fire when commit is rejected, got %d", len(*compacted))
	}
	if len(p.messages) != prevCount {
		t.Errorf("expected message count unchanged on reject, got %d (was %d)", len(p.messages), prevCount)
	}
	for i := range p.messages {
		if p.messages[i].Content != prevSnapshot[i].Content {
			t.Errorf("message[%d] content changed: %q vs %q", i, p.messages[i].Content, prevSnapshot[i].Content)
		}
	}
	// compacting flag should be released so a future compaction can run.
	p.mu.Lock()
	stillCompacting := p.compacting
	p.mu.Unlock()
	if stillCompacting {
		t.Error("compacting flag should be cleared after rejection")
	}
}

func TestCompactionApprovalSizeThreshold_SmallSkipsGate(t *testing.T) {
	p, bus := newApprovalTestPlugin(t, map[string]any{
		"enabled": true,
		"match": map[string]any{
			"size_threshold_bytes": 1000,
		},
	})
	compacted := captureCompacted(bus)
	requested := false
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		requested = true
	}, engine.WithPriority(10))

	p.finishCompaction("tiny")

	if requested {
		t.Error("small summary should bypass approval")
	}
	if len(*compacted) != 1 {
		t.Errorf("expected memory.compacted to fire when gate is bypassed, got %d", len(*compacted))
	}
}

func TestCompactionApprovalActionRefSurfacesContext(t *testing.T) {
	p, bus := newApprovalTestPlugin(t, map[string]any{
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

	p.finishCompaction("a context summary worth committing")

	if captured.ActionKind != "memory.compaction.commit" {
		t.Errorf("ActionKind = %q", captured.ActionKind)
	}
	if size, _ := captured.ActionRef["size"].(int); size == 0 {
		t.Error("ActionRef.size should be non-zero")
	}
	if prev, _ := captured.ActionRef["prev_message_count"].(int); prev == 0 {
		t.Error("ActionRef.prev_message_count should reflect the tracked messages")
	}
}

func TestCompactionApprovalTimeoutDefaultRejects(t *testing.T) {
	p, _ := newApprovalTestPlugin(t, map[string]any{
		"enabled":        true,
		"default_choice": "reject",
		"timeout":        "30ms",
	})
	// No responder.

	start := time.Now()
	p.finishCompaction("summary")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("returned too late: %v", elapsed)
	}
	// Messages should be unchanged because the gate rejected on timeout.
	if len(p.messages) != 5 {
		t.Errorf("expected message count unchanged on timeout-reject, got %d", len(p.messages))
	}
}
