package jsonschema

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/gates/internal/retry"
	jsc "github.com/santhosh-tekuri/jsonschema/v6"
)

const pluginID = "nexus.gate.json_schema"

const defaultRetryPrompt = `Your response must be valid JSON matching the required schema.

Schema:
{schema}

Your previous response failed validation with this error:
{error}

Please provide a corrected response that is ONLY the valid JSON, with no additional text.`

// New creates a new JSON schema gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		maxRetries:  3,
		retryPrompt: defaultRetryPrompt,
	}
}

// Plugin gates before:io.output events by validating LLM output against a JSON schema.
// On failure, retries via LLM with correction instructions.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	compiled    *jsc.Schema // compiled JSON schema for validation
	schemaRaw   string      // raw schema JSON for prompt inclusion
	maxRetries  int
	retryPrompt string

	retrier            *retry.Handler
	unsubs             []func()
	lastNativeEnforced bool // true when most recent llm.response used native structured output
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "JSON Schema Gate" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	var schemaBytes []byte

	// Load schema from file or inline.
	if v, ok := ctx.Config["schema_file"].(string); ok && v != "" {
		v = engine.ExpandPath(v)
		data, err := os.ReadFile(v)
		if err != nil {
			return fmt.Errorf("json_schema gate: failed to read schema file %s: %w", v, err)
		}
		schemaBytes = data
	} else if v, ok := ctx.Config["schema"].(string); ok && v != "" {
		schemaBytes = []byte(v)
	} else if v, ok := ctx.Config["schema"].(map[string]any); ok {
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("json_schema gate: failed to marshal inline schema: %w", err)
		}
		schemaBytes = data
	}

	if schemaBytes == nil {
		return fmt.Errorf("json_schema gate: no schema configured (set 'schema' or 'schema_file')")
	}

	// Validate that the schema bytes are valid JSON before compiling.
	var raw any
	if err := json.Unmarshal(schemaBytes, &raw); err != nil {
		return fmt.Errorf("json_schema gate: invalid schema JSON: %w", err)
	}
	p.schemaRaw = string(schemaBytes)

	// Compile schema using jsonschema library.
	compiled, err := compileSchema(schemaBytes)
	if err != nil {
		return fmt.Errorf("json_schema gate: failed to compile schema: %w", err)
	}
	p.compiled = compiled

	if v, ok := ctx.Config["max_retries"].(int); ok && v >= 0 {
		p.maxRetries = v
	} else if v, ok := ctx.Config["max_retries"].(float64); ok && v >= 0 {
		p.maxRetries = int(v)
	}

	if v, ok := ctx.Config["retry_prompt"].(string); ok && v != "" {
		p.retryPrompt = v
	}

	p.retrier = retry.New(p.bus, p.logger, retry.Config{
		MaxRetries:  p.maxRetries,
		RetryPrompt: p.retryPrompt,
		Source:      pluginID,
	})

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:io.output", p.handleBeforeOutput,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse,
			engine.WithPriority(99), engine.WithSource(pluginID)),
	)

	p.logger.Info("json schema gate initialized",
		"max_retries", p.maxRetries)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	p.retrier.Shutdown()
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:io.output", Priority: 10},
		{EventType: "llm.response", Priority: 99},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"llm.request", "io.output"}
}

// handleLLMResponse tracks whether the most recent response used native structured output.
func (p *Plugin) handleLLMResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	enforced, _ := resp.Metadata["_structured_output"].(bool)
	p.lastNativeEnforced = enforced
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

	// Only validate assistant output.
	if output.Role != "assistant" {
		return
	}

	// Skip validation when provider enforced structured output natively.
	if p.lastNativeEnforced {
		p.logger.Debug("skipping json schema validation: native structured output enforced")
		p.lastNativeEnforced = false
		return
	}

	result := p.retrier.AttemptRetry(
		output.Content,
		nil, // no original messages needed — retry prompt is self-contained
		p.validate,
		map[string]string{"schema": p.schemaRaw},
	)

	if result.Valid {
		// Update output in-place with (possibly corrected) content.
		output.Content = result.Content
		return
	}

	// Retries exhausted — veto with error info.
	p.logger.Warn("json schema validation failed after retries",
		"error", result.Error, "retries", p.maxRetries)
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("JSON schema validation failed: %s", result.Error),
	}
	_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: fmt.Sprintf("Response failed JSON schema validation after %d retries: %s", p.maxRetries, result.Error),
		Role: "system",
	})
}

// validate checks content against the compiled JSON schema.
// Returns error description or "" if valid.
func (p *Plugin) validate(content string) string {
	// Try to extract JSON from the content (may be wrapped in markdown code blocks).
	jsonStr := extractJSON(content)

	inst, err := jsc.UnmarshalJSON(strings.NewReader(jsonStr))
	if err != nil {
		return fmt.Sprintf("invalid JSON: %v", err)
	}

	if err := p.compiled.Validate(inst); err != nil {
		return formatValidationError(err)
	}
	return ""
}

// formatValidationError extracts a readable message from jsonschema validation errors.
func formatValidationError(err error) string {
	if ve, ok := err.(*jsc.ValidationError); ok {
		return flattenValidationError(ve)
	}
	return err.Error()
}

// flattenValidationError walks the validation error tree and returns a concise summary.
func flattenValidationError(ve *jsc.ValidationError) string {
	if len(ve.Causes) == 0 {
		msg := ve.Error()
		// Strip the "jsonschema: " prefix for cleaner messages.
		if idx := strings.Index(msg, ": "); idx >= 0 {
			return msg
		}
		return msg
	}
	var parts []string
	for _, cause := range ve.Causes {
		parts = append(parts, flattenValidationError(cause))
	}
	return strings.Join(parts, "; ")
}

// compileSchema compiles JSON schema bytes into a validated, reusable schema.
func compileSchema(schemaBytes []byte) (*jsc.Schema, error) {
	c := jsc.NewCompiler()
	schema, err := jsc.UnmarshalJSON(strings.NewReader(string(schemaBytes)))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	if err := c.AddResource("schema.json", schema); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return c.Compile("schema.json")
}

// extractJSON attempts to extract JSON from content that may be wrapped in markdown.
func extractJSON(content string) string {
	content = strings.TrimSpace(content)

	// Try to extract from ```json ... ``` blocks.
	if idx := strings.Index(content, "```json"); idx >= 0 {
		start := idx + 7
		if end := strings.Index(content[start:], "```"); end >= 0 {
			return strings.TrimSpace(content[start : start+end])
		}
	}
	if idx := strings.Index(content, "```"); idx >= 0 {
		start := idx + 3
		// Skip optional language tag on same line.
		if nl := strings.Index(content[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(content[start:], "```"); end >= 0 {
			return strings.TrimSpace(content[start : start+end])
		}
	}

	return content
}
