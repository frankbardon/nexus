package stopwords

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newLLMResponsePlugin(words ...string) (engine.EventBus, *[]events.LLMResponse) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.scanLLMResponses = true
	for _, w := range words {
		p.addWord(w)
	}

	bus.Subscribe("before:llm.response", p.handleBeforeLLMResponse,
		engine.WithPriority(10))

	published := &[]events.LLMResponse{}
	bus.Subscribe("llm.response", func(e engine.Event[any]) {
		if resp, ok := e.Payload.(events.LLMResponse); ok {
			*published = append(*published, resp)
		}
	})
	return bus, published
}

func TestStopWords_LLMResponse_SubstitutesOnMatch(t *testing.T) {
	bus, published := newLLMResponsePlugin("forbidden")

	out, substituted := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "here is the forbidden answer",
	})

	if !substituted {
		t.Fatal("a banned term must substitute the response")
	}
	if out.Content != "Content blocked: contains prohibited terms." {
		t.Fatalf("content = %q, want the gate's message", out.Content)
	}
	if out.FinishReason != engine.FinishReasonVetoed {
		t.Fatalf("finish reason = %q", out.FinishReason)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d llm.response events, want exactly 1", len(*published))
	}
}

// A banned response must not be able to execute the tools it asked for.
func TestStopWords_LLMResponse_DropsToolCalls(t *testing.T) {
	bus, published := newLLMResponsePlugin("forbidden")

	out, _ := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "running the forbidden command",
		ToolCalls:     []events.ToolCallRequest{{ID: "c1", Name: "shell"}},
	})

	if len(out.ToolCalls) != 0 {
		t.Fatalf("substituted response carries %d tool calls, want 0", len(out.ToolCalls))
	}
	if len((*published)[0].ToolCalls) != 0 {
		t.Fatal("published response still carries tool calls")
	}
}

func TestStopWords_LLMResponse_CleanPassesThrough(t *testing.T) {
	bus, _ := newLLMResponsePlugin("forbidden")

	out, substituted := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "a perfectly ordinary answer",
		ToolCalls:     []events.ToolCallRequest{{ID: "c1", Name: "shell"}},
	})

	if substituted {
		t.Fatal("clean response must not be substituted")
	}
	if len(out.ToolCalls) != 1 {
		t.Fatal("clean response lost its tool calls")
	}
}

func TestStopWords_LLMResponse_OffByDefault(t *testing.T) {
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

func TestStopWords_LLMResponse_DeclaredWhenEnabled(t *testing.T) {
	p := New().(*Plugin)
	p.scanLLMResponses = true
	found := false
	for _, sub := range p.Subscriptions() {
		if sub.EventType == "before:llm.response" {
			found = true
			if sub.Priority != 10 {
				t.Errorf("priority = %d, want 10 (final ban check, after redaction at 8)", sub.Priority)
			}
		}
	}
	if !found {
		t.Fatal("enabled gate must declare before:llm.response in Subscriptions()")
	}
}

// content_safety at 8 redacts before stop_words at 10 does the final ban
// check, so a redaction that removes a banned term must prevent the veto —
// the same composition the before:io.output pipeline guarantees.
func TestStopWords_LLMResponse_SeesEarlierGateMutation(t *testing.T) {
	bus, _ := newLLMResponsePlugin("secret")

	// Stand in for content_safety at priority 8.
	bus.Subscribe("before:llm.response", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		resp, ok := vp.Original.(*events.LLMResponse)
		if !ok {
			return
		}
		resp.Content = "the [REDACTED] is safe now"
	}, engine.WithPriority(8))

	out, substituted := engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "the secret is out",
	})

	if substituted {
		t.Fatal("stop_words vetoed text the earlier gate had already redacted")
	}
	if out.Content != "the [REDACTED] is safe now" {
		t.Fatalf("content = %q", out.Content)
	}
}
