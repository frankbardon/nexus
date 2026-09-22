package batch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// batchModels builds a registry of single-entry roles, one per map key, merged
// over an OpenAI provider/model skeleton.
func batchModels(entries map[string]map[string]any) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, extra := range entries {
		cfg := map[string]any{"provider": "nexus.llm.openai", "model": "gpt-5"}
		for k, v := range extra {
			cfg[k] = v
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

// captureSubmit runs one submitOpenAI against stub /files and /batches
// handlers and hands back the decoded JSONL lines plus the create-batch body.
func captureSubmit(t *testing.T, configure func(p *Plugin), requests []events.BatchRequest) ([]map[string]any, map[string]any, error) {
	t.Helper()

	var (
		uploaded atomic.Value // []byte
		created  atomic.Value // map[string]any
	)
	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/openai/files", func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
				return
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("form file: %v", err)
				return
			}
			defer f.Close()
			raw, _ := io.ReadAll(f)
			uploaded.Store(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"file-xyz"}`))
		})
		mux.HandleFunc("/openai/batches", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("decode create-batch: %v", err)
				return
			}
			created.Store(got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"batch_abc"}`))
		})
	})
	if configure != nil {
		configure(p)
	}

	if _, err := p.submitOpenAI(context.Background(), requests); err != nil {
		return nil, nil, err
	}

	var lines []map[string]any
	raw, _ := uploaded.Load().([]byte)
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("decode jsonl line %q: %v", l, err)
		}
		lines = append(lines, line)
	}
	createBody, _ := created.Load().(map[string]any)
	return lines, createBody, nil
}

func oneRequest(role string, req events.LLMRequest) []events.BatchRequest {
	req.SchemaVersion = events.LLMRequestVersion
	req.Role = role
	if req.Model == "" {
		req.Model = "gpt-5"
	}
	if len(req.Messages) == 0 {
		req.Messages = []events.Message{{Role: "user", Content: "hi"}}
	}
	return []events.BatchRequest{{CustomID: "req-0", Request: req}}
}

// =====================================================================
// The line url follows the role's effective api:
// =====================================================================

// TestSubmitOpenAI_DefaultsToResponses pins the coordinator's default surface
// to the provider's. nexus.llm.batch has no base_url and no Azure mode, so the
// narrowing that would move nexus.llm.openai to chat_completions never applies
// here and the two agree by construction.
func TestSubmitOpenAI_DefaultsToResponses(t *testing.T) {
	lines, create, err := captureSubmit(t, nil, oneRequest("", events.LLMRequest{}))
	if err != nil {
		t.Fatalf("submitOpenAI: %v", err)
	}
	if got := lines[0]["url"]; got != openaiResponsesEndpointPath {
		t.Errorf("line url = %v, want %s", got, openaiResponsesEndpointPath)
	}
	if got := create["endpoint"]; got != openaiResponsesEndpointPath {
		t.Errorf("create-batch endpoint = %v, want %s", got, openaiResponsesEndpointPath)
	}
	body, _ := lines[0]["body"].(map[string]any)
	if _, ok := body["input"]; !ok {
		t.Errorf("responses body should carry input, got %v", body)
	}
	if _, ok := body["messages"]; ok {
		t.Errorf("responses body must not carry messages: %v", body)
	}
	if body["store"] != false {
		t.Errorf("responses body should send store:false, got %v", body["store"])
	}
}

// TestSubmitOpenAI_RoleAPIWins is the acceptance criterion itself: a role
// declaring `api: chat_completions` is batched through that endpoint even
// though the coordinator's default is the other one.
func TestSubmitOpenAI_RoleAPIWins(t *testing.T) {
	models := batchModels(map[string]map[string]any{
		"legacy": {"api": "chat_completions"},
	})
	lines, create, err := captureSubmit(t,
		func(p *Plugin) { p.models = models },
		oneRequest("legacy", events.LLMRequest{}))
	if err != nil {
		t.Fatalf("submitOpenAI: %v", err)
	}
	if got := lines[0]["url"]; got != openaiChatEndpointPath {
		t.Errorf("line url = %v, want %s", got, openaiChatEndpointPath)
	}
	if got := create["endpoint"]; got != openaiChatEndpointPath {
		t.Errorf("create-batch endpoint = %v, want %s", got, openaiChatEndpointPath)
	}
	// Chat-path batching is unchanged: same body builder, same shape.
	body, _ := lines[0]["body"].(map[string]any)
	if _, ok := body["messages"]; !ok {
		t.Errorf("chat body should carry messages, got %v", body)
	}
	if _, ok := body["max_tokens"]; !ok {
		t.Errorf("chat body should carry max_tokens, got %v", body)
	}
	if _, ok := body["reasoning"]; ok {
		t.Errorf("chat path must stay reasoning-free: %v", body)
	}
}

// TestSubmitOpenAI_CoordinatorAPIPinsChat covers providers.openai.api — the
// coordinator's own statement, which a role can still outrank.
func TestSubmitOpenAI_CoordinatorAPIPinsChat(t *testing.T) {
	lines, _, err := captureSubmit(t,
		func(p *Plugin) { p.openaiAPI = openaiAPIChatCompletions },
		oneRequest("", events.LLMRequest{}))
	if err != nil {
		t.Fatalf("submitOpenAI: %v", err)
	}
	if got := lines[0]["url"]; got != openaiChatEndpointPath {
		t.Errorf("line url = %v, want %s", got, openaiChatEndpointPath)
	}
}

// TestSubmitOpenAI_MixedSurfacesRejected: one OpenAI batch carries one
// endpoint, so a submit whose roles disagree cannot be expressed and fails
// naming both sides rather than being flattened onto one.
func TestSubmitOpenAI_MixedSurfacesRejected(t *testing.T) {
	models := batchModels(map[string]map[string]any{
		"legacy": {"api": "chat_completions"},
		"deep":   {"api": "responses"},
	})
	reqs := []events.BatchRequest{
		{CustomID: "a", Request: events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "legacy", Model: "gpt-5",
			Messages: []events.Message{{Role: "user", Content: "hi"}}}},
		{CustomID: "b", Request: events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "deep", Model: "gpt-5",
			Messages: []events.Message{{Role: "user", Content: "hi"}}}},
	}
	_, _, err := captureSubmit(t, func(p *Plugin) { p.models = models }, reqs)
	if err == nil {
		t.Fatal("expected a mixed-surface submit to fail")
	}
	for _, want := range []string{"mixes api surfaces", `"a"`, `"b"`, "chat_completions", "responses"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// =====================================================================
// A batched reasoning role carries its reasoning configuration
// =====================================================================

// TestSubmitOpenAI_RoleReasoningReachesTheBody is the second acceptance
// criterion: the role's own reasoning block and its shared effort: both reach
// the Responses body.
func TestSubmitOpenAI_RoleReasoningReachesTheBody(t *testing.T) {
	cases := []struct {
		name        string
		entry       map[string]any
		coordinator map[string]any
		wantEffort  string
		wantSummary string
		wantNone    bool
	}{
		{
			name:       "role effort under a role-declared mode",
			entry:      map[string]any{"effort": "high", "reasoning": map[string]any{"mode": "effort"}},
			wantEffort: "high",
		},
		{
			name:        "role block names both",
			entry:       map[string]any{"reasoning": map[string]any{"effort": "xhigh", "summary": "detailed"}},
			wantEffort:  "xhigh",
			wantSummary: "detailed",
		},
		{
			name:       "role block effort outranks the role's shared effort",
			entry:      map[string]any{"effort": "low", "reasoning": map[string]any{"effort": "max"}},
			wantEffort: "max",
		},
		{
			name:        "role effort rides the coordinator's declared mode",
			entry:       map[string]any{"effort": "medium"},
			coordinator: map[string]any{"mode": "effort", "summary": "auto"},
			wantEffort:  "medium",
			wantSummary: "auto",
		},
		{
			name:     "a bare effort with no declared mode sends nothing",
			entry:    map[string]any{"effort": "high"},
			wantNone: true,
		},
		{
			name:     "mode off wins over a configured depth",
			entry:    map[string]any{"effort": "high", "reasoning": map[string]any{"mode": "off"}},
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models := batchModels(map[string]map[string]any{"deep": tc.entry})
			lines, _, err := captureSubmit(t, func(p *Plugin) {
				p.models = models
				if tc.coordinator != nil {
					rc, perr := parseOpenAIReasoning(tc.coordinator)
					if perr != nil {
						t.Fatalf("parse coordinator block: %v", perr)
					}
					p.openaiReasoning, p.openaiReasoningRaw = rc, tc.coordinator
				}
			}, oneRequest("deep", events.LLMRequest{}))
			if err != nil {
				t.Fatalf("submitOpenAI: %v", err)
			}
			body, _ := lines[0]["body"].(map[string]any)
			reasoning, ok := body["reasoning"].(map[string]any)
			if tc.wantNone {
				if ok {
					t.Fatalf("expected no reasoning key, got %v", reasoning)
				}
				return
			}
			if !ok {
				t.Fatalf("expected a reasoning object, body was %v", body)
			}
			if got := reasoning["effort"]; got != tc.wantEffort {
				t.Errorf("effort = %v, want %q", got, tc.wantEffort)
			}
			if tc.wantSummary == "" {
				if _, present := reasoning["summary"]; present {
					t.Errorf("unexpected summary: %v", reasoning["summary"])
				}
			} else if got := reasoning["summary"]; got != tc.wantSummary {
				t.Errorf("summary = %v, want %q", got, tc.wantSummary)
			}
		})
	}
}

// TestApplyOpenAIResponsesReasoning_StripsRejectedFields: declaring the target
// a reasoning model removes the sampling parameters such a model rejects.
func TestApplyOpenAIResponsesReasoning_StripsRejectedFields(t *testing.T) {
	temp := 0.7
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "gpt-5", Temperature: &temp,
		Messages: []events.Message{{Role: "user", Content: "hi"}}}
	body, err := buildOpenAIResponsesBody(req, 1024, openaiReasoning{Mode: openaiReasoningModeEffort, Effort: "high"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("temperature should have been stripped: %v", body)
	}
}

// =====================================================================
// Body shape
// =====================================================================

func TestBuildOpenAIResponsesBody_Shape(t *testing.T) {
	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5",
		MaxTokens:     512,
		Messages: []events.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "weather?"},
			{Role: "assistant", Content: "checking", ToolCalls: []events.ToolCallRequest{{ID: "call_1", Name: "lookup"}}},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
		Tools:          []events.ToolDef{{Name: "lookup", Description: "look up", Parameters: map[string]any{"type": "object"}}},
		ResponseFormat: &events.ResponseFormat{Type: "json_schema", Name: "out", Schema: map[string]any{"type": "object"}, Strict: true},
	}
	body, err := buildOpenAIResponsesBody(req, 1024, openaiReasoning{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if body["max_output_tokens"] != 512 {
		t.Errorf("max_output_tokens = %v, want 512", body["max_output_tokens"])
	}
	items, _ := body["input"].([]map[string]any)
	if len(items) != 5 {
		t.Fatalf("expected 5 input items (system, user, assistant text, function_call, output), got %d: %v", len(items), items)
	}
	if items[2]["role"] != "assistant" || items[3]["type"] != "function_call" {
		t.Errorf("assistant turn should flatten into text + function_call items: %v", items)
	}
	if items[3]["call_id"] != "call_1" || items[3]["arguments"] != "{}" {
		t.Errorf("function_call item mismatch: %v", items[3])
	}
	if items[4]["type"] != "function_call_output" || items[4]["call_id"] != "call_1" {
		t.Errorf("tool result should be a function_call_output item: %v", items[4])
	}
	tools, _ := body["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "lookup" || tools[0]["strict"] != false {
		t.Errorf("tool should be flattened with an explicit strict:false: %v", tools)
	}
	text, _ := body["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "out" || format["strict"] != true {
		t.Errorf("structured output should live under text.format: %v", text)
	}
	if _, ok := body["stream"]; ok {
		t.Errorf("a batch line is never a stream: %v", body)
	}
}

func TestBuildOpenAIResponsesBody_RejectsEmptyModel(t *testing.T) {
	if _, err := buildOpenAIResponsesBody(events.LLMRequest{SchemaVersion: events.LLMRequestVersion}, 1024, openaiReasoning{}); err == nil {
		t.Fatal("expected an error for a request with no model")
	}
}

// =====================================================================
// Result parsing
// =====================================================================

func TestOpenAIResultBodyIsResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"responses by object", `{"object":"response","output":[]}`, true},
		{"chat by object", `{"object":"chat.completion","choices":[]}`, false},
		{"responses by output", `{"output":[{"type":"message"}]}`, true},
		{"chat by choices", `{"choices":[{"index":0}]}`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openaiResultBodyIsResponses(json.RawMessage(tc.body)); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResultsOpenAI_ResponsesShape is the fourth acceptance criterion: an
// output file written by a Responses batch decodes into the same LLMResponse a
// chat one would, finish reason and all.
func TestResultsOpenAI_ResponsesShape(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"id":"r1","custom_id":"a","response":{"status_code":200,"body":{"id":"resp_1","object":"response","model":"gpt-5","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"xx"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi there"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5,"output_tokens_details":{"reasoning_tokens":2}}}}}`,
		`{"id":"r2","custom_id":"b","response":{"status_code":200,"body":{"id":"resp_2","object":"response","model":"gpt-5","status":"completed","output":[{"type":"function_call","call_id":"call_9","name":"lookup","arguments":"{\"q\":1}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}}`,
		`{"id":"r3","custom_id":"c","response":{"status_code":200,"body":{"id":"resp_3","object":"response","model":"gpt-5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{}}}}`,
		`{"id":"r4","custom_id":"d","response":{"status_code":200,"body":{"id":"resp_4","object":"response","model":"gpt-5","error":{"code":"server_error","message":"run blew up"}}}}`,
	}, "\n") + "\n"

	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/openai/files/file-out/content", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(jsonl))
		})
	})

	results, err := p.resultsOpenAI(context.Background(), "file-out")
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}

	if results[0].Response == nil || results[0].Response.Content != "hi there" {
		t.Fatalf("text result mismatch: %#v", results[0])
	}
	if results[0].Response.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop", results[0].Response.FinishReason)
	}
	if results[0].Response.Usage.PromptTokens != 2 || results[0].Response.Usage.ReasoningTokens != 2 {
		t.Errorf("usage mismatch: %#v", results[0].Response.Usage)
	}

	if results[1].Response == nil || len(results[1].Response.ToolCalls) != 1 {
		t.Fatalf("tool-call result mismatch: %#v", results[1])
	}
	if tc := results[1].Response.ToolCalls[0]; tc.ID != "call_9" || tc.Name != "lookup" {
		t.Errorf("tool call mismatch: %#v", tc)
	}
	if results[1].Response.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", results[1].Response.FinishReason)
	}

	if results[2].Response == nil || results[2].Response.FinishReason != "length" {
		t.Errorf("incomplete run should map to length: %#v", results[2])
	}

	// A run that failed inside an HTTP 200 is an error, not an empty success.
	if !strings.Contains(results[3].Error, "run blew up") {
		t.Errorf("failed run should surface as an error: %#v", results[3])
	}
}

// TestResultsOpenAI_ChatStillDecodes guards the "chat-path batching unchanged"
// criterion on the read side: sniffing must not steal a chat body.
func TestResultsOpenAI_ChatStillDecodes(t *testing.T) {
	jsonl := `{"id":"r1","custom_id":"a","response":{"status_code":200,"body":{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}}` + "\n"

	p, _ := newTestPlugin(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/openai/files/file-out/content", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(jsonl))
		})
	})
	results, err := p.resultsOpenAI(context.Background(), "file-out")
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 1 || results[0].Response == nil || results[0].Response.Content != "hi" {
		t.Fatalf("chat body should still decode: %#v", results)
	}
}

// =====================================================================
// Config validation
// =====================================================================

func TestParseOpenAIReasoning(t *testing.T) {
	cases := []struct {
		name    string
		block   map[string]any
		wantErr bool
		want    openaiReasoning
	}{
		{name: "nil is off", block: nil, want: openaiReasoning{Mode: openaiReasoningModeOff}},
		{name: "present with no mode is effort", block: map[string]any{}, want: openaiReasoning{Mode: openaiReasoningModeEffort}},
		{name: "full", block: map[string]any{"mode": "effort", "effort": "xhigh", "summary": "concise"},
			want: openaiReasoning{Mode: openaiReasoningModeEffort, Effort: "xhigh", Summary: "concise"}},
		{name: "bad mode", block: map[string]any{"mode": "thorough"}, wantErr: true},
		{name: "bad effort", block: map[string]any{"effort": "deep"}, wantErr: true},
		{name: "bad summary", block: map[string]any{"summary": "verbose"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOpenAIReasoning(tc.block)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestInitRejectsBadOpenAIAPI: a typo'd surface fails the boot, not the first
// submit.
func TestInitRejectsBadOpenAIAPI(t *testing.T) {
	p := &Plugin{}
	err := p.Init(engine.PluginContext{
		Logger: silentLogger(),
		Config: map[string]any{
			"data_dir": t.TempDir(),
			"providers": map[string]any{
				"openai": map[string]any{"api": "assistants"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "providers.openai.api") {
		t.Fatalf("expected a providers.openai.api error, got %v", err)
	}
}

func TestInitRejectsBadOpenAIReasoning(t *testing.T) {
	p := &Plugin{}
	err := p.Init(engine.PluginContext{
		Logger: silentLogger(),
		Config: map[string]any{
			"data_dir": t.TempDir(),
			"providers": map[string]any{
				"openai": map[string]any{"reasoning": map[string]any{"effort": "deep"}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "reasoning.effort") {
		t.Fatalf("expected a reasoning.effort error, got %v", err)
	}
}
