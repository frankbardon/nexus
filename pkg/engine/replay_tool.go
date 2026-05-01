package engine

import (
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

// ReplayToolShortCircuit handles the tool-side replay path. Side-effecting
// tool plugins call it from their tool.invoke handler — after they have
// filtered for tool calls they own — and return early when it returns true.
//
// The helper pops the next journaled tool.result from the replay queue,
// re-stamps the live tool.invoke's ID + Name onto it (the agent correlates
// tool.result back to tool.invoke by ID), and emits the typed result. An
// empty stash emits a stub error result rather than hanging — replay
// divergence surfaces as a tool-level error the agent can see, not a
// silent stall.
//
// Lives in the engine package (not journal) because the helper needs the
// events package, which the journal package intentionally does not import.
func ReplayToolShortCircuit(replay *ReplayState, bus EventBus, tc events.ToolCall, logger *slog.Logger) bool {
	if replay == nil || !replay.Active() {
		return false
	}
	raw, ok := replay.Pop("tool.result")
	if !ok {
		if logger != nil {
			logger.Warn("replay: tool.result stash empty", "tool", tc.Name, "id", tc.ID)
		}
		_ = bus.Emit("tool.result", events.ToolResult{
			ID:     tc.ID,
			Name:   tc.Name,
			Error:  "replay stash empty",
			TurnID: tc.TurnID,
		})
		return true
	}
	result, err := journal.PayloadAs[events.ToolResult](raw)
	if err != nil {
		if logger != nil {
			logger.Warn("replay: tool.result decode failed", "tool", tc.Name, "error", err)
		}
		_ = bus.Emit("tool.result", events.ToolResult{
			ID:     tc.ID,
			Name:   tc.Name,
			Error:  "replay decode failed: " + err.Error(),
			TurnID: tc.TurnID,
		})
		return true
	}
	// Live invoke's correlation IDs win — the agent dispatched with these
	// and matches results by ID. The journaled result's Output / Error /
	// structured payload is the deterministic part.
	result.ID = tc.ID
	if result.Name == "" {
		result.Name = tc.Name
	}
	if result.TurnID == "" {
		result.TurnID = tc.TurnID
	}
	_ = bus.Emit("tool.result", result)
	return true
}
