package openai

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// Two system messages — the agent prompt and a mid-conversation summary — both
// reach the body, in order, at their own positions, and the prompt registry
// decorates only the first (GitHub #205).
func TestSystemMessages_AllKeptInOrder_RegistryOnFirstOnly(t *testing.T) {
	msgs := []events.Message{
		{Role: "system", Content: "agent prompt"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "system", Content: "summary so far"},
		{Role: "user", Content: "next"},
	}
	newPlugin := func() *Plugin {
		p := quietPlugin()
		p.prompts = engine.NewPromptRegistry()
		p.prompts.Register("extra", 10, func() string { return "SECTION" })
		return p
	}

	check := func(t *testing.T, got []map[string]any) {
		t.Helper()
		var sys []string
		var idx []int
		for i, m := range got {
			if m["role"] == "system" {
				c, _ := m["content"].(string)
				sys = append(sys, c)
				idx = append(idx, i)
			}
		}
		if len(sys) != 2 {
			t.Fatalf("expected 2 system messages, got %d: %#v", len(sys), got)
		}
		if !strings.Contains(sys[0], "agent prompt") || !strings.Contains(sys[0], "SECTION") {
			t.Errorf("first system message should be the decorated agent prompt, got %q", sys[0])
		}
		if sys[1] != "summary so far" {
			t.Errorf("second system message should be undecorated, got %q", sys[1])
		}
		if idx[0] != 0 || idx[1] != 3 {
			t.Errorf("system messages moved: positions %v, want [0 3]", idx)
		}
	}

	t.Run("chat", func(t *testing.T) {
		body := newPlugin().buildRequestBody("gpt-4o", 512, events.LLMRequest{
			SchemaVersion: events.LLMRequestVersion, Messages: msgs,
		})
		got, _ := body["messages"].([]map[string]any)
		check(t, got)
	})
	t.Run("responses", func(t *testing.T) {
		check(t, items(t, newPlugin().buildResponsesBody("gpt-5.1", 512, events.LLMRequest{
			SchemaVersion: events.LLMRequestVersion, Messages: msgs,
		})))
	})
}
