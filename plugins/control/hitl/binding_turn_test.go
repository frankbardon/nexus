package hitl

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// TestBuildRequestFromToolCall_BindsParentTurn pins that ask_user called
// from a nested loop raises its HITLRequest on the parent turn, not the
// loop's private result-matching TurnID, which nexus.io.agui drops (#224).
func TestBuildRequestFromToolCall_BindsParentTurn(t *testing.T) {
	req, errMsg := buildRequestFromToolCall(events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "ask_user",
		Arguments:     map[string]any{"prompt": "Proceed?"},
		TurnID:        "delegate_abc",
		ParentTurnID:  "turn-parent",
	})
	if errMsg != "" {
		t.Fatalf("build: %s", errMsg)
	}
	if req.TurnID != "turn-parent" {
		t.Errorf("HITLRequest.TurnID = %q, want turn-parent", req.TurnID)
	}

	req, _ = buildRequestFromToolCall(events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-2",
		Name:          "ask_user",
		Arguments:     map[string]any{"prompt": "Proceed?"},
		TurnID:        "turn-top",
	})
	if req.TurnID != "turn-top" {
		t.Errorf("top-level HITLRequest.TurnID = %q, want turn-top", req.TurnID)
	}
}
