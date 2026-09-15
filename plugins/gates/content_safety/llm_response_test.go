package contentsafety

import (
	"log/slog"
	"regexp"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newLLMResponsePlugin builds a gate wired to before:llm.response the way
// Init does when scan_llm_responses is enabled, and returns the bus plus a
// pointer to every llm.response published on it.
func newLLMResponsePlugin(action string, checks ...string) (engine.EventBus, *[]events.LLMResponse) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.action = action
	p.scanLLMResponses = true
	p.checks = nil

	for _, name := range checks {
		for _, bc := range builtinChecks {
			if bc.name == name {
				re, _ := regexp.Compile(bc.pattern)
				p.checks = append(p.checks, check{name: bc.name, pattern: re})
				break
			}
		}
	}

	bus.Subscribe("before:llm.response", p.handleBeforeLLMResponse,
		engine.WithPriority(8))

	published := &[]events.LLMResponse{}
	bus.Subscribe("llm.response", func(e engine.Event[any]) {
		if resp, ok := e.Payload.(events.LLMResponse); ok {
			*published = append(*published, resp)
		}
	})
	return bus, published
}

// Block mode replaces the flagged response with the gate's message rather
// than the raw veto reason.
func TestContentSafety_LLMResponse_BlockSubstitutes(t *testing.T) {
	bus, published := newLLMResponsePlugin("block", "ssn")

	out, substituted := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "The SSN is 123-45-6789.",
	})

	if !substituted {
		t.Fatal("block mode must substitute the response")
	}
	if out.Content == "The SSN is 123-45-6789." {
		t.Fatal("flagged content reached the agent loop")
	}
	if out.FinishReason != engine.FinishReasonVetoed {
		t.Fatalf("finish reason = %q, want %q", out.FinishReason, engine.FinishReasonVetoed)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d llm.response events, want exactly 1", len(*published))
	}
}

// The reason this hook exists: before:io.output cannot stop a flagged
// response from executing its tool calls, because the loop has already run
// them by then. Here the substitute carries none.
func TestContentSafety_LLMResponse_BlockDropsToolCalls(t *testing.T) {
	bus, published := newLLMResponsePlugin("block", "ssn")

	out, _ := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "Filing 123-45-6789 now.",
		ToolCalls: []events.ToolCallRequest{
			{ID: "c1", Name: "file_write", Arguments: `{"path":"/tmp/leak"}`},
		},
	})

	if len(out.ToolCalls) != 0 {
		t.Fatalf("substituted response carries %d tool calls, want 0", len(out.ToolCalls))
	}
	if len((*published)[0].ToolCalls) != 0 {
		t.Fatal("published response still carries tool calls")
	}
}

// Redact mode is the mutate-without-veto path: the response proceeds, its
// tool calls intact, with matches scrubbed.
func TestContentSafety_LLMResponse_RedactKeepsToolCalls(t *testing.T) {
	bus, published := newLLMResponsePlugin("redact", "ssn")

	out, substituted := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "The SSN is 123-45-6789.",
		ToolCalls: []events.ToolCallRequest{
			{ID: "c1", Name: "file_read", Arguments: `{"path":"/tmp/x"}`},
		},
	})

	if substituted {
		t.Fatal("redact mode must not substitute — it mutates and proceeds")
	}
	if out.Content != "The SSN is [REDACTED]." {
		t.Fatalf("content = %q, want the SSN redacted", out.Content)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("redact mode dropped tool calls (%d), want them preserved", len(out.ToolCalls))
	}
	if (*published)[0].Content != "The SSN is [REDACTED]." {
		t.Fatalf("published content = %q", (*published)[0].Content)
	}
}

// A clean response must pass through untouched.
func TestContentSafety_LLMResponse_CleanPassesThrough(t *testing.T) {
	bus, published := newLLMResponsePlugin("block", "ssn")

	out, substituted := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "Nothing sensitive here.",
		ToolCalls:     []events.ToolCallRequest{{ID: "c1", Name: "file_read"}},
	})

	if substituted {
		t.Fatal("clean response must not be substituted")
	}
	if out.Content != "Nothing sensitive here." || len(out.ToolCalls) != 1 {
		t.Fatalf("clean response was altered: %+v", out)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d llm.response events, want exactly 1", len(*published))
	}
}

// Off by default: without scan_llm_responses the gate never subscribes.
func TestContentSafety_LLMResponse_OffByDefault(t *testing.T) {
	p := New().(*Plugin)
	if p.scanLLMResponses {
		t.Fatal("scan_llm_responses must default to false")
	}
	for _, sub := range p.Subscriptions() {
		if sub.EventType == "before:llm.response" {
			t.Fatal("gate declares before:llm.response without the opt-in")
		}
	}
}

func TestContentSafety_LLMResponse_DeclaredWhenEnabled(t *testing.T) {
	p := New().(*Plugin)
	p.scanLLMResponses = true
	found := false
	for _, sub := range p.Subscriptions() {
		if sub.EventType == "before:llm.response" {
			found = true
			if sub.Priority != 8 {
				t.Errorf("priority = %d, want 8 (redaction runs before ban checks)", sub.Priority)
			}
		}
	}
	if !found {
		t.Fatal("enabled gate must declare before:llm.response in Subscriptions()")
	}
}
