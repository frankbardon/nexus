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

// A reasoning model refuses the next request that replays a tool call without
// the opaque continuity token it issued beside it -- Gemini's thoughtSignature,
// Anthropic's thinking blocks. AG-UI is a client-owned-history protocol, so the
// only party able to hand that token back is the client. The token left on the
// CALL, because that is the grain it was issued at and the grain the spec
// merges a TOOL_CALL_* event's metadata into, so it comes back there too.
func TestAReplayedToolCallsSignatureReachesMessageMetadata(t *testing.T) {
	p := &Plugin{}
	ui := p.buildUserInput(runInput{
		threadID: "thread-1",
		messages: []agui.Message{
			{
				ID:   "m1",
				Role: "assistant",
				ToolCalls: []agui.ToolCall{{
					ID:       "call_0_lookup",
					Function: agui.ToolCallFunction{Name: "lookup", Arguments: `{}`},
					Metadata: map[string]any{
						"gemini_thought_signatures": map[string]any{"call_0_lookup": "opaque-token"},
					},
				}},
			},
			{ID: "m2", Role: "user", Content: "and the quarter before"},
		},
	})

	if len(ui.PreloadMessages) != 1 {
		t.Fatalf("preloaded %d messages, want 1", len(ui.PreloadMessages))
	}
	meta := ui.PreloadMessages[0].Metadata
	if meta == nil {
		t.Fatal("replayed message carries no Metadata -- the provider will replay the " +
			"call bare and the API may refuse the request")
	}
	sigs, ok := meta["gemini_thought_signatures"].(map[string]any)
	if !ok {
		t.Fatalf("Metadata[gemini_thought_signatures] = %T, want a map",
			meta["gemini_thought_signatures"])
	}
	if got := sigs["call_0_lookup"]; got != "opaque-token" {
		t.Errorf("signature for call_0_lookup = %v, want %q", got, "opaque-token")
	}
}

// Gemini signs the FIRST functionCall of a parallel batch and no other, so a
// turn's signatures arrive spread across its calls and sparse. Reassembly has to
// put them back into one map without letting the unsigned calls erase it.
func TestEveryToolCallsMetadataFoldsIntoOneMap(t *testing.T) {
	p := &Plugin{}
	ui := p.buildUserInput(runInput{
		threadID: "t",
		messages: []agui.Message{
			{
				ID:   "m1",
				Role: "assistant",
				ToolCalls: []agui.ToolCall{
					{
						ID:       "call_0_a",
						Function: agui.ToolCallFunction{Name: "a"},
						Metadata: map[string]any{
							"gemini_thought_signatures": map[string]any{"call_0_a": "sig-a"},
							"thinking_blocks":           []any{"block"},
						},
					},
					{ID: "call_1_b", Function: agui.ToolCallFunction{Name: "b"}},
					{
						ID:       "call_2_c",
						Function: agui.ToolCallFunction{Name: "c"},
						Metadata: map[string]any{
							"gemini_thought_signatures": map[string]any{"call_2_c": "sig-c"},
						},
					},
				},
			},
			{ID: "m2", Role: "user", Content: "next"},
		},
	})

	meta := ui.PreloadMessages[0].Metadata
	sigs, _ := meta["gemini_thought_signatures"].(map[string]any)
	if len(sigs) != 2 {
		t.Fatalf("folded %d signatures, want 2: %v", len(sigs), sigs)
	}
	if sigs["call_0_a"] != "sig-a" || sigs["call_2_c"] != "sig-c" {
		t.Errorf("signatures = %v, want both calls' own tokens", sigs)
	}
	if _, ok := meta["thinking_blocks"]; !ok {
		t.Error("the per-message key did not survive the fold")
	}
}

// The inbound map is authored by a CLIENT, so the allowlist is a trust boundary
// here rather than tidiness: without it a replayed message could name an
// engine-internal routing hint and have it applied as though an internal
// sub-flow had produced the message.
func TestAReplayedMessageCannotInjectEngineInternalMetadata(t *testing.T) {
	p := &Plugin{}
	ui := p.buildUserInput(runInput{
		threadID: "t",
		messages: []agui.Message{
			{
				ID:   "m1",
				Role: "assistant",
				Metadata: map[string]any{
					"_source":            "nexus.agent.subagent",
					"_structured_output": true,
					"thinking_blocks":    []any{"legit"},
				},
			},
			{ID: "m2", Role: "user", Content: "next"},
		},
	})

	meta := ui.PreloadMessages[0].Metadata
	for _, k := range []string{"_source", "_structured_output"} {
		if _, ok := meta[k]; ok {
			t.Errorf("client-authored %q survived the allowlist", k)
		}
	}
	if _, ok := meta["thinking_blocks"]; !ok {
		t.Error("the allowlisted key was dropped along with the rest")
	}
}

// A client that preserves neither metadata field must leave the message exactly
// as it arrives today -- the status quo, never a new failure.
func TestAMessageWithNoUsableMetadataIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  agui.Message
	}{
		{"absent", agui.Message{ID: "m1", Role: "assistant", Content: "hi"}},
		{"empty map", agui.Message{ID: "m1", Role: "assistant", Content: "hi",
			Metadata: map[string]any{}}},
		{"nothing allowlisted", agui.Message{ID: "m1", Role: "assistant", Content: "hi",
			Metadata: map[string]any{"trace_id": "abc"}}},
		{"call metadata of the wrong shape", agui.Message{ID: "m1", Role: "assistant", Content: "hi",
			ToolCalls: []agui.ToolCall{{ID: "call_0_a",
				Metadata: map[string]any{"gemini_thought_signatures": "not-a-map"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			ui := p.buildUserInput(runInput{
				threadID: "t",
				messages: []agui.Message{tc.msg, {ID: "m2", Role: "user", Content: "next"}},
			})
			if len(ui.PreloadMessages) != 1 {
				t.Fatalf("preloaded %d, want 1", len(ui.PreloadMessages))
			}
			if md := ui.PreloadMessages[0].Metadata; len(md) != 0 {
				t.Errorf("Metadata = %v, want nothing", md)
			}
			if ui.PreloadMessages[0].Content != "hi" {
				t.Errorf("Content = %q, want it carried through untouched",
					ui.PreloadMessages[0].Content)
			}
		})
	}
}
