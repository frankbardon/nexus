package stopwords

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newTestPlugin(words []string, caseSensitive bool) (*Plugin, engine.EventBus) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.caseSensitive = caseSensitive
	for _, w := range words {
		p.addWord(w)
	}

	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(10))
	bus.Subscribe("before:io.output", p.handleBeforeOutput,
		engine.WithPriority(10))

	return p, bus
}

func TestStopWords_BlocksOutputWithBannedWord(t *testing.T) {
	_, bus := newTestPlugin([]string{"forbidden", "banned"}, false)

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This contains a forbidden word."}
	result, err := bus.EmitVetoable("before:io.output", &output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Vetoed {
		t.Fatal("expected veto for banned word in output")
	}
}

func TestStopWords_AllowsCleanOutput(t *testing.T) {
	_, bus := newTestPlugin([]string{"forbidden"}, false)

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This is perfectly fine."}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if result.Vetoed {
		t.Fatal("should not veto clean output")
	}
}

func TestStopWords_CaseInsensitive(t *testing.T) {
	_, bus := newTestPlugin([]string{"forbidden"}, false)

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This is FORBIDDEN content."}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("case-insensitive match should veto")
	}
}

func TestStopWords_CaseSensitive(t *testing.T) {
	_, bus := newTestPlugin([]string{"Forbidden"}, true)

	// Lowercase should NOT match.
	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This is forbidden content."}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if result.Vetoed {
		t.Fatal("case-sensitive: lowercase should not match uppercase word")
	}

	// Exact case SHOULD match.
	output2 := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This is Forbidden content."}
	result2, _ := bus.EmitVetoable("before:io.output", &output2)
	if !result2.Vetoed {
		t.Fatal("case-sensitive: exact case should match")
	}
}

func TestStopWords_BlocksInputWithBannedWord(t *testing.T) {
	_, bus := newTestPlugin([]string{"hack"}, false)

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "How do I hack into systems?"},
	},
	}
	result, _ := bus.EmitVetoable("before:llm.request", &req)
	if !result.Vetoed {
		t.Fatal("expected veto for banned word in user input")
	}
}

func TestStopWords_IgnoresSystemMessages(t *testing.T) {
	_, bus := newTestPlugin([]string{"hack"}, false)

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "system", Content: "hack prevention system"},
		{Role: "user", Content: "Hello, how are you?"},
	},
	}
	result, _ := bus.EmitVetoable("before:llm.request", &req)
	if result.Vetoed {
		t.Fatal("should not veto — banned word is in system message, not user")
	}
}

func TestStopWords_UnderscoreCompoundWord(t *testing.T) {
	_, bus := newTestPlugin([]string{"forbidden_word"}, false)

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This has FORBIDDEN_WORD in it."}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("underscore compound word should match as single token")
	}

	// Partial match should not trigger.
	output2 := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This is just forbidden."}
	result2, _ := bus.EmitVetoable("before:io.output", &output2)
	if result2.Vetoed {
		t.Fatal("partial match of compound word should not veto")
	}
}

func TestStopWords_EmptyWordList(t *testing.T) {
	_, bus := newTestPlugin(nil, false)

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "Anything goes."}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if result.Vetoed {
		t.Fatal("empty word list should never veto")
	}
}
