// Package summary_buffer provides an inline auto-compacting memory.history
// provider. It keeps the most recent N messages verbatim and replaces older
// ones with an LLM-generated summary, emitted in place as a system message
// at the head of the buffer.
//
// Unlike nexus.memory.compaction — which is an external coordinator that
// emits memory.compacted for a separate history plugin to adopt — this
// plugin serves memory.history directly, so the summarised view is what
// the ReAct agent sees on each request.
package summary_buffer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.memory.summary_buffer"
	pluginName = "Summary-Buffer Memory"
	version    = "0.1.0"

	// llmSource tags our own llm.request so handleLLMResponse can distinguish
	// the summary reply from normal agent responses.
	llmSource = "nexus.memory.summary_buffer"
)

// triggerStrategy mirrors the compaction plugin's triggers so configs can
// move between the two without relearning semantics.
type triggerStrategy string

const (
	triggerMessageCount  triggerStrategy = "message_count"
	triggerTokenEstimate triggerStrategy = "token_estimate"
	triggerTurnCount     triggerStrategy = "turn_count"
)

const defaultSummaryPrompt = `You are a context compaction assistant. Your job is to distill a conversation history into a concise summary that preserves all information needed for the assistant to continue working effectively.

Rules:
- Preserve the user's original goals, preferences, and constraints.
- Preserve all decisions made, actions taken, and their outcomes.
- Preserve any file paths, code snippets, variable names, or technical details that were referenced.
- Preserve the current state of any in-progress work.
- Preserve any errors encountered and how they were resolved (or not).
- Remove redundant back-and-forth, pleasantries, and repeated information.
- Remove verbose tool outputs that have already been processed — keep only the conclusions.
- Write in third person narrative form, not as a dialogue.

Output ONLY the compacted summary as a single block of text. Do not include any preamble, explanation, or metadata.`

// Plugin implements inline auto-compaction for the memory.history buffer.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	// Configuration.
	strategy         triggerStrategy
	messageThreshold int
	tokenThreshold   int
	turnThreshold    int
	charsPerToken    float64
	maxRecent        int
	modelRole        string
	model            string
	summaryPrompt    string

	mu              sync.RWMutex
	messages        []events.Message
	turnCount       int
	summarising     bool
	unsubs          []func()
	internalCallIDs map[string]struct{}
}

// New creates a new summary_buffer memory plugin.
func New() engine.Plugin {
	return &Plugin{
		strategy:         triggerMessageCount,
		messageThreshold: 50,
		tokenThreshold:   30000,
		turnThreshold:    10,
		charsPerToken:    4.0,
		maxRecent:        8,
		modelRole:        "quick",
		summaryPrompt:    defaultSummaryPrompt,
		internalCallIDs:  make(map[string]struct{}),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises both memory.history (direct history provider) and
// memory.compaction (since triggering-plus-summarising is this plugin's
// core behaviour). Running alongside nexus.memory.compaction is a
// misconfiguration — both would advertise memory.compaction and the engine
// warns on the ambiguity.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "memory.history",
			Description: "LLM-native conversation history with inline auto-compaction (recent N verbatim, older summarised).",
		},
		{
			Name:        "memory.compaction",
			Description: "Inline summarisation triggered by message count, token estimate, or turn count.",
		},
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 10},
		{EventType: "llm.response", Priority: 10},
		{EventType: "tool.invoke", Priority: 10},
		{EventType: "tool.result", Priority: 10},
		{EventType: "agent.turn.end", Priority: 5},
		{EventType: "memory.history.query", Priority: 50},
		{EventType: "memory.compact.request", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
		"memory.compaction.triggered",
		"memory.compacted",
		"io.status",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if s, ok := ctx.Config["strategy"].(string); ok {
		switch triggerStrategy(s) {
		case triggerMessageCount, triggerTokenEstimate, triggerTurnCount:
			p.strategy = triggerStrategy(s)
		default:
			return fmt.Errorf("summary_buffer: unknown strategy %q", s)
		}
	}

	p.messageThreshold = intConfig(ctx.Config, "message_threshold", p.messageThreshold)
	p.tokenThreshold = intConfig(ctx.Config, "token_threshold", p.tokenThreshold)
	p.turnThreshold = intConfig(ctx.Config, "turn_threshold", p.turnThreshold)
	p.maxRecent = intConfig(ctx.Config, "max_recent", p.maxRecent)
	if p.maxRecent < 0 {
		return fmt.Errorf("summary_buffer: max_recent must be >= 0, got %d", p.maxRecent)
	}
	if v, ok := ctx.Config["chars_per_token"].(float64); ok {
		p.charsPerToken = v
	}
	if mr, ok := ctx.Config["model_role"].(string); ok {
		p.modelRole = mr
	}
	if m, ok := ctx.Config["model"].(string); ok {
		p.model = m
	}

	if promptFile, ok := ctx.Config["prompt_file"].(string); ok && promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return fmt.Errorf("summary_buffer: failed to read prompt file %s: %w", promptFile, err)
		}
		p.summaryPrompt = string(data)
	} else if prompt, ok := ctx.Config["prompt"].(string); ok && prompt != "" {
		p.summaryPrompt = prompt
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.history.query", p.handleHistoryQuery, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compact.request", p.handleCompactRequest, engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	p.logger.Info("summary_buffer memory plugin initialized",
		"strategy", p.strategy,
		"message_threshold", p.messageThreshold,
		"token_threshold", p.tokenThreshold,
		"turn_threshold", p.turnThreshold,
		"max_recent", p.maxRecent,
		"model_role", p.modelRole,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// GetHistory returns a copy of the current buffer for in-process callers.
func (p *Plugin) GetHistory() []events.Message {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]events.Message, len(p.messages))
	copy(out, p.messages)
	return out
}

// ── Event handlers ──────────────────────────────────────────────────────

func (p *Plugin) handleInput(e engine.Event[any]) {
	input, ok := e.Payload.(events.UserInput)
	if !ok {
		return
	}
	p.append(events.Message{Role: "user", Content: input.Content})
}

func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	source, _ := resp.Metadata["_source"].(string)
	if source == llmSource {
		p.finishSummarisation(resp.Content)
		return
	}
	if source != "" {
		// Owned by another plugin (planner etc.) — don't record.
		return
	}
	p.append(events.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	})
}

func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.ParentCallID != "" {
		p.mu.Lock()
		p.internalCallIDs[tc.ID] = struct{}{}
		p.mu.Unlock()
	}
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	result, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}
	p.mu.Lock()
	if _, internal := p.internalCallIDs[result.ID]; internal {
		delete(p.internalCallIDs, result.ID)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	content := result.Output
	if result.Error != "" {
		content = "Error: " + result.Error
	}
	p.append(events.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: result.ID,
	})
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	if _, ok := e.Payload.(events.TurnInfo); !ok {
		return
	}
	p.mu.Lock()
	p.turnCount++
	p.mu.Unlock()
	p.checkTrigger()
}

func (p *Plugin) handleHistoryQuery(e engine.Event[any]) {
	q, ok := e.Payload.(*events.HistoryQuery)
	if !ok {
		return
	}
	p.mu.RLock()
	out := make([]events.Message, len(p.messages))
	copy(out, p.messages)
	p.mu.RUnlock()
	q.Messages = out
}

func (p *Plugin) handleCompactRequest(event engine.Event[any]) {
	reason := "external request"
	if m, ok := event.Payload.(map[string]any); ok {
		if r, ok := m["reason"].(string); ok {
			reason = r
		}
	}
	p.triggerSummarisation(reason)
}

// ── Core logic ──────────────────────────────────────────────────────────

func (p *Plugin) append(msg events.Message) {
	p.mu.Lock()
	p.messages = append(p.messages, msg)
	p.mu.Unlock()
	if p.strategy != triggerTurnCount {
		p.checkTrigger()
	}
}

func (p *Plugin) checkTrigger() {
	p.mu.RLock()
	if p.summarising {
		p.mu.RUnlock()
		return
	}
	triggered := false
	var reason string
	switch p.strategy {
	case triggerMessageCount:
		if len(p.messages) >= p.messageThreshold {
			triggered = true
			reason = fmt.Sprintf("message count %d >= threshold %d", len(p.messages), p.messageThreshold)
		}
	case triggerTokenEstimate:
		est := p.estimateTokensLocked()
		if est >= p.tokenThreshold {
			triggered = true
			reason = fmt.Sprintf("estimated tokens %d >= threshold %d", est, p.tokenThreshold)
		}
	case triggerTurnCount:
		if p.turnCount >= p.turnThreshold {
			triggered = true
			reason = fmt.Sprintf("turn count %d >= threshold %d", p.turnCount, p.turnThreshold)
		}
	}
	p.mu.RUnlock()
	if !triggered {
		return
	}
	p.triggerSummarisation(reason)
}

func (p *Plugin) estimateTokensLocked() int {
	total := 0
	for _, msg := range p.messages {
		total += utf8.RuneCountInString(msg.Content)
	}
	return int(float64(total) / p.charsPerToken)
}

// triggerSummarisation snapshots the buffer, decides which slice to summarise,
// and dispatches the LLM request. On empty or all-protected buffers it is a
// no-op.
func (p *Plugin) triggerSummarisation(reason string) {
	p.mu.Lock()
	if p.summarising {
		p.mu.Unlock()
		return
	}
	if len(p.messages) == 0 {
		p.mu.Unlock()
		return
	}

	protectCount := p.maxRecent
	if protectCount > len(p.messages) {
		protectCount = len(p.messages)
	}
	if len(p.messages)-protectCount <= 0 {
		p.mu.Unlock()
		return
	}

	// Safe split point: never split a tool_use/tool_result pair — if the
	// message immediately before the split is an assistant with pending
	// ToolCalls, include it in the summarised slice instead of the
	// protected tail. This keeps the recent-messages window internally
	// consistent for the next llm.request.
	splitIdx := len(p.messages) - protectCount
	splitIdx = safeSplit(p.messages, splitIdx)
	if splitIdx <= 0 {
		p.mu.Unlock()
		return
	}

	snapshot := make([]events.Message, splitIdx)
	copy(snapshot, p.messages[:splitIdx])
	msgCount := len(p.messages)
	p.summarising = true
	p.mu.Unlock()

	_ = p.bus.Emit("memory.compaction.triggered", events.CompactionTriggered{SchemaVersion: events.CompactionTriggeredVersion, Reason: reason,
		MessageCount: msgCount,
	})
	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "thinking", Detail: "Summarising context..."})

	var transcript strings.Builder
	for _, msg := range snapshot {
		role := msg.Role
		switch role {
		case "tool":
			role = "Tool Result"
		case "user":
			role = "User"
		case "assistant":
			role = "Assistant"
		case "system":
			role = "System"
		default:
			if len(role) > 0 {
				role = strings.ToUpper(role[:1]) + role[1:]
			}
		}
		// Include a compact representation of ToolCalls when present so the
		// summary retains what the assistant attempted.
		content := msg.Content
		if len(msg.ToolCalls) > 0 {
			args, _ := json.Marshal(msg.ToolCalls)
			if content != "" {
				content += "\n"
			}
			content += "[tool_calls: " + string(args) + "]"
		}
		fmt.Fprintf(&transcript, "[%s]: %s\n\n", role, content)
	}

	messages := []events.Message{
		{Role: "system", Content: p.summaryPrompt},
		{Role: "user", Content: "Here is the conversation to compact:\n\n" + transcript.String()},
	}

	_ = p.bus.Emit("llm.request", events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: p.modelRole,
		Model:    p.model,
		Messages: messages,
		Stream:   false,
		Metadata: map[string]any{
			"_source":      llmSource,
			"_prev_count":  msgCount,
			"_protect":     protectCount,
			"_summarising": true,
			"task_kind":    "summarise",
		},
		Tags: map[string]string{"source_plugin": pluginID},
	})
}

func (p *Plugin) finishSummarisation(summary string) {
	p.mu.Lock()
	prevCount := len(p.messages)

	protectCount := p.maxRecent
	if protectCount > len(p.messages) {
		protectCount = len(p.messages)
	}
	// Re-derive the split using the same safe-split rule; messages may have
	// grown while the LLM was working, which is fine — the summary still
	// covers the originally-snapshotted range and new messages land in the
	// protected tail by definition.
	splitIdx := len(p.messages) - protectCount
	splitIdx = safeSplit(p.messages, splitIdx)
	if splitIdx < 0 {
		splitIdx = 0
	}

	protected := make([]events.Message, len(p.messages)-splitIdx)
	copy(protected, p.messages[splitIdx:])

	compacted := make([]events.Message, 0, 1+len(protected))
	compacted = append(compacted, events.Message{
		Role:    "system",
		Content: "## Prior Context (Summarised)\n\n" + summary,
	})
	compacted = append(compacted, protected...)

	p.messages = compacted
	p.turnCount = 0
	p.summarising = false
	p.mu.Unlock()

	p.logger.Info("summary_buffer compaction complete",
		"prev_messages", prevCount,
		"new_messages", len(compacted),
	)

	_ = p.bus.Emit("memory.compacted", events.CompactionComplete{SchemaVersion: events.CompactionCompleteVersion, Messages: compacted,
		MessageCount: len(compacted),
		PrevCount:    prevCount,
	})
	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "idle", Detail: ""})
}

// safeSplit walks a proposed split index leftward until splitting at that
// position would not separate an assistant tool_use from its matching tool
// result(s). Returns 0 if no safe split exists (caller treats as no-op).
func safeSplit(msgs []events.Message, idx int) int {
	if idx <= 0 {
		return 0
	}
	if idx >= len(msgs) {
		return idx
	}
	// Collect outstanding tool_call IDs advertised by assistant messages in
	// the tail (idx..end). If any tail tool_use_id is owed a tool result
	// that currently sits before idx — shouldn't happen in practice since
	// results follow their invocation — we don't need to shift. The real
	// risk runs the other way: a tool result in the tail whose invocation
	// lives before idx. In that case, shift idx left past the assistant
	// whose ToolCalls contain that id.
	needID := map[string]bool{}
	for i := idx; i < len(msgs); i++ {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" {
			needID[msgs[i].ToolCallID] = true
		}
	}
	if len(needID) == 0 {
		return idx
	}
	for i := idx - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		produces := false
		for _, tc := range msgs[i].ToolCalls {
			if needID[tc.ID] {
				produces = true
				break
			}
		}
		if produces {
			// Shift idx to just before this assistant so both the invoke
			// and its result(s) land in the protected tail together.
			return i
		}
	}
	return idx
}

func intConfig(cfg map[string]any, key string, def int) int {
	if v, ok := cfg[key].(int); ok {
		return v
	}
	if v, ok := cfg[key].(float64); ok {
		return int(v)
	}
	return def
}
