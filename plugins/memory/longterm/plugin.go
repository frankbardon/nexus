package longterm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.memory.longterm"
	pluginName = "Long-Term Memory"
	version    = "0.1.0"
)

// Plugin provides cross-session memory persistence via file-per-entry storage
// with YAML frontmatter and markdown content.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	prompts *engine.PromptRegistry

	mu     sync.RWMutex
	index  []events.LongTermMemoryIndex
	unsubs []func()

	// Config.
	paths                []string // resolved memory directories (1 or 2 for "both" scope)
	writePath            string   // directory for writes
	scope                string   // "agent", "global", or "both"
	autoLoad             bool
	autoSaveInstructions string
	agentID              string
}

// New creates a new long-term memory plugin.
func New() engine.Plugin {
	return &Plugin{
		scope:    "agent",
		autoLoad: true,
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises this plugin as a provider of "memory.longterm" —
// cross-session persistent memory entries (YAML frontmatter + markdown, one
// file per entry) that survive session boundaries.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "memory.longterm",
			Description: "Cross-session long-term memory store (file-per-entry, YAML + markdown).",
		},
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "memory.longterm.store", Priority: 50},
		{EventType: "memory.longterm.read", Priority: 50},
		{EventType: "memory.longterm.delete", Priority: 50},
		{EventType: "memory.longterm.list", Priority: 50},
		{EventType: "tool.invoke", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"memory.longterm.loaded",
		"memory.longterm.stored",
		"memory.longterm.result",
		"memory.longterm.deleted",
		"memory.longterm.list.result",
		"tool.register",
		"before:tool.result",
		"tool.result",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.prompts = ctx.Prompts

	// Parse config.
	if v, ok := ctx.Config["scope"].(string); ok {
		p.scope = v
	}
	if v, ok := ctx.Config["auto_load"].(bool); ok {
		p.autoLoad = v
	}
	if v, ok := ctx.Config["auto_save_instructions"].(string); ok {
		p.autoSaveInstructions = v
	}
	if v, ok := ctx.Config["agent_id"].(string); ok {
		p.agentID = v
	}

	// Resolve storage paths based on scope.
	basePath := "~/.nexus/memory/"
	if v, ok := ctx.Config["path"].(string); ok {
		basePath = v
	}
	basePath = engine.ExpandPath(basePath)

	switch p.scope {
	case "global":
		p.paths = []string{basePath}
		p.writePath = basePath
	case "both":
		agentPath := p.agentMemoryPath()
		p.paths = []string{basePath, agentPath}
		p.writePath = agentPath
	default: // "agent"
		if p.agentID != "" {
			agentPath := p.agentMemoryPath()
			p.paths = []string{agentPath}
			p.writePath = agentPath
		} else {
			p.paths = []string{basePath}
			p.writePath = basePath
		}
	}

	// Ensure write directory exists.
	if err := os.MkdirAll(p.writePath, 0o755); err != nil {
		return fmt.Errorf("longterm: creating memory directory %s: %w", p.writePath, err)
	}

	// Subscribe to events.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("memory.longterm.store", p.handleStore, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.longterm.read", p.handleRead, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.longterm.delete", p.handleDelete, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.longterm.list", p.handleList, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	// Register system prompt section.
	if p.autoLoad && p.prompts != nil {
		p.prompts.Register(pluginID, 40, p.buildPromptSection)
	}

	p.logger.Info("long-term memory plugin initialized",
		"scope", p.scope,
		"write_path", p.writePath,
		"paths", p.paths,
	)
	return nil
}

func (p *Plugin) Ready() error {
	// Register LLM tools.
	tools := []events.ToolDef{
		{
			Name:        "memory_write",
			Description: "Create or update a long-term memory entry that persists across sessions.",
			Class:       "memory",
			Subclass:    "manage",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{
						"type":        "string",
						"description": "Unique identifier for the memory (alphanumeric, hyphens, underscores)",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "The memory content (markdown)",
					},
					"tags": map[string]any{
						"type":                 "object",
						"description":          "Optional key-value tags for categorization and filtering",
						"additionalProperties": map[string]any{"type": "string"},
					},
				},
				"required": []string{"key", "content"},
			},
		},
		{
			Name:        "memory_read",
			Description: "Read the full content of a long-term memory entry by key.",
			Class:       "memory",
			Subclass:    "manage",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{
						"type":        "string",
						"description": "The memory key to read",
					},
				},
				"required": []string{"key"},
			},
		},
		{
			Name:        "memory_list",
			Description: "List all long-term memories, optionally filtered by tags.",
			Class:       "memory",
			Subclass:    "manage",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tags": map[string]any{
						"type":                 "object",
						"description":          "Optional tag filters (AND semantics — all must match)",
						"additionalProperties": map[string]any{"type": "string"},
					},
				},
			},
		},
		{
			Name:        "memory_delete",
			Description: "Delete a long-term memory entry by key.",
			Class:       "memory",
			Subclass:    "manage",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{
						"type":        "string",
						"description": "The memory key to delete",
					},
				},
				"required": []string{"key"},
			},
		},
	}

	for _, tool := range tools {
		_ = p.bus.Emit("tool.register", tool)
	}

	// Load index and emit loaded event.
	if err := p.loadIndex(); err != nil {
		p.logger.Warn("failed to load memory index", "error", err)
	}

	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.prompts != nil {
		p.prompts.Unregister(pluginID)
	}
	return nil
}

// --- Index loading ---

func (p *Plugin) loadIndex() error {
	var merged []events.LongTermMemoryIndex
	seen := make(map[string]bool)

	// Read paths in reverse so later paths (agent-scoped) win on conflict.
	for i := len(p.paths) - 1; i >= 0; i-- {
		entries, err := list(p.paths[i], nil)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !seen[e.Key] {
				seen[e.Key] = true
				merged = append(merged, e)
			}
		}
	}

	p.mu.Lock()
	p.index = merged
	p.mu.Unlock()

	_ = p.bus.Emit("memory.longterm.loaded", events.LongTermMemoryLoaded{
		Entries: merged,
		Scope:   p.scope,
	})

	p.logger.Info("memory index loaded", "count", len(merged))
	return nil
}

// --- System prompt injection ---

func (p *Plugin) buildPromptSection() string {
	p.mu.RLock()
	idx := p.index
	p.mu.RUnlock()

	if len(idx) == 0 && p.autoSaveInstructions == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Long-Term Memory\n\n")

	if len(idx) > 0 {
		b.WriteString("You have access to memories from previous sessions. Use memory_read to retrieve full details.\n\n")
		b.WriteString("Available memories:\n")
		for _, e := range idx {
			b.WriteString("- ")
			b.WriteString(e.Key)
			if len(e.Tags) > 0 {
				b.WriteString(" [")
				first := true
				for k, v := range e.Tags {
					if !first {
						b.WriteString(", ")
					}
					b.WriteString(k)
					b.WriteString(":")
					b.WriteString(v)
					first = false
				}
				b.WriteString("]")
			}
			b.WriteString(" — ")
			b.WriteString(e.Preview)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("No memories stored yet. Use memory_write to save information for future sessions.\n")
	}

	if p.autoSaveInstructions != "" {
		b.WriteString("\n")
		b.WriteString(p.autoSaveInstructions)
		b.WriteString("\n")
	}

	return b.String()
}

// --- Event handlers (bus events) ---

func (p *Plugin) handleStore(e engine.Event[any]) {
	req, ok := e.Payload.(events.LongTermMemoryStoreRequest)
	if !ok {
		return
	}
	p.doStore(req)
}

func (p *Plugin) handleRead(e engine.Event[any]) {
	req, ok := e.Payload.(events.LongTermMemoryReadRequest)
	if !ok {
		return
	}
	p.doRead(req.Key)
}

func (p *Plugin) handleDelete(e engine.Event[any]) {
	req, ok := e.Payload.(events.LongTermMemoryDeleteRequest)
	if !ok {
		return
	}
	p.doDelete(req.Key)
}

func (p *Plugin) handleList(e engine.Event[any]) {
	req, ok := e.Payload.(events.LongTermMemoryQuery)
	if !ok {
		return
	}
	p.doList(req.Tags)
}

// --- Tool invocation handler ---

func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}

	switch tc.Name {
	case "memory_write":
		p.handleMemoryWrite(tc)
	case "memory_read":
		p.handleMemoryRead(tc)
	case "memory_list":
		p.handleMemoryList(tc)
	case "memory_delete":
		p.handleMemoryDelete(tc)
	}
}

func (p *Plugin) handleMemoryWrite(tc events.ToolCall) {
	key, _ := tc.Arguments["key"].(string)
	content, _ := tc.Arguments["content"].(string)

	if key == "" || content == "" {
		p.emitToolResult(tc, "", "key and content are required")
		return
	}

	tags := extractStringMap(tc.Arguments["tags"])

	req := events.LongTermMemoryStoreRequest{
		Key:     key,
		Content: content,
		Tags:    tags,
	}

	if err := p.doStore(req); err != nil {
		p.emitToolResult(tc, "", err.Error())
		return
	}

	p.emitToolResult(tc, fmt.Sprintf("Memory '%s' saved successfully.", sanitizeKey(key)), "")
}

func (p *Plugin) handleMemoryRead(tc events.ToolCall) {
	key, _ := tc.Arguments["key"].(string)
	if key == "" {
		p.emitToolResult(tc, "", "key is required")
		return
	}

	entry, err := p.doRead(key)
	if err != nil {
		p.emitToolResult(tc, "", err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Key: %s\n", entry.Key)
	if len(entry.Tags) > 0 {
		b.WriteString("Tags: ")
		first := true
		for k, v := range entry.Tags {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s:%s", k, v)
			first = false
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Created: %s\n", entry.Created.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(&b, "Updated: %s\n\n", entry.Updated.Format("2006-01-02 15:04:05 UTC"))
	b.WriteString(entry.Content)

	p.emitToolResult(tc, b.String(), "")
}

func (p *Plugin) handleMemoryList(tc events.ToolCall) {
	tags := extractStringMap(tc.Arguments["tags"])

	entries, err := p.doList(tags)
	if err != nil {
		p.emitToolResult(tc, "", err.Error())
		return
	}

	if len(entries) == 0 {
		p.emitToolResult(tc, "No memories found.", "")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d memories:\n\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s", e.Key)
		if len(e.Tags) > 0 {
			b.WriteString(" [")
			first := true
			for k, v := range e.Tags {
				if !first {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s:%s", k, v)
				first = false
			}
			b.WriteString("]")
		}
		fmt.Fprintf(&b, " — %s\n", e.Preview)
	}

	p.emitToolResult(tc, b.String(), "")
}

func (p *Plugin) handleMemoryDelete(tc events.ToolCall) {
	key, _ := tc.Arguments["key"].(string)
	if key == "" {
		p.emitToolResult(tc, "", "key is required")
		return
	}

	if err := p.doDelete(key); err != nil {
		p.emitToolResult(tc, "", err.Error())
		return
	}

	p.emitToolResult(tc, fmt.Sprintf("Memory '%s' deleted.", sanitizeKey(key)), "")
}

// --- Core operations ---

func (p *Plugin) doStore(req events.LongTermMemoryStoreRequest) error {
	sessionID := ""
	if p.session != nil {
		sessionID = p.session.ID
	}

	if err := store(p.writePath, req, sessionID); err != nil {
		p.logger.Error("failed to store memory", "key", req.Key, "error", err)
		return err
	}

	// Refresh index.
	_ = p.loadIndex()

	_ = p.bus.Emit("memory.longterm.stored", events.LongTermMemoryStored{
		Key: sanitizeKey(req.Key),
	})

	p.logger.Info("memory stored", "key", sanitizeKey(req.Key))
	return nil
}

func (p *Plugin) doRead(key string) (*events.LongTermMemoryEntry, error) {
	// Search all paths, agent-scoped first.
	for i := len(p.paths) - 1; i >= 0; i-- {
		entry, err := read(p.paths[i], key)
		if err == nil {
			_ = p.bus.Emit("memory.longterm.result", events.LongTermMemoryReadResult{
				Key:     entry.Key,
				Content: entry.Content,
				Tags:    entry.Tags,
			})
			return entry, nil
		}
	}
	return nil, fmt.Errorf("memory '%s' not found", sanitizeKey(key))
}

func (p *Plugin) doDelete(key string) error {
	if err := remove(p.writePath, key); err != nil {
		p.logger.Error("failed to delete memory", "key", key, "error", err)
		return err
	}

	_ = p.loadIndex()

	_ = p.bus.Emit("memory.longterm.deleted", events.LongTermMemoryDeleted{
		Key: sanitizeKey(key),
	})

	p.logger.Info("memory deleted", "key", sanitizeKey(key))
	return nil
}

func (p *Plugin) doList(tags map[string]string) ([]events.LongTermMemoryIndex, error) {
	var merged []events.LongTermMemoryIndex
	seen := make(map[string]bool)

	for i := len(p.paths) - 1; i >= 0; i-- {
		entries, err := list(p.paths[i], tags)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !seen[e.Key] {
				seen[e.Key] = true
				merged = append(merged, e)
			}
		}
	}

	_ = p.bus.Emit("memory.longterm.list.result", events.LongTermMemoryListResult{
		Entries: merged,
	})

	return merged, nil
}

// --- Helpers ---

func (p *Plugin) emitToolResult(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{
		ID:     tc.ID,
		Name:   tc.Name,
		Output: output,
		Error:  errMsg,
		TurnID: tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}

func (p *Plugin) agentMemoryPath() string {
	if p.agentID != "" {
		return engine.ExpandPath(filepath.Join("~/.nexus/agents", p.agentID, "memory"))
	}
	return engine.ExpandPath("~/.nexus/memory/")
}

// extractStringMap extracts a map[string]string from an any value (JSON-decoded map).
func extractStringMap(v any) map[string]string {
	if v == nil {
		return nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(raw))
	for k, val := range raw {
		if s, ok := val.(string); ok {
			result[k] = s
		}
	}
	return result
}
