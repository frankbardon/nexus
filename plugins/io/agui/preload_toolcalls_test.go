package agui

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
)

// A replayed thread is the normal case, not an edge one: an AG-UI client posts
// the WHOLE conversation on every run, so every turn after the first arrives
// carrying the previous turn's tool calls and tool results.
//
// WHAT WAS MEASURED. A browser's second question against a tool-calling agent
// came back 400 INVALID_ARGUMENT from Gemini, seven times over:
//
//	GenerateContentRequest.contents[18].parts[0].function_response.name:
//	Name cannot be empty.
//
// The preload copied Role and Content and nothing else, so every replayed tool
// result lost its ToolCallID and every replayed assistant turn lost its
// ToolCalls. The provider pairs a response to its call BY NAME, reads that name
// off the matching call, and had neither -- so it emitted an empty one and the
// API refused the request.
func TestAReplayedToolResultKeepsTheCallItAnswers(t *testing.T) {
	p := &Plugin{}
	ui := p.buildUserInput(runInput{
		threadID: "thread-1",
		messages: []agui.Message{
			{ID: "m1", Role: "user", Content: "what was it last quarter"},
			{ID: "m2", Role: "assistant", ToolCalls: []agui.ToolCall{{
				ID:       "call-7",
				Function: agui.ToolCallFunction{Name: "arc_metric_lookup", Arguments: `{"brand":"6"}`},
			}}},
			{ID: "m3", Role: "tool", ToolCallID: "call-7", Content: `{"rows":[{"beraScore":97.9}]}`},
			{ID: "m4", Role: "user", Content: "and the quarter before"},
		},
	})

	if ui.Content != "and the quarter before" {
		t.Fatalf("live turn content = %q, want the trailing user message", ui.Content)
	}
	if len(ui.PreloadMessages) != 3 {
		t.Fatalf("preloaded %d messages, want 3", len(ui.PreloadMessages))
	}

	call := ui.PreloadMessages[1]
	if len(call.ToolCalls) != 1 {
		t.Fatalf("replayed assistant turn carries %d tool calls, want 1 -- an unpaired "+
			"tool result is a 400 of its own", len(call.ToolCalls))
	}
	if got := call.ToolCalls[0].ID; got != "call-7" {
		t.Errorf("replayed tool call ID = %q, want %q", got, "call-7")
	}
	if got := call.ToolCalls[0].Name; got != "arc_metric_lookup" {
		t.Errorf("replayed tool call Name = %q, want %q -- this is the name the "+
			"provider puts on the paired function response", got, "arc_metric_lookup")
	}
	if got := call.ToolCalls[0].Arguments; got != `{"brand":"6"}` {
		t.Errorf("replayed tool call Arguments = %q, want the posted arguments", got)
	}

	result := ui.PreloadMessages[2]
	if result.Role != "tool" {
		t.Fatalf("replayed tool result role = %q, want %q", result.Role, "tool")
	}
	if result.ToolCallID != "call-7" {
		t.Errorf("replayed tool result ToolCallID = %q, want %q -- empty is exactly "+
			"what produced `function_response.name: Name cannot be empty`", result.ToolCallID, "call-7")
	}
}

// A message with no tool calls must not grow an empty slice: a provider that
// branches on len(ToolCalls) would start emitting an empty model turn.
func TestAPlainReplayedMessageCarriesNoToolCalls(t *testing.T) {
	p := &Plugin{}
	ui := p.buildUserInput(runInput{
		threadID: "thread-1",
		messages: []agui.Message{
			{ID: "m1", Role: "assistant", Content: "97.9"},
			{ID: "m2", Role: "user", Content: "thanks"},
		},
	})
	if len(ui.PreloadMessages) != 1 {
		t.Fatalf("preloaded %d messages, want 1", len(ui.PreloadMessages))
	}
	if ui.PreloadMessages[0].ToolCalls != nil {
		t.Errorf("a message with no tool calls carries %v, want nil",
			ui.PreloadMessages[0].ToolCalls)
	}
	if ui.PreloadMessages[0].ToolCallID != "" {
		t.Errorf("a message with no tool call id carries %q, want empty",
			ui.PreloadMessages[0].ToolCallID)
	}
}
