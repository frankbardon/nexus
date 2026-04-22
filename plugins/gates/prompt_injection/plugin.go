package promptinjection

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.prompt_injection"

// Default injection patterns.
var defaultPatterns = []string{
	`(?i)ignore\s+(all\s+)?(previous|prior|above)\s+instructions`,
	`(?i)you\s+are\s+now\s+`,
	`(?i)system\s*prompt\s*:`,
	`(?i)disregard\s+(all\s+)?(previous|prior|earlier)`,
	`(?i)new\s+instructions?\s*:`,
	`(?i)override\s+(system|safety|previous)`,
	`\[INST\]`,
	`<\|im_start\|>`,
	`<\|system\|>`,
	`(?i)pretend\s+you\s+are`,
	`(?i)act\s+as\s+if\s+you\s+(have\s+)?no\s+(restrictions|rules|guidelines)`,
}

// New creates a new prompt injection gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		action:  "block",
		message: "Input blocked: potential prompt injection detected.",
	}
}

// Plugin gates before:llm.request events by scanning user messages for
// common prompt injection patterns. Programmatic regex matching.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	patterns []*regexp.Regexp
	action   string // "block" or "warn"
	message  string

	unsubs []func()
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Prompt Injection Gate" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["action"].(string); ok && (v == "block" || v == "warn") {
		p.action = v
	}
	if v, ok := ctx.Config["message"].(string); ok && v != "" {
		p.message = v
	}

	// Compile default patterns.
	for _, pat := range defaultPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			p.logger.Error("failed to compile default pattern", "pattern", pat, "error", err)
			continue
		}
		p.patterns = append(p.patterns, re)
	}

	// Load inline patterns.
	if v, ok := ctx.Config["patterns"]; ok {
		p.loadPatterns(v)
	}

	// Load patterns from file.
	if v, ok := ctx.Config["patterns_file"].(string); ok && v != "" {
		p.loadPatternsFile(v)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	p.logger.Info("prompt injection gate initialized",
		"pattern_count", len(p.patterns),
		"action", p.action)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:llm.request", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"io.output"}
}

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Only scan user messages.
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		if matched := p.findInjection(msg.Content); matched != "" {
			p.logger.Warn("prompt injection detected",
				"pattern", matched, "action", p.action)

			if p.action == "block" {
				vp.Veto = engine.VetoResult{
					Vetoed: true,
					Reason: fmt.Sprintf("Prompt injection detected: %s", matched),
				}
				_ = p.bus.Emit("io.output", events.AgentOutput{
					Content: p.message,
					Role:    "system",
				})
			} else {
				// Warn mode — emit warning but don't veto.
				_ = p.bus.Emit("io.output", events.AgentOutput{
					Content: "Warning: potential prompt injection detected in input.",
					Role:    "system",
				})
			}
			return
		}
	}
}

func (p *Plugin) findInjection(text string) string {
	for _, re := range p.patterns {
		if re.MatchString(text) {
			return re.String()
		}
	}
	return ""
}

func (p *Plugin) loadPatterns(v any) {
	var raw []string
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	case []string:
		raw = list
	}

	for _, pat := range raw {
		re, err := regexp.Compile(pat)
		if err != nil {
			p.logger.Error("failed to compile pattern", "pattern", pat, "error", err)
			continue
		}
		p.patterns = append(p.patterns, re)
	}
}

func (p *Plugin) loadPatternsFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		p.logger.Error("failed to open patterns file", "path", path, "error", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			p.logger.Error("failed to compile pattern from file",
				"pattern", line, "path", path, "error", err)
			continue
		}
		p.patterns = append(p.patterns, re)
	}
}
