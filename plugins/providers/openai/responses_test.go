package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the Responses API request serializer. The reply parser, the
// SSE reader, the multimodal Item shapes and the endpoint builder are their own
// stories; everything here stops at the body.

func quietPlugin() *Plugin {
	return &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// items pulls the input array out of a body as a slice of maps.
func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	got, ok := body["input"].([]map[string]any)
	if !ok {
		t.Fatalf("input is %T, want []map[string]any", body["input"])
	}
	return got
}

// --- the shape of the body -------------------------------------------------

// max_tokens becomes max_output_tokens, and store is always false: Nexus keeps
// its own history, so server-side state would be a second source of truth.
func TestResponsesBody_TokenCapAndStore(t *testing.T) {
	body := quietPlugin().buildResponsesBody("gpt-5.1", 2048, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
	})

	if body["max_output_tokens"] != 2048 {
		t.Errorf("max_output_tokens = %v, want 2048", body["max_output_tokens"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Error("max_tokens must not appear on the Responses body")
	}
	if store, ok := body["store"].(bool); !ok || store {
		t.Errorf("store = %v, want false — and present on every request", body["store"])
	}
	if _, ok := body["messages"]; ok {
		t.Error("messages must not appear on the Responses body; input replaces it")
	}
	if body["model"] != "gpt-5.1" {
		t.Errorf("model = %v, want gpt-5.1", body["model"])
	}
}

// Azure carries the deployment in the body's model field on the Responses
// route, so the chat path's strip must not apply here.
func TestResponsesBody_ModelSurvivesAzure(t *testing.T) {
	p := quietPlugin()
	p.auth = &authState{mode: authModeAzureKey}

	body := p.buildResponsesBody("my-deployment", 512, events.LLMRequest{SchemaVersion: events.LLMRequestVersion})
	if body["model"] != "my-deployment" {
		t.Errorf("model = %v, want the deployment name in the body", body["model"])
	}

	// The chat path still strips it, unchanged.
	chat := p.buildRequestBody("my-deployment", 512, events.LLMRequest{SchemaVersion: events.LLMRequestVersion})
	if _, ok := chat["model"]; ok {
		t.Error("the chat path must still strip model in Azure mode")
	}
}

// Predicted outputs have no Responses counterpart, so the field is dropped
// rather than sent and rejected.
func TestResponsesBody_PredictionIsDropped(t *testing.T) {
	body := quietPlugin().buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Prediction:    "the known text",
	})
	if _, ok := body["prediction"]; ok {
		t.Errorf("prediction must not reach the Responses body, got %v", body["prediction"])
	}
}

// --- input Items -----------------------------------------------------------

// A turn's assistant text, its tool calls and their results become sibling
// Items linked by call_id, not a message with a tool_calls field.
func TestResponsesInput_ToolRoundTripsAsItems(t *testing.T) {
	p := quietPlugin()
	body := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages: []events.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "weather?"},
			{Role: "assistant", Content: "looking it up", ToolCalls: []events.ToolCallRequest{
				{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Berlin"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "17C"},
		},
	})

	got := items(t, body)
	if len(got) != 5 {
		t.Fatalf("got %d items, want 5: %#v", len(got), got)
	}

	if got[0]["role"] != "system" || got[0]["content"] != "be brief" {
		t.Errorf("item 0 = %#v, want the system message", got[0])
	}
	if got[1]["role"] != "user" || got[1]["content"] != "weather?" {
		t.Errorf("item 1 = %#v, want the user message", got[1])
	}
	if got[2]["role"] != "assistant" || got[2]["content"] != "looking it up" {
		t.Errorf("item 2 = %#v, want the assistant text", got[2])
	}
	if _, ok := got[2]["tool_calls"]; ok {
		t.Error("the assistant Item must not carry a tool_calls field")
	}

	call := got[3]
	if call["type"] != "function_call" || call["call_id"] != "call_1" ||
		call["name"] != "get_weather" || call["arguments"] != `{"city":"Berlin"}` {
		t.Errorf("item 3 = %#v, want the function_call Item", call)
	}

	out := got[4]
	if out["type"] != "function_call_output" || out["call_id"] != "call_1" || out["output"] != "17C" {
		t.Errorf("item 4 = %#v, want the function_call_output Item", out)
	}
	if call["call_id"] != out["call_id"] {
		t.Error("the call and its output must be linked by the same call_id")
	}
}

// A tool-only assistant turn produces no empty message Item — the API has no
// such thing.
func TestResponsesInput_ToolOnlyAssistantEmitsNoMessage(t *testing.T) {
	got := items(t, quietPlugin().buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages: []events.Message{
			{Role: "assistant", ToolCalls: []events.ToolCallRequest{{ID: "call_9", Name: "noop"}}},
		},
	}))

	if len(got) != 1 {
		t.Fatalf("got %d items, want only the function_call: %#v", len(got), got)
	}
	if got[0]["type"] != "function_call" {
		t.Errorf("item 0 = %#v, want a function_call Item", got[0])
	}
	// Empty arguments are not valid JSON on the wire; one lost call must not
	// fail the whole request.
	if got[0]["arguments"] != "{}" {
		t.Errorf("arguments = %v, want the empty object", got[0]["arguments"])
	}
}

// System content goes through the prompt registry here exactly as it does on
// the chat path, and stays in position rather than being hoisted.
func TestResponsesInput_SystemGoesThroughThePromptRegistry(t *testing.T) {
	p := quietPlugin()
	p.prompts = engine.NewPromptRegistry()
	p.prompts.Register("extra", 10, func() string { return "appended" })

	got := items(t, p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages:      []events.Message{{Role: "system", Content: "base"}},
	}))

	content, _ := got[0]["content"].(string)
	if content == "base" {
		t.Error("system content did not go through the prompt registry")
	}
	if _, ok := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{SchemaVersion: events.LLMRequestVersion})["instructions"]; ok {
		t.Error("system content must stay an input Item, not become `instructions`")
	}
}

// --- tools -----------------------------------------------------------------

// The definition is flattened, and strict is written explicitly because on this
// surface omitting it attempts strict mode.
func TestResponsesTools_FlattenedAndExplicitlyNonStrict(t *testing.T) {
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	body := quietPlugin().buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Tools:         []events.ToolDef{{Name: "search", Description: "find things", Parameters: params}},
	})

	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one flattened function", body["tools"])
	}
	tool := tools[0]
	if tool["type"] != "function" || tool["name"] != "search" || tool["description"] != "find things" {
		t.Errorf("tool = %#v, want the flattened shape", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Error("the Responses tool definition must not nest under `function`")
	}
	strict, present := tool["strict"].(bool)
	if !present {
		t.Fatal("strict must be present — omitting it attempts strict mode on this API")
	}
	if strict {
		t.Error("strict must be false: Nexus tool schemas come from plugins, MCP servers and skills, and the chat path is not strict either")
	}
}

// A named tool_choice is flattened the same way the definition is.
func TestResponsesToolChoice_FlattenedNamedTool(t *testing.T) {
	tools := []events.ToolDef{{Name: "search"}}

	got := resolveResponsesToolChoice(&events.ToolChoice{Mode: "tool", Name: "search"}, tools)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("tool_choice = %#v, want a map", got)
	}
	if m["type"] != "function" || m["name"] != "search" {
		t.Errorf("tool_choice = %#v, want {type:function, name:search}", m)
	}
	if _, nested := m["function"]; nested {
		t.Error("the Responses tool_choice must not nest under `function`")
	}

	for _, tc := range []struct {
		mode string
		want any
	}{{"auto", "auto"}, {"required", "required"}, {"none", "none"}} {
		if got := resolveResponsesToolChoice(&events.ToolChoice{Mode: tc.mode}, tools); got != tc.want {
			t.Errorf("mode %q = %v, want %v", tc.mode, got, tc.want)
		}
	}

	// The shared policy still applies: an unknown named tool degrades to
	// "required" rather than naming a tool the model does not have.
	if got := resolveResponsesToolChoice(&events.ToolChoice{Mode: "tool", Name: "absent"}, tools); got != "required" {
		t.Errorf("unknown named tool = %v, want \"required\"", got)
	}
}

// tool_choice: none strips the tools as well, same as the chat path.
func TestResponsesTools_ChoiceNoneStripsThem(t *testing.T) {
	body := quietPlugin().buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Tools:         []events.ToolDef{{Name: "search"}},
		ToolChoice:    &events.ToolChoice{Mode: "none"},
	})
	if _, ok := body["tools"]; ok {
		t.Errorf("tools = %v, want none under tool_choice: none", body["tools"])
	}
}

// --- structured output -----------------------------------------------------

// response_format becomes text.format, flattened, with the caller's own strict
// forwarded verbatim — that one is a deliberate statement, unlike the tool one.
func TestResponsesText_JSONSchema(t *testing.T) {
	schema := map[string]any{"type": "object"}
	body := quietPlugin().buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion:  events.LLMRequestVersion,
		ResponseFormat: &events.ResponseFormat{Type: "json_schema", Name: "answer", Schema: schema, Strict: true},
	})

	if _, ok := body["response_format"]; ok {
		t.Error("response_format must not appear on the Responses body")
	}
	text, ok := body["text"].(map[string]any)
	if !ok {
		t.Fatalf("text = %#v, want a map", body["text"])
	}
	format, ok := text["format"].(map[string]any)
	if !ok {
		t.Fatalf("text.format = %#v, want a map", text["format"])
	}
	if format["type"] != "json_schema" || format["name"] != "answer" || format["strict"] != true {
		t.Errorf("text.format = %#v, want the flattened json_schema form", format)
	}
	if _, nested := format["json_schema"]; nested {
		t.Error("text.format must not nest under `json_schema`")
	}
}

func TestResponsesText_JSONObjectAndText(t *testing.T) {
	p := quietPlugin()

	body := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion:  events.LLMRequestVersion,
		ResponseFormat: &events.ResponseFormat{Type: "json_object"},
	})
	text, _ := body["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Errorf("text.format = %#v, want json_object", format)
	}

	plain := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion:  events.LLMRequestVersion,
		ResponseFormat: &events.ResponseFormat{Type: "text"},
	})
	if _, ok := plain["text"]; ok {
		t.Errorf("text = %v, want nothing for the API's own default", plain["text"])
	}
}

// --- reasoning -------------------------------------------------------------

// The resolved effort and summary both land, as one `reasoning` object, and the
// sampling parameters a reasoning model rejects are stripped.
func TestResponsesReasoning_ObjectCarriesEffortAndSummary(t *testing.T) {
	p := quietPlugin()
	p.reasoning = reasoningConfig{Mode: reasoningModeEffort, Effort: "high", Summary: "auto"}

	temp := 0.7
	body := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Temperature:   &temp,
	})

	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want a map", body["reasoning"])
	}
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Errorf("reasoning = %#v, want effort high and summary auto", reasoning)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("reasoning_effort is the chat scalar and must not appear here")
	}
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must be stripped under a declared reasoning mode")
	}
}

// A role's `effort:` reaches the wire through the same resolveReasoning the
// chat path uses — the lookup is surface-agnostic and is not duplicated here.
func TestResponsesReasoning_RoleEffortReachesTheWire(t *testing.T) {
	p := quietPlugin()
	p.reasoning = reasoningConfig{Mode: reasoningModeEffort, Effort: "low"}

	body := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Effort:        "max",
	})
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "max" {
		t.Errorf("reasoning.effort = %v, want the role's max to outrank the plugin's low", reasoning["effort"])
	}
}

// mode: off — and the absent block that resolves to it — sends no reasoning and
// strips nothing.
func TestResponsesReasoning_ModeOffSendsAndStripsNothing(t *testing.T) {
	p := quietPlugin()
	p.reasoning = reasoningConfig{Mode: reasoningModeOff, Effort: "high", Summary: "auto"}

	temp := 0.3
	body := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Temperature:   &temp,
	})
	if _, ok := body["reasoning"]; ok {
		t.Errorf("reasoning = %v, want nothing under mode: off", body["reasoning"])
	}
	if body["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want it untouched under mode: off", body["temperature"])
	}
}

// An effort-mode block with nothing resolved writes no empty object.
func TestResponsesReasoning_NoEmptyObject(t *testing.T) {
	p := quietPlugin()
	p.reasoning = reasoningConfig{Mode: reasoningModeEffort}

	body := p.buildResponsesBody("gpt-5.1", 512, events.LLMRequest{SchemaVersion: events.LLMRequestVersion})
	if _, ok := body["reasoning"]; ok {
		t.Errorf("reasoning = %v, want no key when neither effort nor summary resolved", body["reasoning"])
	}
}

// --- the summary warning is now surface-conditional ------------------------

const summaryPhrase = "reasoning.summary does not reach the wire"

// On the chat surface the statement still holds, and is still made once.
func TestInit_WarnsOnceThatSummaryCannotReachTheChatWire(t *testing.T) {
	_, records, err := initAPI(t, map[string]any{
		"api":       "chat_completions",
		"reasoning": map[string]any{"mode": "effort", "summary": "detailed"},
	}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, summaryPhrase); n != 1 {
		t.Errorf("warning count = %d, want exactly 1", n)
	}
}

// A role can name the summary the plugin did not, and the warning still fires
// once, naming the role.
func TestInit_WarnsOnceForARoleThatNamesASummary(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"reasoning": map[string]any{"mode": "effort", "summary": "auto"}},
		"deeper":   {"reasoning": map[string]any{"mode": "effort", "summary": "concise"}},
	})
	_, records, err := initAPI(t, map[string]any{}, models)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, summaryPhrase); n != 1 {
		t.Errorf("warning count = %d, want exactly 1", n)
	}
}

// No summary anywhere, nothing to say.
func TestInit_NoSummaryWarningWithoutASummary(t *testing.T) {
	_, records, err := initAPI(t, map[string]any{
		"reasoning": map[string]any{"mode": "effort", "effort": "high"},
	}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, summaryPhrase); n != 0 {
		t.Errorf("warning count = %d, want 0", n)
	}
}

// The point of making it conditional: on the Responses surface the summary does
// reach the wire, so warning about it would be a lie. `api: responses` is still
// an Init error, so the sweep is driven directly here.
func TestWarnSummary_SilentOnTheResponsesSurface(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{
		logger:    slog.New(slog.NewJSONHandler(&buf, nil)),
		api:       apiResponses,
		reasoning: reasoningConfig{Mode: reasoningModeEffort, Summary: "auto"},
	}
	p.warnSummaryOnChatCompletions()
	if buf.Len() != 0 {
		t.Errorf("warned on the Responses surface, where the summary does reach the wire: %s", buf.String())
	}

	// And a role that declares the Responses surface for itself is likewise
	// silent, while its chat-surface sibling is not.
	p = &Plugin{
		logger:       slog.New(slog.NewJSONHandler(&buf, nil)),
		api:          apiChatCompletions,
		reasoning:    reasoningConfig{Mode: reasoningModeEffort},
		reasoningRaw: map[string]any{"mode": "effort"},
		models: apiModels(map[string]map[string]any{
			"onresponses": {"api": "responses", "reasoning": map[string]any{"summary": "auto"}},
		}),
	}
	buf.Reset()
	p.warnSummaryOnChatCompletions()
	if buf.Len() != 0 {
		t.Errorf("warned for a role that declared api: responses: %s", buf.String())
	}
}

// --- round-trip ------------------------------------------------------------

// The whole body marshals — every value in it is JSON-encodable.
func TestResponsesBody_Marshals(t *testing.T) {
	p := quietPlugin()
	p.reasoning = reasoningConfig{Mode: reasoningModeEffort, Effort: "medium", Summary: "auto"}

	body := p.buildResponsesBody("gpt-5.1", 4096, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Stream:        true,
		Messages: []events.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []events.ToolCallRequest{{ID: "c1", Name: "t", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "c1", Content: "done"},
		},
		Tools:          []events.ToolDef{{Name: "t", Parameters: map[string]any{"type": "object"}}},
		ToolChoice:     &events.ToolChoice{Mode: "auto"},
		ResponseFormat: &events.ResponseFormat{Type: "json_schema", Name: "n", Schema: map[string]any{"type": "object"}},
	})

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["store"] != false || back["stream"] != true {
		t.Errorf("store/stream = %v/%v, want false/true", back["store"], back["stream"])
	}
	if n := len(back["input"].([]any)); n != 3 {
		t.Errorf("input items = %d, want 3", n)
	}
}
