package stopwords

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.stop_words"

// New creates a new stop words gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		words:   make(map[string]bool),
		message: "Content blocked: contains prohibited terms.",
	}
}

// Plugin gates before:llm.request and before:io.output events by checking for
// banned words. Programmatic string matching, not LLM-based.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	words            map[string]bool
	caseSensitive    bool
	message          string
	scanLLMResponses bool

	unsubs []func()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Stop Words Gate" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["case_sensitive"].(bool); ok {
		p.caseSensitive = v
	}
	if v, ok := ctx.Config["message"].(string); ok && v != "" {
		p.message = v
	}
	if v, ok := ctx.Config["scan_llm_responses"].(bool); ok {
		p.scanLLMResponses = v
	}

	// Load inline words.
	if v, ok := ctx.Config["words"]; ok {
		p.loadWordList(v)
	}

	// Load words from files.
	if v, ok := ctx.Config["word_files"]; ok {
		p.loadWordFiles(v)
	}

	if len(p.words) == 0 {
		p.logger.Warn("stop words gate has no words configured")
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:io.output", p.handleBeforeOutput,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)
	if p.scanLLMResponses {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe("before:llm.response", p.handleBeforeLLMResponse,
				engine.WithPriority(10), engine.WithSource(pluginID)),
		)
	}

	p.logger.Info("stop words gate initialized",
		"word_count", len(p.words),
		"case_sensitive", p.caseSensitive,
		"scan_llm_responses", p.scanLLMResponses)
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
	// before:llm.request priority 12 places stop_words after the cheap
	// structural gates (endless_loop=6, token_budget=7) and request-shape
	// mutators (rate_limiter=9, tool_filter=10), but before approval_policy
	// (=13) which may trigger HITL. before:io.output priority 10 keeps
	// stop_words as the final ban check after content_safety (=8) redaction
	// and json_schema (=9) format-retry. See #121.
	subs := []engine.EventSubscription{
		{EventType: "before:llm.request", Priority: 12},
		{EventType: "before:io.output", Priority: 10},
	}
	// before:llm.response priority 10 keeps the same relative order as the
	// output pipeline: content_safety (=8) redacts first, stop_words does the
	// final ban check on the text that will actually ship.
	if p.scanLLMResponses {
		subs = append(subs, engine.EventSubscription{EventType: "before:llm.response", Priority: 10})
	}
	return subs
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

	// Check user messages for stop words.
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		if found := p.findStopWord(msg.Content); found != "" {
			p.logger.Info("stop word found in input", "word", found)
			vp.Veto = engine.VetoResult{
				Vetoed: true,
				Reason: fmt.Sprintf("Input contains prohibited term: %q", found),
			}
			_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: p.message,
				Role: "system",
			})
			return
		}
	}
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

	// A gate-authored refusal from a before:llm.response veto has already been
	// adjudicated; re-judging it here would at best duplicate work and at worst
	// veto it into a blank, which is the outcome the substitution contract
	// exists to prevent.
	if engine.IsVetoSubstituted(output.Metadata) {
		return
	}

	if found := p.findStopWord(output.Content); found != "" {
		p.logger.Info("stop word found in output", "word", found)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Output contains prohibited term: %q", found),
		}
		_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: p.message,
			Role: "system",
		})
	}
}

// handleBeforeLLMResponse mirrors handleBeforeOutput against the model's raw
// response, before the agent loop consumes it. Only active when
// scan_llm_responses is enabled.
//
// Unlike before:io.output, a veto here also drops the response's tool calls
// (see engine.PublishLLMResponse), so a response that names a banned term
// while requesting a tool cannot execute that tool. Only Content is scanned;
// tool-call arguments belong to before:tool.invoke, which sees them parsed.
func (p *Plugin) handleBeforeLLMResponse(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	resp, ok := vp.Original.(*events.LLMResponse)
	if !ok {
		return
	}

	found := p.findStopWord(resp.Content)
	if found == "" {
		return
	}

	p.logger.Info("stop word found in llm.response",
		"word", found, "model", resp.Model, "request_id", resp.RequestID,
		"tool_calls_dropped", len(resp.ToolCalls))
	// Dictate the replacement text, then veto: PublishLLMResponse keeps the
	// Content set here, so the agent loop sees the operator's message rather
	// than the banned text or a bare veto reason.
	resp.Content = p.message
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("Response contains prohibited term: %q", found),
	}
}

// findStopWord scans text for any banned word. Returns the matched word or "".
func (p *Plugin) findStopWord(text string) string {
	if len(p.words) == 0 {
		return ""
	}

	check := text
	if !p.caseSensitive {
		check = strings.ToLower(text)
	}

	// Split on whitespace and punctuation boundaries for word-level matching.
	// Underscores are kept so compound words like "FORBIDDEN_WORD" match as one token.
	words := strings.FieldsFunc(check, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '\'' || r == '-' || r == '_')
	})

	for _, w := range words {
		if p.words[w] {
			return w
		}
	}
	return ""
}

func (p *Plugin) loadWordList(v any) {
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				p.addWord(s)
			}
		}
	case []string:
		for _, s := range list {
			if s != "" {
				p.addWord(s)
			}
		}
	}
}

func (p *Plugin) loadWordFiles(v any) {
	var files []string
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				files = append(files, s)
			}
		}
	case []string:
		files = list
	}

	for _, path := range files {
		path = engine.ExpandPath(path)
		f, err := os.Open(path)
		if err != nil {
			p.logger.Error("failed to open word file", "path", path, "error", err)
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			word := strings.TrimSpace(scanner.Text())
			if word != "" && !strings.HasPrefix(word, "#") {
				p.addWord(word)
			}
		}
		f.Close()
	}
}

func (p *Plugin) addWord(word string) {
	if !p.caseSensitive {
		word = strings.ToLower(word)
	}
	p.words[word] = true
}
