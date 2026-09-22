package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the non-streaming Responses reply parser. The SSE reader is
// its own story; everything here starts from a complete JSON body.

// decodeReply is the parser's real entry shape: the tests hand it the bytes the
// API would send rather than a hand-built struct, so a field renamed in the
// struct tags fails here.
func decodeReply(t *testing.T, raw string) responsesReply {
	t.Helper()
	var reply responsesReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}

// --- output Items ----------------------------------------------------------

// Text arrives as output_text parts inside message Items, and several of them
// concatenate into the one Content string the rest of Nexus expects.
func TestResponsesReply_TextItems(t *testing.T) {
	reply := decodeReply(t, `{
	  "model": "gpt-5.1",
	  "status": "completed",
	  "output": [
	    {"type":"message","role":"assistant","content":[
	      {"type":"output_text","text":"Hello, "},
	      {"type":"output_text","text":"world."}
	    ]}
	  ]
	}`)

	resp := quietPlugin().convertResponsesReply(reply)

	if resp.Content != "Hello, world." {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello, world.")
	}
	if resp.Model != "gpt-5.1" {
		t.Errorf("Model = %q, want gpt-5.1", resp.Model)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %d, want 0", len(resp.ToolCalls))
	}
	if resp.SchemaVersion != events.LLMResponseVersion {
		t.Errorf("SchemaVersion = %d, want %d", resp.SchemaVersion, events.LLMResponseVersion)
	}
}

// function_call Items become ToolCallRequests carrying their call_id — the
// handle buildResponsesInput writes back out as a function_call_output's
// call_id. The Item's own `id` is deliberately NOT the one used.
func TestResponsesReply_FunctionCallsCarryCallID(t *testing.T) {
	reply := decodeReply(t, `{
	  "model": "gpt-5.1",
	  "status": "completed",
	  "output": [
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"looking"}]},
	    {"type":"function_call","id":"fc_item_1","call_id":"call_abc","name":"search","arguments":"{\"q\":\"go\"}"},
	    {"type":"function_call","id":"fc_item_2","call_id":"call_def","name":"fetch","arguments":""}
	  ]
	}`)

	resp := quietPlugin().convertResponsesReply(reply)

	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_abc" {
		t.Errorf("ToolCalls[0].ID = %q, want the call_id call_abc", resp.ToolCalls[0].ID)
	}
	if resp.ToolCalls[0].Name != "search" || resp.ToolCalls[0].Arguments != `{"q":"go"}` {
		t.Errorf("ToolCalls[0] = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID != "call_def" {
		t.Errorf("ToolCalls[1].ID = %q, want call_def", resp.ToolCalls[1].ID)
	}
	// An empty arguments string is not valid JSON; the serializer normalises
	// it on the way out and the parser does the same on the way in.
	if resp.ToolCalls[1].Arguments != "{}" {
		t.Errorf("ToolCalls[1].Arguments = %q, want {}", resp.ToolCalls[1].Arguments)
	}
	if resp.Content != "looking" {
		t.Errorf("Content = %q, want the text Item's text alongside the calls", resp.Content)
	}
}

// Item types this provider never asks for (server-side tools) are stepped over
// rather than failing the turn.
func TestResponsesReply_UnknownItemTypeIsIgnored(t *testing.T) {
	reply := decodeReply(t, `{
	  "status": "completed",
	  "output": [
	    {"type":"web_search_call","id":"ws_1","status":"completed"},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
	  ]
	}`)

	resp := quietPlugin().convertResponsesReply(reply)
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want ok", resp.Content)
	}
}

// --- reasoning Items -------------------------------------------------------

// The load-bearing one: reasoning Items must survive the parser whole,
// encrypted_content included, because nothing downstream can reconstruct them
// and E5-S2 replays them verbatim on the next request.
func TestResponsesReply_CapturesReasoningItemsVerbatim(t *testing.T) {
	reply := decodeReply(t, `{
	  "model": "gpt-5.1",
	  "status": "completed",
	  "output": [
	    {"type":"reasoning","id":"rs_1","encrypted_content":"gAAAAABopaque==","summary":[
	      {"type":"summary_text","text":"Considering the options"}
	    ]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
	    {"type":"reasoning","id":"rs_2","encrypted_content":"gAAAAABsecond=="}
	  ]
	}`)

	resp := quietPlugin().convertResponsesReply(reply)

	items, ok := resp.Metadata[reasoningItemsMetaKey].([]map[string]any)
	if !ok {
		t.Fatalf("Metadata[%s] is %T, want []map[string]any", reasoningItemsMetaKey, resp.Metadata[reasoningItemsMetaKey])
	}
	if len(items) != 2 {
		t.Fatalf("reasoning items = %d, want 2", len(items))
	}
	// Order is the order they arrived in — the replay depends on it.
	if items[0]["id"] != "rs_1" || items[1]["id"] != "rs_2" {
		t.Errorf("reasoning item order = %v, %v", items[0]["id"], items[1]["id"])
	}
	if items[0]["encrypted_content"] != "gAAAAABopaque==" {
		t.Errorf("encrypted_content = %v, want it preserved", items[0]["encrypted_content"])
	}
	if items[1]["encrypted_content"] != "gAAAAABsecond==" {
		t.Errorf("second encrypted_content = %v", items[1]["encrypted_content"])
	}
	// The summary rides along untouched too — the parser narrows nothing.
	if _, ok := items[0]["summary"]; !ok {
		t.Error("summary dropped from the captured reasoning item")
	}

	// And they are not rendered as output.
	if resp.Content != "answer" {
		t.Errorf("Content = %q — reasoning must not leak into the response text", resp.Content)
	}

	// The whole response must survive a JSON round trip: it is persisted to
	// history JSONL and replayed from there.
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(raw), "gAAAAABopaque==") {
		t.Errorf("encrypted_content lost on marshal: %s", raw)
	}
}

// No reasoning Items, no metadata key — a plain turn serialises without an
// empty container.
func TestResponsesReply_NoReasoningNoMetadata(t *testing.T) {
	reply := decodeReply(t, `{"status":"completed","output":[
	  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}
	]}`)

	resp := quietPlugin().convertResponsesReply(reply)
	if _, ok := resp.Metadata[reasoningItemsMetaKey]; ok {
		t.Errorf("Metadata carries %s on a turn with no reasoning", reasoningItemsMetaKey)
	}
}

// --- usage -----------------------------------------------------------------

// input/output token names differ from the chat block; the meaning does not.
func TestResponsesReply_Usage(t *testing.T) {
	reply := decodeReply(t, `{
	  "model":"gpt-5.1",
	  "status":"completed",
	  "output":[],
	  "usage":{
	    "input_tokens": 120,
	    "output_tokens": 300,
	    "total_tokens": 420,
	    "input_tokens_details": {"cached_tokens": 64, "audio_tokens": 5},
	    "output_tokens_details": {"reasoning_tokens": 256, "audio_tokens": 3}
	  }
	}`)

	resp := quietPlugin().convertResponsesReply(reply)

	want := events.Usage{
		PromptTokens:     120,
		CompletionTokens: 300,
		TotalTokens:      420,
		CachedTokens:     64,
		ReasoningTokens:  256,
	}
	got := resp.Usage
	mb := got.ModalityBreakdown
	got.ModalityBreakdown = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Usage = %+v, want %+v", got, want)
	}
	if mb["audio"] != 8 {
		t.Errorf("ModalityBreakdown[audio] = %d, want 8 (input + output)", mb["audio"])
	}
}

// --- finish reason ---------------------------------------------------------

// The lifecycle is translated into the chat path's vocabulary: the same
// provider must not describe the same outcome with two different words
// depending on which `api:` the operator declared.
func TestResponsesReply_FinishReason(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"completed with text", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}]}`, "stop"},
		{"completed with a tool call", `{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"t","arguments":"{}"}]}`, "tool_calls"},
		{"token cap", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, "length"},
		{"filtered", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`, "content_filter"},
		{"incomplete, reason unknown", `{"status":"incomplete","output":[]}`, "incomplete"},
		{"a status we do not know", `{"status":"cancelled","output":[]}`, "cancelled"},
		{"no status at all", `{"output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}]}`, "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := quietPlugin().convertResponsesReply(decodeReply(t, tc.body))
			if resp.FinishReason != tc.want {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tc.want)
			}
		})
	}
}

// --- publication -----------------------------------------------------------

// The publishing contract: engine.PublishLLMResponse, never a raw Emit, with
// the RequestID copied on and the request's passthrough metadata merged over
// the parser's.
func TestResponsesReply_PublishesThroughTheGate(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var got []events.LLMResponse
	bus.Subscribe("llm.response", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.LLMResponse); ok {
			got = append(got, r)
		}
	})

	// A before:llm.response handler must see it — that only happens when the
	// response went through PublishLLMResponse.
	var gated bool
	bus.Subscribe("before:llm.response", func(e engine.Event[any]) {
		gated = true
	})

	body := strings.NewReader(`{
	  "model":"gpt-5.1",
	  "status":"completed",
	  "output":[
	    {"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
	  ],
	  "usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}
	}`)

	p.handleResponsesSyncResponse(body, "req-42", map[string]any{"_structured_output": true}, map[string]string{"team": "core"}, reasoningConfig{})

	if len(got) != 1 {
		t.Fatalf("published %d responses, want 1", len(got))
	}
	resp := got[0]
	if !gated {
		t.Error("before:llm.response never fired — the response bypassed PublishLLMResponse")
	}
	if resp.RequestID != "req-42" {
		t.Errorf("RequestID = %q, want req-42", resp.RequestID)
	}
	if resp.Content != "done" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Tags["team"] != "core" {
		t.Errorf("Tags = %v", resp.Tags)
	}
	if resp.Metadata["_structured_output"] != true {
		t.Errorf("request metadata lost: %v", resp.Metadata)
	}
	if _, ok := resp.Metadata[reasoningItemsMetaKey]; !ok {
		t.Errorf("reasoning items lost in the metadata merge: %v", resp.Metadata)
	}
}

// A run that failed inside an HTTP 200 surfaces as core.error, which is what
// the fallback coordinator watches — not as an empty response.
func TestResponsesReply_RunFailureBecomesAnError(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var responses, errs int
	bus.Subscribe("llm.response", func(e engine.Event[any]) { responses++ })
	bus.Subscribe("core.error", func(e engine.Event[any]) { errs++ })

	p.handleResponsesSyncResponse(strings.NewReader(
		`{"status":"failed","error":{"code":"server_error","message":"boom"},"output":[]}`),
		"req-1", nil, nil, reasoningConfig{})

	if responses != 0 {
		t.Errorf("published %d llm.response events for a failed run, want 0", responses)
	}
	if errs != 1 {
		t.Errorf("emitted %d core.error events, want 1", errs)
	}
}

// Undecodable bodies error rather than publishing an empty turn.
func TestResponsesReply_DecodeFailure(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var responses, errs int
	bus.Subscribe("llm.response", func(e engine.Event[any]) { responses++ })
	bus.Subscribe("core.error", func(e engine.Event[any]) { errs++ })

	p.handleResponsesSyncResponse(strings.NewReader(`{not json`), "req-1", nil, nil, reasoningConfig{})

	if responses != 0 || errs != 1 {
		t.Errorf("responses = %d, errors = %d; want 0 and 1", responses, errs)
	}
}

// --- thinking.step ----------------------------------------------------------

// A non-streamed turn never sees the reasoning-summary event family — the text
// arrives only inside each reasoning Item's `summary` array. It still becomes
// thinking.step, so the two surfaces put the same reasoning on the bus.
func TestResponsesReply_ItemSummariesBecomeThinkingSteps(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var steps []events.ThinkingStep
	bus.Subscribe("thinking.step", func(e engine.Event[any]) {
		if s, ok := e.Payload.(events.ThinkingStep); ok {
			steps = append(steps, s)
		}
	})

	body := strings.NewReader(`{
	  "id":"resp_7","model":"gpt-5.1","status":"completed",
	  "output":[
	    {"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[
	      {"type":"summary_text","text":"Checking the units."},
	      {"type":"summary_text","text":"Then the arithmetic."}
	    ]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"42"}]}
	  ]
	}`)
	p.handleResponsesSyncResponse(body, "req-1", nil, nil, summariesOn())

	if len(steps) != 2 {
		t.Fatalf("emitted %d thinking.step events, want 2: %+v", len(steps), steps)
	}
	for i, want := range []string{"Checking the units.", "Then the arithmetic."} {
		if steps[i].Content != want {
			t.Errorf("step %d Content = %q, want %q", i, steps[i].Content, want)
		}
		if steps[i].Index != i {
			t.Errorf("step %d Index = %d, want %d — the summary array position is the index", i, steps[i].Index, i)
		}
		if steps[i].TurnID != "resp_7" {
			t.Errorf("step %d TurnID = %q, want resp_7", i, steps[i].TurnID)
		}
		if steps[i].Source != pluginID || steps[i].Phase != "reasoning" {
			t.Errorf("step %d = %+v, want source %q phase reasoning", i, steps[i], pluginID)
		}
	}
}

// Summaries are opt-in here too: a turn that asked for none publishes none,
// even when the reply volunteers them. The Items themselves are still captured
// verbatim, because they are replay state rather than display material.
func TestResponsesReply_NoSummaryConfiguredEmitsNoSteps(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var steps int
	var got []events.LLMResponse
	bus.Subscribe("thinking.step", func(e engine.Event[any]) { steps++ })
	bus.Subscribe("llm.response", func(e engine.Event[any]) {
		if resp, ok := e.Payload.(events.LLMResponse); ok {
			got = append(got, resp)
		}
	})

	body := `{
	  "id":"resp_7","model":"gpt-5.1","status":"completed",
	  "output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque",
	    "summary":[{"type":"summary_text","text":"unasked for"}]}]
	}`
	p.handleResponsesSyncResponse(strings.NewReader(body), "req-1", nil, nil, reasoningConfig{})

	if steps != 0 {
		t.Errorf("emitted %d thinking.step events for a turn that asked for no summary, want 0", steps)
	}
	if len(got) != 1 {
		t.Fatalf("published %d responses, want 1", len(got))
	}
	if _, ok := got[0].Metadata[reasoningItemsMetaKey]; !ok {
		t.Errorf("reasoning items dropped — the gate is on emission, not on capture: %v", got[0].Metadata)
	}
}

// A `summary` array in a shape this does not recognise is stepped over rather
// than panicking a live turn: the Items are JSON the provider does not control.
func TestResponsesReply_MalformedSummariesAreSkipped(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var steps int
	bus.Subscribe("thinking.step", func(e engine.Event[any]) { steps++ })

	body := `{
	  "id":"resp_7","model":"gpt-5.1","status":"completed",
	  "output":[
	    {"type":"reasoning","id":"rs_1","summary":"not an array"},
	    {"type":"reasoning","id":"rs_2","summary":["a bare string",{"type":"summary_text"},{"type":"summary_text","text":"real"}]}
	  ]
	}`
	p.handleResponsesSyncResponse(strings.NewReader(body), "req-1", nil, nil, summariesOn())

	if steps != 1 {
		t.Errorf("emitted %d thinking.step events, want 1 (only the one with text)", steps)
	}
}
