package contentsafety

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.content_safety"

// Check represents a named content safety check with a compiled regex.
type check struct {
	name    string
	pattern *regexp.Regexp
}

// builtinChecks defines all available content safety checks.
var builtinChecks = []struct {
	name      string
	configKey string
	pattern   string
}{
	{"email", "check_pii_email", `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`},
	{"phone", "check_pii_phone", `\b(\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`},
	{"ssn", "check_pii_ssn", `\b\d{3}-\d{2}-\d{4}\b`},
	{"api_key", "check_secrets_api_key", `(AKIA[0-9A-Z]{16})|(sk-[a-zA-Z0-9]{20,})|(ghp_[a-zA-Z0-9]{36})|(xox[bporas]-[a-zA-Z0-9\-]+)`},
	{"private_key", "check_secrets_private_key", `-----BEGIN\s+(RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE\s+KEY-----`},
	{"password", "check_secrets_password", `(?i)(password|passwd|pwd|secret|token)\s*[:=]\s*\S+`},
	{"credit_card", "check_credit_card", `\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b`},
	{"internal_ip", "check_ip_internal", `\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`},
}

// New creates a new content safety gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		action:  "block",
		message: "Content blocked: contains sensitive information ({checks}).",
	}
}

// Plugin gates before:io.output events by running multiple programmatic checks
// for PII, secrets, and other sensitive content. Each check individually toggleable.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	checks  []check
	action  string // "block" or "redact"
	message string

	unsubs []func()
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Content Safety Gate" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["action"].(string); ok && (v == "block" || v == "redact") {
		p.action = v
	}
	if v, ok := ctx.Config["message"].(string); ok && v != "" {
		p.message = v
	}

	// Enable builtin checks (all on by default).
	for _, bc := range builtinChecks {
		enabled := true
		if v, ok := ctx.Config[bc.configKey].(bool); ok {
			enabled = v
		}
		if enabled {
			re, err := regexp.Compile(bc.pattern)
			if err != nil {
				p.logger.Error("failed to compile builtin pattern",
					"check", bc.name, "error", err)
				continue
			}
			p.checks = append(p.checks, check{name: bc.name, pattern: re})
		}
	}

	// Custom patterns.
	if v, ok := ctx.Config["custom_patterns"]; ok {
		p.loadCustomPatterns(v)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:io.output", p.handleBeforeOutput,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	checkNames := make([]string, len(p.checks))
	for i, c := range p.checks {
		checkNames[i] = c.name
	}
	p.logger.Info("content safety gate initialized",
		"checks", strings.Join(checkNames, ","),
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
		{EventType: "before:io.output", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"io.output"}
}

func (p *Plugin) handleBeforeOutput(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	output, ok := vp.Original.(*events.AgentOutput)
	if !ok {
		return
	}

	if output.Role != "assistant" {
		return
	}

	// Run all checks, collect matches.
	var matched []string
	for _, c := range p.checks {
		if c.pattern.MatchString(output.Content) {
			matched = append(matched, c.name)
		}
	}

	if len(matched) == 0 {
		return
	}

	checkList := strings.Join(matched, ", ")
	p.logger.Warn("content safety checks triggered",
		"checks", checkList, "action", p.action)

	switch p.action {
	case "block":
		msg := strings.ReplaceAll(p.message, "{checks}", checkList)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Content safety: %s", checkList),
		}
		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: msg,
			Role:    "system",
		})

	case "redact":
		content := output.Content
		for _, c := range p.checks {
			for _, name := range matched {
				if c.name == name {
					content = c.pattern.ReplaceAllString(content, "[REDACTED]")
				}
			}
		}
		output.Content = content
		// Don't veto — modified output proceeds.
	}
}

func (p *Plugin) loadCustomPatterns(v any) {
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
			p.logger.Error("failed to compile custom pattern",
				"pattern", pat, "error", err)
			continue
		}
		p.checks = append(p.checks, check{name: "custom:" + pat, pattern: re})
	}
}

// luhnValid performs Luhn checksum validation on a digit string.
// Not currently wired in (credit card check uses regex only),
// available for future enhancement.
func luhnValid(number string) bool {
	// Strip spaces and dashes.
	clean := strings.NewReplacer(" ", "", "-", "").Replace(number)
	if len(clean) < 13 || len(clean) > 19 {
		return false
	}

	sum := 0
	alt := false
	for i := len(clean) - 1; i >= 0; i-- {
		d := int(clean[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}
