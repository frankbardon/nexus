package contentsafety

import (
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newTestPlugin(action string, checks ...string) (*Plugin, engine.EventBus) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.action = action
	p.checks = nil

	// Enable only requested checks.
	for _, name := range checks {
		for _, bc := range builtinChecks {
			if bc.name == name {
				re, _ := regexp.Compile(bc.pattern)
				p.checks = append(p.checks, check{name: bc.name, pattern: re})
				break
			}
		}
	}

	bus.Subscribe("before:io.output", p.handleBeforeOutput,
		engine.WithPriority(10))
	return p, bus
}

func TestContentSafety_DetectsEmail(t *testing.T) {
	_, bus := newTestPlugin("block", "email")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "Contact me at user@example.com for details.",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("expected veto for email")
	}
}

func TestContentSafety_DetectsSSN(t *testing.T) {
	_, bus := newTestPlugin("block", "ssn")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "The SSN is 123-45-6789.",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("expected veto for SSN")
	}
}

func TestContentSafety_DetectsAPIKey(t *testing.T) {
	_, bus := newTestPlugin("block", "api_key")

	tests := []struct {
		name    string
		content string
	}{
		{"AWS", "Key: AKIAIOSFODNN7EXAMPLE"},
		{"OpenAI", "sk-abcdefghijklmnopqrstuvwxyz1234567890"},
		{"GitHub", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: tt.content, Role: "assistant"}
			result, _ := bus.EmitVetoable("before:io.output", &output)
			if !result.Vetoed {
				t.Fatalf("expected veto for %s API key", tt.name)
			}
		})
	}
}

func TestContentSafety_DetectsPrivateKey(t *testing.T) {
	_, bus := newTestPlugin("block", "private_key")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("expected veto for private key")
	}
}

func TestContentSafety_DetectsInternalIP(t *testing.T) {
	_, bus := newTestPlugin("block", "internal_ip")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "The server is at 192.168.1.100.",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("expected veto for internal IP")
	}
}

func TestContentSafety_AllowsCleanContent(t *testing.T) {
	_, bus := newTestPlugin("block", "email", "ssn", "api_key", "private_key")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "This is perfectly safe content with no sensitive data.",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if result.Vetoed {
		t.Fatal("clean content should not be vetoed")
	}
}

func TestContentSafety_RedactMode(t *testing.T) {
	_, bus := newTestPlugin("redact", "email")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "Contact user@example.com for help.",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if result.Vetoed {
		t.Fatal("redact mode should not veto")
	}
	if !strings.Contains(output.Content, "[REDACTED]") {
		t.Fatalf("expected redacted content, got: %s", output.Content)
	}
	if strings.Contains(output.Content, "user@example.com") {
		t.Fatal("email should have been redacted")
	}
}

func TestContentSafety_SkipsNonAssistant(t *testing.T) {
	_, bus := newTestPlugin("block", "email")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "Contact user@example.com",
		Role: "system",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if result.Vetoed {
		t.Fatal("should not check non-assistant output")
	}
}

func TestContentSafety_DetectsPassword(t *testing.T) {
	_, bus := newTestPlugin("block", "password")

	output := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "password=mysecretpass123",
		Role: "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)
	if !result.Vetoed {
		t.Fatal("expected veto for password pattern")
	}
}

// Policy lock for issue #121. content_safety subscribes before:io.output at
// priority 8 (redact-mode mutator); a stop-words-style vetoer at priority 10
// must see the post-redaction content, so a banned token that only existed
// inside the redacted span never trips the veto. If the priorities ever
// drift back together — or the bus loses its stable tiebreak — this test
// flips.
func TestContentSafety_RedactBeatsBanCheckOnOutputGate(t *testing.T) {
	_, bus := newTestPluginAtRealPriority("redact", "email")

	// Simulated stop_words veto-only gate at the real downstream priority.
	bannedWordVetoed := false
	bus.Subscribe("before:io.output", func(e engine.Event[any]) {
		vp := e.Payload.(*engine.VetoablePayload)
		out := vp.Original.(*events.AgentOutput)
		if strings.Contains(out.Content, "badword") {
			bannedWordVetoed = true
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: "stop_word: badword"}
		}
	}, engine.WithPriority(10))

	output := events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Content:       "Contact me at badword@example.com for help.",
		Role:          "assistant",
	}
	result, _ := bus.EmitVetoable("before:io.output", &output)

	if bannedWordVetoed {
		t.Fatalf("downstream ban-check fired on un-redacted content — content_safety priority must run first; got %q", output.Content)
	}
	if result.Vetoed {
		t.Fatalf("redact mode + post-redaction-clean content should not veto, got reason %q", result.Reason)
	}
	if !strings.Contains(output.Content, "[REDACTED]") {
		t.Fatalf("expected redacted content, got: %s", output.Content)
	}
	if strings.Contains(output.Content, "badword@example.com") {
		t.Fatal("email containing the banned word should have been redacted")
	}
}

// newTestPluginAtRealPriority mirrors newTestPlugin but uses the priority
// the plugin actually advertises in Subscriptions(), so this test is
// sensitive to drift in the production wiring.
func newTestPluginAtRealPriority(action string, checks ...string) (*Plugin, engine.EventBus) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.action = action
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
	subs := p.Subscriptions()
	var outputPriority int
	for _, s := range subs {
		if s.EventType == "before:io.output" {
			outputPriority = s.Priority
			break
		}
	}
	bus.Subscribe("before:io.output", p.handleBeforeOutput, engine.WithPriority(outputPriority))
	return p, bus
}

func TestLuhnValid(t *testing.T) {
	tests := []struct {
		number string
		valid  bool
	}{
		{"4532015112830366", true},  // Valid Visa
		{"1234567890123456", false}, // Invalid
		{"", false},
	}

	for _, tt := range tests {
		got := luhnValid(tt.number)
		if got != tt.valid {
			t.Errorf("luhnValid(%q) = %v, want %v", tt.number, got, tt.valid)
		}
	}
}
