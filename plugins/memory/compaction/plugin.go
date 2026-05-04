package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/memory/internal/approval"
)

const (
	pluginID   = "nexus.memory.compaction"
	pluginName = "Memory Compaction"
	version    = "0.2.0"

	llmSource = "nexus.memory.compaction"

	// Session workspace paths. `currentFile` holds the live tracked
	// messages and is rotated into `archiveDir` on each compaction so
	// the full pre-compaction transcript remains inspectable alongside
	// the summary that replaced it.
	currentFile = "plugins/" + pluginID + "/current.jsonl"
	archiveDir  = "plugins/" + pluginID + "/archive"
)

// triggerStrategy determines when compaction fires.
type triggerStrategy string

const (
	triggerMessageCount  triggerStrategy = "message_count"
	triggerTokenEstimate triggerStrategy = "token_estimate"
	triggerTurnCount     triggerStrategy = "turn_count"
)

const defaultCompactionPrompt = `You are a context compaction assistant. Your job is to distill a conversation history into a concise summary that preserves all information needed for the assistant to continue working effectively.

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

// Plugin implements LLM-based context compaction with backup and multiple trigger strategies.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	// Configuration.
	strategy         triggerStrategy
	messageThreshold int     // for message_count strategy
	tokenThreshold   int     // for token_estimate strategy
	turnThreshold    int     // for turn_count strategy
	charsPerToken    float64 // rough estimate for token counting
	modelRole        string
	model            string
	compactionPrompt string
	protectRecent    int  // number of recent messages to keep verbatim
	persist          bool // persist tracked messages + archives to session workspace

	// HITL approval gating for compaction commits. Off by default.
	approvalEnabled        bool
	approvalDefaultChoice  string
	approvalTimeout        time.Duration
	approvalSizeThreshold  int

	// Runtime state.
	mu                 sync.Mutex
	messages           []events.Message
	turnCount          int
	compacting         bool
	backupCounter      int
	currentArchiveStem string // filename stem of the in-progress archive cycle
	unsubs             []func()

	// internalCallIDs tracks ToolCall IDs marked ParentCallID!="" so their
	// result never reaches the archive. Same invariant as the conversation
	// plugin: the LLM must only see tool_use_ids it generated.
	internalCallIDs map[string]struct{}
}

// New creates a new memory compaction plugin.
func New() engine.Plugin {
	return &Plugin{
		strategy:         triggerMessageCount,
		messageThreshold: 50,
		tokenThreshold:   30000,
		turnThreshold:    10,
		charsPerToken:    4.0,
		modelRole:        "quick",
		protectRecent:    4,
		persist:          true,
		internalCallIDs:  make(map[string]struct{}),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises this plugin as a provider of "memory.compaction" —
// it reacts to context-window pressure by summarizing conversation history
// and emitting memory.compacted for history buffers to adopt.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "memory.compaction",
			Description: "LLM-driven conversation summarization that compresses history when the context window fills.",
		},
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 5},
		{EventType: "io.output", Priority: 5},
		{EventType: "tool.invoke", Priority: 5},
		{EventType: "tool.result", Priority: 5},
		{EventType: "agent.turn.end", Priority: 5},
		{EventType: "llm.response", Priority: 30},
		{EventType: "memory.compact.request", Priority: 10},
		// hitl.responded is dynamically subscribed by the approval helper
		// when require_approval.enabled and a commit is pending. Declared
		// here for introspection completeness.
		{EventType: "hitl.responded", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
		"memory.compaction.triggered",
		"memory.compacted",
		"thinking.step",
		"io.status",
		"hitl.requested",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Parse trigger strategy.
	if s, ok := ctx.Config["strategy"].(string); ok {
		switch triggerStrategy(s) {
		case triggerMessageCount:
			p.strategy = triggerMessageCount
		case triggerTokenEstimate:
			p.strategy = triggerTokenEstimate
		case triggerTurnCount:
			p.strategy = triggerTurnCount
		default:
			return fmt.Errorf("compaction: unknown strategy %q", s)
		}
	}

	// Parse thresholds.
	if v, ok := ctx.Config["message_threshold"].(int); ok {
		p.messageThreshold = v
	} else if v, ok := ctx.Config["message_threshold"].(float64); ok {
		p.messageThreshold = int(v)
	}

	if v, ok := ctx.Config["token_threshold"].(int); ok {
		p.tokenThreshold = v
	} else if v, ok := ctx.Config["token_threshold"].(float64); ok {
		p.tokenThreshold = int(v)
	}

	if v, ok := ctx.Config["turn_threshold"].(int); ok {
		p.turnThreshold = v
	} else if v, ok := ctx.Config["turn_threshold"].(float64); ok {
		p.turnThreshold = int(v)
	}

	if v, ok := ctx.Config["chars_per_token"].(float64); ok {
		p.charsPerToken = v
	}

	// Parse model configuration.
	if mr, ok := ctx.Config["model_role"].(string); ok {
		p.modelRole = mr
	}
	if m, ok := ctx.Config["model"].(string); ok {
		p.model = m
	}

	// Parse compaction prompt (file takes precedence over inline).
	p.compactionPrompt = defaultCompactionPrompt
	if promptFile, ok := ctx.Config["prompt_file"].(string); ok && promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return fmt.Errorf("compaction: failed to read prompt file %s: %w", promptFile, err)
		}
		p.compactionPrompt = string(data)
	} else if prompt, ok := ctx.Config["prompt"].(string); ok && prompt != "" {
		p.compactionPrompt = prompt
	}

	// Parse protect_recent.
	if v, ok := ctx.Config["protect_recent"].(int); ok {
		p.protectRecent = v
	} else if v, ok := ctx.Config["protect_recent"].(float64); ok {
		p.protectRecent = int(v)
	}

	// Parse persist toggle.
	if v, ok := ctx.Config["persist"].(bool); ok {
		p.persist = v
	}
	p.parseApprovalConfig(ctx.Config["require_approval"])

	// Subscribe to events.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse, engine.WithPriority(30), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compact.request", p.handleCompactRequest, engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	p.logger.Info("compaction plugin initialized",
		"strategy", p.strategy,
		"message_threshold", p.messageThreshold,
		"token_threshold", p.tokenThreshold,
		"turn_threshold", p.turnThreshold,
		"model_role", p.modelRole,
		"protect_recent", p.protectRecent,
	)
	return nil
}

func (p *Plugin) Ready() error {
	if !p.persist || p.session == nil {
		return nil
	}

	// Initialize archive counter from existing archive directory so that
	// rotated files keep a monotonic numbering across sessions.
	if files, err := p.session.ListFiles(archiveDir); err == nil {
		maxN := 0
		for _, name := range files {
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			// Expected form: NNN-YYYYMMDD-HHMMSS.jsonl
			if idx := strings.Index(name, "-"); idx > 0 {
				var n int
				if _, err := fmt.Sscanf(name[:idx], "%d", &n); err == nil && n > maxN {
					maxN = n
				}
			}
		}
		p.mu.Lock()
		p.backupCounter = maxN
		p.mu.Unlock()
	}

	// Preload any previously tracked messages so that a resumed session
	// continues from where compaction left off.
	if p.session.FileExists(currentFile) {
		data, err := p.session.ReadFile(currentFile)
		if err != nil {
			p.logger.Warn("failed to preload compaction current state", "error", err)
			return nil
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		p.mu.Lock()
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var msg events.Message
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				p.logger.Warn("skipping malformed compaction entry", "error", err)
				continue
			}
			p.messages = append(p.messages, msg)
		}
		count := len(p.messages)
		p.mu.Unlock()
		p.logger.Info("preloaded compaction state", "messages", count)
	}
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// ── Event Handlers ──────────────────────────────────────────────────────

func (p *Plugin) handleInput(e engine.Event[any]) {
	input, ok := e.Payload.(events.UserInput)
	if !ok {
		return
	}
	p.trackMessage(events.Message{Role: "user", Content: input.Content})
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	out, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	p.trackMessage(events.Message{Role: out.Role, Content: out.Content})
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
		return
	}
	content, _ := json.Marshal(tc.Arguments)
	p.trackMessage(events.Message{
		Role:       "tool_invoke",
		Content:    fmt.Sprintf("[%s] %s", tc.Name, string(content)),
		ToolCallID: tc.ID,
	})
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
	p.trackMessage(events.Message{
		Role:       "tool_result",
		Content:    fmt.Sprintf("[%s] %s", result.Name, content),
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

func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	source, _ := resp.Metadata["_source"].(string)
	if source != llmSource {
		return
	}
	p.finishCompaction(resp.Content)
}

// ── Core Logic ──────────────────────────────────────────────────────────

func (p *Plugin) trackMessage(msg events.Message) {
	p.mu.Lock()
	p.messages = append(p.messages, msg)
	p.mu.Unlock()

	// Append to the live tracked file so the on-disk state mirrors
	// what the plugin will consider for compaction.
	p.appendCurrent(msg)

	// Check non-turn triggers after each message.
	if p.strategy != triggerTurnCount {
		p.checkTrigger()
	}
}

// appendCurrent writes a single tracked message to the live on-disk log.
func (p *Plugin) appendCurrent(msg events.Message) {
	if !p.persist || p.session == nil {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		p.logger.Error("failed to marshal message for current log", "error", err)
		return
	}
	data = append(data, '\n')
	if err := p.session.AppendFile(currentFile, data); err != nil {
		p.logger.Error("failed to append to compaction current log", "error", err)
	}
}

// writeCurrent replaces the live on-disk log with the given messages.
// This is used after a compaction completes so the file contains the
// new compacted view (summary system message + protected recent messages).
func (p *Plugin) writeCurrent(messages []events.Message) {
	if !p.persist || p.session == nil {
		return
	}
	var buf []byte
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			p.logger.Error("failed to marshal compacted message", "error", err)
			continue
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	if err := p.session.WriteFile(currentFile, buf); err != nil {
		p.logger.Error("failed to rewrite compaction current log", "error", err)
	}
}

func (p *Plugin) checkTrigger() {
	p.mu.Lock()
	if p.compacting {
		p.mu.Unlock()
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

	if !triggered {
		p.mu.Unlock()
		return
	}

	p.compacting = true
	p.mu.Unlock()

	p.logger.Info("compaction triggered", "reason", reason)
	p.startCompaction(reason)
}

// handleCompactRequest handles external compaction requests (e.g. from context window gate).
func (p *Plugin) handleCompactRequest(event engine.Event[any]) {
	p.mu.Lock()
	if p.compacting {
		p.mu.Unlock()
		return
	}
	if len(p.messages) == 0 {
		p.mu.Unlock()
		return
	}
	p.compacting = true
	p.mu.Unlock()

	reason := "external request"
	if m, ok := event.Payload.(map[string]any); ok {
		if r, ok := m["reason"].(string); ok {
			reason = r
		}
	}

	p.logger.Info("compaction triggered by external request", "reason", reason)
	p.startCompaction(reason)
}

func (p *Plugin) estimateTokensLocked() int {
	total := 0
	for _, msg := range p.messages {
		total += utf8.RuneCountInString(msg.Content)
	}
	return int(float64(total) / p.charsPerToken)
}

func (p *Plugin) startCompaction(reason string) {
	p.mu.Lock()
	msgCount := len(p.messages)

	// Snapshot the messages to compact.
	snapshot := make([]events.Message, len(p.messages))
	copy(snapshot, p.messages)
	p.mu.Unlock()

	// Step 1: Archive the pre-compaction conversation. This snapshot is
	// what will be replaced once the summary is written in finishCompaction.
	backupPath := p.archiveSnapshot(snapshot, reason)

	// Emit trigger notification.
	_ = p.bus.Emit("memory.compaction.triggered", events.CompactionTriggered{
		Reason:       reason,
		MessageCount: msgCount,
		BackupPath:   backupPath,
	})

	_ = p.bus.Emit("thinking.step", events.ThinkingStep{
		Source:    pluginID,
		Content:   fmt.Sprintf("Context compaction triggered: %s. Backup saved to %s", reason, backupPath),
		Phase:     "compaction",
		Timestamp: time.Now(),
	})

	_ = p.bus.Emit("io.status", events.StatusUpdate{
		State:  "thinking",
		Detail: "Compacting context...",
	})

	// Step 2: Determine which messages to compact vs protect.
	protectCount := min(p.protectRecent, len(snapshot))
	compactSlice := snapshot[:len(snapshot)-protectCount]

	if len(compactSlice) == 0 {
		// Nothing to compact — all messages are protected.
		p.mu.Lock()
		p.compacting = false
		p.mu.Unlock()
		p.logger.Info("compaction skipped: all messages within protect_recent window")
		return
	}

	// Step 3: Build the conversation transcript for the LLM.
	var transcript strings.Builder
	for _, msg := range compactSlice {
		role := msg.Role
		switch role {
		case "tool_invoke":
			role = "Tool Call"
		case "tool_result":
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
		fmt.Fprintf(&transcript, "[%s]: %s\n\n", role, msg.Content)
	}

	// Step 4: Send LLM request for compaction.
	messages := []events.Message{
		{Role: "system", Content: p.compactionPrompt},
		{Role: "user", Content: "Here is the conversation to compact:\n\n" + transcript.String()},
	}

	_ = p.bus.Emit("llm.request", events.LLMRequest{
		Role:     p.modelRole,
		Model:    p.model,
		Messages: messages,
		Stream:   false,
		Metadata: map[string]any{
			"_source":     llmSource,
			"_backup":     backupPath,
			"_prev_count": msgCount,
		},
	})
}

func (p *Plugin) finishCompaction(summary string) {
	// Optional approval gate. Block before committing the summary back
	// into history so a rejection leaves the live message list intact and
	// the next attempt can retry.
	if p.shouldRequireApproval(summary) {
		if err := p.gateWithApproval(summary); err != nil {
			p.abortCompaction(summary, err)
			return
		}
	}

	p.mu.Lock()

	prevCount := len(p.messages)

	// Build compacted message list: summary + protected recent messages.
	protectCount := min(p.protectRecent, len(p.messages))
	protected := make([]events.Message, protectCount)
	copy(protected, p.messages[len(p.messages)-protectCount:])

	// Replace tracked messages with the compacted set.
	compacted := make([]events.Message, 0, 1+len(protected))
	compacted = append(compacted, events.Message{
		Role:    "system",
		Content: "## Prior Context (Compacted)\n\n" + summary,
	})
	compacted = append(compacted, protected...)

	p.messages = compacted
	p.turnCount = 0
	p.compacting = false
	p.mu.Unlock()

	// Persist the summary alongside the archived snapshot for this
	// compaction cycle, then rotate the live log so the on-disk state
	// now holds the compacted view.
	backupPath := p.latestArchivePath()
	p.writeSummarySidecar(backupPath, summary, prevCount, len(compacted))
	p.writeCurrent(compacted)

	p.logger.Info("compaction complete",
		"prev_messages", prevCount,
		"new_messages", len(compacted),
		"backup", backupPath,
	)

	_ = p.bus.Emit("thinking.step", events.ThinkingStep{
		Source:    pluginID,
		Content:   fmt.Sprintf("Context compacted: %d messages → %d messages", prevCount, len(compacted)),
		Phase:     "compaction",
		Timestamp: time.Now(),
	})

	// Emit compaction complete so conversation memory and agents replace their history.
	_ = p.bus.Emit("memory.compacted", events.CompactionComplete{
		Messages:     compacted,
		BackupPath:   backupPath,
		MessageCount: len(compacted),
		PrevCount:    prevCount,
	})

	_ = p.bus.Emit("io.status", events.StatusUpdate{
		State:  "idle",
		Detail: "",
	})
}

// ── Archive ─────────────────────────────────────────────────────────────

// archiveSnapshot writes the pre-compaction conversation to the archive
// directory along with a metadata sidecar. The on-disk current log is
// left untouched — it will be rotated by writeCurrent() once the summary
// is available in finishCompaction().
func (p *Plugin) archiveSnapshot(messages []events.Message, reason string) string {
	if !p.persist || p.session == nil {
		return ""
	}

	p.mu.Lock()
	p.backupCounter++
	counter := p.backupCounter
	// Remember the stem of the current archive cycle so the summary
	// sidecar written at the end can be matched to this snapshot.
	p.currentArchiveStem = fmt.Sprintf("%03d-%s", counter, time.Now().Format("20060102-150405"))
	stem := p.currentArchiveStem
	p.mu.Unlock()

	archivePath := fmt.Sprintf("%s/%s.jsonl", archiveDir, stem)

	var buf strings.Builder
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			p.logger.Error("failed to marshal message for archive", "error", err)
			continue
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}

	if err := p.session.WriteFile(archivePath, []byte(buf.String())); err != nil {
		p.logger.Error("failed to write compaction archive", "error", err, "path", archivePath)
		return ""
	}

	meta := map[string]any{
		"archive_number": counter,
		"message_count":  len(messages),
		"timestamp":      time.Now().Format(time.RFC3339),
		"strategy":       string(p.strategy),
		"reason":         reason,
	}
	metaData, _ := json.MarshalIndent(meta, "", "  ")
	metaPath := fmt.Sprintf("%s/%s.meta.json", archiveDir, stem)
	_ = p.session.WriteFile(metaPath, metaData)

	p.logger.Info("compaction archive saved", "path", archivePath, "messages", len(messages))
	return archivePath
}

// writeSummarySidecar writes the LLM-produced summary next to its archive
// snapshot so the two halves of the compaction (before → summary) can be
// inspected together.
func (p *Plugin) writeSummarySidecar(archivePath, summary string, prevCount, newCount int) {
	if !p.persist || p.session == nil {
		return
	}

	p.mu.Lock()
	stem := p.currentArchiveStem
	p.mu.Unlock()
	if stem == "" {
		return
	}

	summaryPath := fmt.Sprintf("%s/%s.summary.md", archiveDir, stem)
	header := fmt.Sprintf("# Compaction Summary\n\n- archive: `%s`\n- timestamp: %s\n- messages: %d → %d\n\n---\n\n",
		archivePath, time.Now().Format(time.RFC3339), prevCount, newCount)
	if err := p.session.WriteFile(summaryPath, []byte(header+summary+"\n")); err != nil {
		p.logger.Error("failed to write compaction summary sidecar", "error", err, "path", summaryPath)
	}
}

// latestArchivePath returns the most recently written archive snapshot path.
func (p *Plugin) latestArchivePath() string {
	if p.session == nil {
		return ""
	}
	files, err := p.session.ListFiles(archiveDir)
	if err != nil || len(files) == 0 {
		return ""
	}
	for i := len(files) - 1; i >= 0; i-- {
		if strings.HasSuffix(files[i], ".jsonl") {
			return fmt.Sprintf("%s/%s", archiveDir, files[i])
		}
	}
	return ""
}

// --- Approval gating ---

// parseApprovalConfig reads the optional `require_approval` block.
// Malformed values are treated as "off" — boot continues.
func (p *Plugin) parseApprovalConfig(raw any) {
	cfg, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if v, ok := cfg["enabled"].(bool); ok {
		p.approvalEnabled = v
	}
	if !p.approvalEnabled {
		return
	}
	if v, ok := cfg["default_choice"].(string); ok {
		p.approvalDefaultChoice = v
	}
	if v, ok := cfg["timeout"].(string); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			p.approvalTimeout = d
		}
	}
	match, _ := cfg["match"].(map[string]any)
	if v, ok := match["size_threshold_bytes"].(int); ok {
		p.approvalSizeThreshold = v
	} else if v, ok := match["size_threshold_bytes"].(float64); ok {
		p.approvalSizeThreshold = int(v)
	}
}

// shouldRequireApproval returns true when the configured match block
// matches the pending compaction summary. The only match key supported
// here is size_threshold_bytes — namespace and key glob don't apply to
// in-context compaction.
func (p *Plugin) shouldRequireApproval(summary string) bool {
	if !p.approvalEnabled {
		return false
	}
	if p.approvalSizeThreshold > 0 && len(summary) < p.approvalSizeThreshold {
		return false
	}
	return true
}

// gateWithApproval emits hitl.requested for the pending summary commit.
// Returns a non-nil error when the operator rejects.
func (p *Plugin) gateWithApproval(summary string) error {
	preview := summary
	truncated := false
	const maxPreview = 2000
	if len(preview) > maxPreview {
		preview = preview[:maxPreview]
		truncated = true
	}
	p.mu.Lock()
	prevCount := len(p.messages)
	p.mu.Unlock()
	actionRef := map[string]any{
		"summary":            preview,
		"size":               len(summary),
		"prev_message_count": prevCount,
		"protect_recent":     p.protectRecent,
	}
	if truncated {
		actionRef["_truncated"] = true
	}

	prompt := fmt.Sprintf("Commit compaction summary back into history? [%d bytes, %d → %d messages]",
		len(summary), prevCount, p.protectRecent+1)

	sessionID := ""
	if p.session != nil {
		sessionID = p.session.ID
	}

	_, allowed, err := approval.RequestApproval(context.Background(), approval.Request{
		Bus:             p.bus,
		Logger:          p.logger,
		PluginID:        pluginID,
		ActionKind:      "memory.compaction.commit",
		ActionRef:       actionRef,
		Prompt:          prompt,
		DefaultChoiceID: p.approvalDefaultChoice,
		Timeout:         p.approvalTimeout,
		SessionID:       sessionID,
	})
	if err != nil {
		p.logger.Error("compaction: approval request failed", "error", err)
		return fmt.Errorf("compaction: approval: %w", err)
	}
	if !allowed {
		return fmt.Errorf("compaction commit rejected by approval gate")
	}
	return nil
}

// abortCompaction releases the compacting flag and emits a status update
// without mutating the tracked-message list. Called when the approval
// gate rejects the summary.
func (p *Plugin) abortCompaction(summary string, gateErr error) {
	p.logger.Info("compaction commit rejected", "size", len(summary), "error", gateErr)

	p.mu.Lock()
	p.compacting = false
	p.mu.Unlock()

	_ = p.bus.Emit("thinking.step", events.ThinkingStep{
		Source:    pluginID,
		Content:   fmt.Sprintf("Context compaction commit rejected: %v", gateErr),
		Phase:     "compaction",
		Timestamp: time.Now(),
	})
	_ = p.bus.Emit("io.status", events.StatusUpdate{
		State:  "idle",
		Detail: "",
	})
}
