package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func twoSystemRequest() events.LLMRequest {
	return events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-sonnet-4-6-20250514",
		MaxTokens: 1024,
		Messages: []events.Message{
			{Role: "system", Content: "agent prompt"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "system", Content: "summary so far"},
			{Role: "user", Content: "next"},
		},
	}
}

// Before GitHub #205 the last system message replaced every earlier one. Both
// must now reach the body, in order, as separate text blocks, and a
// mid-conversation system message is folded into `system`, not into messages.
func TestBuildRequestBody_AllSystemMessagesInOrder(t *testing.T) {
	p := &Plugin{logger: silentLogger()}
	req := twoSystemRequest()
	body := p.buildRequestBody(req.Model, req.MaxTokens, req)

	sys, ok := body["system"].([]map[string]any)
	if !ok || len(sys) != 2 {
		t.Fatalf("system: want two blocks, got %#v", body["system"])
	}
	if sys[0]["type"] != "text" || sys[0]["text"] != "agent prompt" {
		t.Errorf("system[0] = %#v, want the agent prompt", sys[0])
	}
	if sys[1]["type"] != "text" || sys[1]["text"] != "summary so far" {
		t.Errorf("system[1] = %#v, want the summary", sys[1])
	}
	for _, b := range sys {
		if _, ok := b["cache_control"]; ok {
			t.Errorf("cache disabled: block must carry no cache_control: %#v", b)
		}
	}
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("messages: want the 3 non-system turns, got %d", len(msgs))
	}
	for _, m := range msgs {
		if m["role"] == "system" {
			t.Errorf("a system message leaked into messages: %#v", m)
		}
	}
}

// The registry decorates the first (agent) block only, and the cache
// breakpoint sits on that stable block, never on the volatile summary.
func TestBuildRequestBody_SystemBlocksCacheAndRegistry(t *testing.T) {
	p := &Plugin{
		cache:   cacheConfig{Enabled: true, System: true, TTL: "5m"},
		logger:  silentLogger(),
		prompts: engine.NewPromptRegistry(),
	}
	p.prompts.Register("extra", 10, func() string { return "SECTION" })
	req := twoSystemRequest()
	body := p.buildRequestBody(req.Model, req.MaxTokens, req)

	sys, ok := body["system"].([]map[string]any)
	if !ok || len(sys) != 2 {
		t.Fatalf("system: want two blocks, got %#v", body["system"])
	}
	first, _ := sys[0]["text"].(string)
	if !strings.Contains(first, "agent prompt") || !strings.Contains(first, "SECTION") {
		t.Errorf("system[0] should be the decorated agent prompt, got %q", first)
	}
	if sys[1]["text"] != "summary so far" {
		t.Errorf("system[1] should be the undecorated summary, got %#v", sys[1]["text"])
	}
	if _, ok := sys[0]["cache_control"]; !ok {
		t.Error("system[0]: want the cache breakpoint on the stable agent block")
	}
	if _, ok := sys[1]["cache_control"]; ok {
		t.Error("system[1]: the volatile summary block must not carry the breakpoint")
	}
	if _, err := json.Marshal(body); err != nil {
		t.Fatalf("json.Marshal(body): %v", err)
	}
}

// One system message keeps the bare-string wire form it always had.
func TestBuildRequestBody_SingleSystemStaysString(t *testing.T) {
	p := &Plugin{logger: silentLogger()}
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-sonnet-4-6-20250514",
		MaxTokens: 1024,
		Messages: []events.Message{
			{Role: "system", Content: "only"},
			{Role: "user", Content: "hi"},
		},
	}
	body := p.buildRequestBody(req.Model, req.MaxTokens, req)
	if body["system"] != "only" {
		t.Fatalf("system: want bare string %q, got %#v", "only", body["system"])
	}
}
