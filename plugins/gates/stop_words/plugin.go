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
	scanStream       bool

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
	// Streaming follows scan_llm_responses by default. An operator who asked
	// for responses to be gated wants that to survive streaming; without it
	// the engine turns streaming off for this plugin instead (see
	// engine.StreamUnsafePlugins), which is safe but costs every turn its
	// time-to-first-token.
	p.scanStream = p.scanLLMResponses
	if v, ok := ctx.Config["scan_stream"].(bool); ok {
		p.scanStream = v
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
	if p.scanStream {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe(engine.EventBeforeStreamChunk, p.handleBeforeStreamChunk,
				engine.WithPriority(10), engine.WithSource(pluginID)),
		)
	}

	p.logger.Info("stop words gate initialized",
		"word_count", len(p.words),
		"case_sensitive", p.caseSensitive,
		"scan_llm_responses", p.scanLLMResponses,
		"scan_stream", p.scanStream)
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
	if p.scanStream {
		subs = append(subs, engine.EventSubscription{EventType: engine.EventBeforeStreamChunk, Priority: 10})
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

// handleBeforeStreamChunk is the streaming counterpart of
// handleBeforeLLMResponse. Without it, a veto at before:llm.response would
// land after every token of the banned text had already been rendered; here
// the text has not left the engine yet, so a block actually blocks.
//
// The gate matches whole tokens, which makes its hold rule exact rather than
// heuristic: the trailing run of word characters is withheld on every
// segment, because a trailing "FORBID" may yet become "FORBIDDEN". Once a
// separator arrives the run is complete, can be judged, and is released. The
// lag is one partial word.
//
// A stream cut here gets no replacement text. A stream has no substitute,
// only an end; the operator's message reaches the user from
// handleBeforeLLMResponse when the provider publishes the completed response.
func (p *Plugin) handleBeforeStreamChunk(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	seg, ok := vp.Original.(*events.StreamSegment)
	if !ok || seg.Content == "" {
		return
	}

	// Hold the incomplete trailing token before judging anything, so no word
	// is ever ruled on before the model has finished writing it.
	seg.HoldFrom(trailingWordStart(seg.Content))

	// Scan only the text that has not shipped, plus enough lookbehind for a
	// word that straddles the boundary. findStopWord tokenizes, so feeding it
	// the whole turn would re-report a word from an earlier segment that this
	// handler has already let through — and cutting the stream over bytes
	// already released helps nobody.
	full := seg.Full()
	from := seg.PendingOffset()
	for from > 0 && isWordByte(full[from-1]) {
		from--
	}
	found := p.findStopWord(full[from:])
	if found == "" {
		return
	}

	p.logger.Info("stop word found in llm.stream.chunk",
		"word", found, "turn_id", seg.TurnID, "request_id", seg.RequestID,
		"released_bytes", seg.PendingOffset())
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("Response contains prohibited term: %q", found),
	}
}

// trailingWordStart returns the offset at which the trailing run of word
// characters begins, or len(s) when s ends on a separator. It mirrors the
// tokenizer in findStopWord: the run it identifies is exactly the token that
// findStopWord would form if the stream stopped here, and that token is the
// one that may still be incomplete.
func trailingWordStart(s string) int {
	i := len(s)
	for i > 0 && isWordByte(s[i-1]) {
		i--
	}
	return i
}

// isWordByte reports whether b continues a token under findStopWord's
// splitting rule. ASCII-only, matching that tokenizer: a multi-byte rune is
// a separator to both, so the two stay consistent.
func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '\'' || b == '-' || b == '_':
		return true
	}
	return false
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
