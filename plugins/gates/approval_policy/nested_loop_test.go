package approvalpolicy

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
)

// TestApprovalPolicy_LLMRequest_NestedLoopCarriesParentTurnID drives a real
// non-react agent loop (the pkg/delegate runtime) through an llm.request rule
// and requires the raised HITLRequest to name the parent turn. Before every
// loop stamped engine.MetaTurnID, only nexus.agent.react did, so an
// llm.request rule on a delegated / subagent / planexec / orchestrator turn
// raised a request with TurnID "" and a resumed AG-UI run never finished
// (#218).
func TestApprovalPolicy_LLMRequest_NestedLoopCarriesParentTurnID(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "llm.request", "model": "claude-opus-*"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	resp := newScriptedResponder(bus, events.HITLResponse{SchemaVersion: events.HITLResponseVersion, ChoiceID: "allow"})

	// Fake provider: answer each llm.request with a tool-free response so the
	// delegated loop completes after one iteration.
	defer bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		_ = bus.Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "done",
			Metadata:      map[string]any{"_source": req.Metadata["_source"]},
		})
	})()

	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{
		Name:          "analyst",
		Model:         posture.ModelConfig{Model: "claude-opus-4-7"},
		DefaultBudget: posture.ResourceBudget{Timeout: 5 * time.Second},
	})
	rt := &delegate.Runtime{
		Registry: reg,
		Bus:      bus,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxDepth: 3,
	}
	out, err := rt.Run(context.Background(), delegate.Input{
		Posture:    "analyst",
		Task:       "Summarize.",
		ParentTurn: "turn-parent",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != delegate.StatusSuccess {
		t.Fatalf("Status = %v (%s), want success", out.Status, out.Error)
	}

	resp.mu.Lock()
	defer resp.mu.Unlock()
	if len(resp.seen) == 0 {
		t.Fatal("rule did not raise a hitl.requested for the delegated turn")
	}
	if got := resp.seen[0].TurnID; got != "turn-parent" {
		t.Errorf("HITLRequest.TurnID = %q, want turn-parent", got)
	}
}
