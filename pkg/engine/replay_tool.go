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
// Lookup order:
//
//  1. Args-keyed disk cache (ReplayState.ToolCache). Survives memory-state
//     divergence between original and replay runs because the lookup key
//     is sha256(tool_id || canonical_args), not call order.
//  2. FIFO stash (ReplayState.Pop). Fallback for tools whose original
//     result was not cached — typically because the cache subscription
//     was not yet installed when the original tool.result fired.
//  3. Empty-stash sentinel: emit an error tool.result so replay
//     divergence surfaces as a tool-level error the agent can see
//     rather than a silent stall.
//
// In every case, the live invoke's correlation IDs win over journaled
// ones — the agent dispatched with these and matches results by ID.
//
// Lives in the engine package (not journal) because the helper needs the
// events package, which the journal package intentionally does not import.
func ReplayToolShortCircuit(replay *ReplayState, bus EventBus, tc events.ToolCall, logger *slog.Logger) bool {
	if replay == nil || !replay.Active() {
		return false
	}

	// Tier 1: args-keyed disk cache.
	if cache := replay.ToolCache(); cache != nil {
		if result, ok := cache.Lookup(tc.Name, tc.Arguments); ok {
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
	}

	// Tier 2: FIFO stash.
	raw, ok := replay.Pop("tool.result")
	if !ok {
		if logger != nil {
			logger.Warn("replay: tool.result stash empty (cache miss + FIFO miss)",
				"tool", tc.Name, "id", tc.ID)
		}
		_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
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
		_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
			Name:   tc.Name,
			Error:  "replay decode failed: " + err.Error(),
			TurnID: tc.TurnID,
		})
		return true
	}
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
