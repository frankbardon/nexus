package protocol

import (
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/events"
)

// journalDecodeLLMResponse rehydrates a journal payload into an LLMResponse.
// Journal envelopes round-trip through JSON — the live payload struct is
// re-marshaled at write time and read back as map[string]any here.
func journalDecodeLLMResponse(raw any) (events.LLMResponse, bool) {
	if v, ok := raw.(events.LLMResponse); ok {
		return v, true
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return events.LLMResponse{SchemaVersion: events.LLMResponseVersion}, false
	}
	var out events.LLMResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return events.LLMResponse{SchemaVersion: events.LLMResponseVersion}, false
	}
	return out, true
}

// journalDecodeToolCall rehydrates a journal payload into a ToolCall. The
// LLM-supplied Arguments string lives on ToolCallRequest, but tool.invoke
// carries events.ToolCall (already-parsed Arguments map).
func journalDecodeToolCall(raw any) (events.ToolCall, bool) {
	if v, ok := raw.(events.ToolCall); ok {
		return v, true
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return events.ToolCall{SchemaVersion: events.ToolCallVersion}, false
	}
	var out events.ToolCall
	if err := json.Unmarshal(data, &out); err != nil {
		return events.ToolCall{SchemaVersion: events.ToolCallVersion}, false
	}
	return out, true
}

// journalDecodeToolResult rehydrates a journal payload into a ToolResult.
func journalDecodeToolResult(raw any) (events.ToolResult, bool) {
	if v, ok := raw.(events.ToolResult); ok {
		return v, true
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return events.ToolResult{SchemaVersion: events.ToolResultVersion}, false
	}
	var out events.ToolResult
	if err := json.Unmarshal(data, &out); err != nil {
		return events.ToolResult{SchemaVersion: events.ToolResultVersion}, false
	}
	return out, true
}
