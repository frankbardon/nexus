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

// A request with no system message still carries the prompt registry's
// sections: they become a leading system message on chat and a leading system
// Item on Responses, the way the Anthropic and Gemini providers make the
// registry output the whole system prompt (GitHub #216). An empty registry
// synthesizes nothing.
func TestSystemMessages_NoSystemMessage_RegistryStillApplied(t *testing.T) {
	msgs := []events.Message{
		{Role: "user", Content: "hi"},
	}
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: msgs}

	withSection := func() *Plugin {
		p := quietPlugin()
		p.prompts = engine.NewPromptRegistry()
		p.prompts.Register("extra", 10, func() string { return "SECTION" })
		return p
	}
	emptyRegistry := func() *Plugin {
		p := quietPlugin()
		p.prompts = engine.NewPromptRegistry()
		p.prompts.Register("silent", 10, func() string { return "" })
		return p
	}

	chat := func(p *Plugin) []map[string]any {
		got, _ := p.buildRequestBody("gpt-4o", 512, req)["messages"].([]map[string]any)
		return got
	}
	responses := func(p *Plugin) []map[string]any {
		return items(t, p.buildResponsesBody("gpt-5.1", 512, req))
	}

	for name, build := range map[string]func(*Plugin) []map[string]any{"chat": chat, "responses": responses} {
		t.Run(name, func(t *testing.T) {
			got := build(withSection())
			if len(got) != 2 {
				t.Fatalf("expected a synthesized system message plus the user turn, got %#v", got)
			}
			if got[0]["role"] != "system" {
				t.Fatalf("first message should be the synthesized system message, got %#v", got[0])
			}
			c, _ := got[0]["content"].(string)
			if !strings.Contains(c, "SECTION") {
				t.Errorf("synthesized system message lacks the registry section: %q", c)
			}
			if strings.Contains(c, "system_instructions") {
				t.Errorf("an empty base prompt must not be wrapped: %q", c)
			}
			if got[1]["role"] != "user" {
				t.Errorf("user turn moved: %#v", got[1])
			}

			got = build(emptyRegistry())
			if len(got) != 1 || got[0]["role"] != "user" {
				t.Errorf("an empty registry must not synthesize a system message, got %#v", got)
			}
		})
	}
}
