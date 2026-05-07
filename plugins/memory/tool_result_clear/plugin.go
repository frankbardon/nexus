// Package tool_result_clear implements live curation of tool result bodies
// in the LLM-visible conversation. The call/result envelope stays in
// history so the agent retains the fact that a tool was invoked; only the
// (often-large) result body is replaced with a marker once heuristics
// decide it is no longer load-bearing.
//
// Operates at priority 12 on before:llm.request so it runs after
// nexus.discovery.progressive (priority 8) has shaped the tool list, then
// re-shapes the outgoing message slice without touching the upstream
// history buffer.
package tool_result_clear

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/memory/internal/internalflow"
)

const (
	pluginID   = "nexus.memory.tool_result_clear"
	pluginName = "Tool-Result Clearing"
	version    = "0.1.0"
)

// Drop strategies.
const (
	dropReplaceEnvelope = "replace_with_envelope"
	dropFullDrop        = "full_drop"
)

// Reason tags carried on MemoryToolResultCleared.
const (
	reasonAge            = "age"
	reasonSubsequentCall = "subsequent_call"
	reasonPreservedKind  = "preserved_kind"
)

// callRecord tracks a single tool invocation we've seen on the bus.
type callRecord struct {
	id        string
	name      string
	argHash   string
	turn      int
	resultLen int
	cleared   bool
	// kind classifies the result body so preserve_recent_kinds can keep
	// errors and user_question results verbatim past the age cutoff.
	kind string
}

// Plugin clears stale tool result bodies from outgoing LLM requests.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	// Config.
	enabled            bool
	ageTurns           int
	sizeBytesThreshold int
	preserveKinds      map[string]bool
	dropStrategy       string

	mu    sync.Mutex
	turn  int
	calls map[string]*callRecord // tool_call_id -> record
	// argIndex groups call ids by tool name + canonical args hash so we
	// can detect "called again with the same args, drop earlier results".
	argIndex map[string][]string // "name|argHash" -> []callID
	// preCleared remembers which IDs we've already cleared so subsequent
	// before:llm.request handlers re-clear deterministically without
	// re-running the heuristic.
	preCleared map[string]string // id -> reason

	unsubs []func()
}

// New creates a new tool_result_clear plugin.
func New() engine.Plugin {
	return &Plugin{
		enabled:            true,
		ageTurns:           5,
		sizeBytesThreshold: 1000,
		preserveKinds: map[string]bool{
			"error":         true,
			"user_question": true,
		},
		dropStrategy: dropReplaceEnvelope,
		calls:        make(map[string]*callRecord),
		argIndex:     make(map[string][]string),
		preCleared:   make(map[string]string),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 60},
		{EventType: "tool.result", Priority: 60},
		{EventType: "agent.turn.end", Priority: 60},
		{EventType: "before:llm.request", Priority: 12},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"memory.tool_result_cleared",
		"memory.curated",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["enabled"].(bool); ok {
		p.enabled = v
	}
	if v, ok := intConfig(ctx.Config, "age_turns", p.ageTurns); ok {
		p.ageTurns = v
	}
	if v, ok := intConfig(ctx.Config, "size_bytes_threshold", p.sizeBytesThreshold); ok {
		p.sizeBytesThreshold = v
	}
	if list, ok := ctx.Config["preserve_recent_kinds"].([]any); ok {
		preserve := make(map[string]bool, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				preserve[s] = true
			}
		}
		p.preserveKinds = preserve
	}
	if v, ok := ctx.Config["drop_strategy"].(string); ok {
		switch v {
		case dropReplaceEnvelope, dropFullDrop:
			p.dropStrategy = v
		default:
			return fmt.Errorf("tool_result_clear: invalid drop_strategy %q", v)
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(60), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult,
			engine.WithPriority(60), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd,
			engine.WithPriority(60), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(12), engine.WithSource(pluginID)),
	)

	p.logger.Info("tool_result_clear initialized",
		"enabled", p.enabled,
		"age_turns", p.ageTurns,
		"size_bytes_threshold", p.sizeBytesThreshold,
		"drop_strategy", p.dropStrategy)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	return nil
}

// --- bus handlers ---

func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.ID == "" {
		return
	}

	hash := canonicalArgHash(tc.Arguments)
	key := tc.Name + "|" + hash

	p.mu.Lock()
	defer p.mu.Unlock()

	if rec, exists := p.calls[tc.ID]; exists {
		// Re-invoke of same id (rare); just update turn pointer.
		rec.turn = p.turn
		rec.argHash = hash
		rec.name = tc.Name
		return
	}

	p.calls[tc.ID] = &callRecord{
		id:      tc.ID,
		name:    tc.Name,
		argHash: hash,
		turn:    p.turn,
	}
	p.argIndex[key] = append(p.argIndex[key], tc.ID)
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	res, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	rec, exists := p.calls[res.ID]
	if !exists {
		// Result without a recorded invoke (e.g. tool emitted result
		// pre-init); make a minimal record so we can still age it.
		rec = &callRecord{id: res.ID, name: res.Name, turn: p.turn}
		p.calls[res.ID] = rec
	}
	rec.resultLen = len(res.Output)
	rec.kind = classifyResult(res)
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	if _, ok := e.Payload.(events.TurnInfo); !ok {
		return
	}
	p.mu.Lock()
	p.turn++
	p.mu.Unlock()
}

// handleBeforeLLMRequest mutates req.Messages, replacing tool result bodies
// that meet the staleness criteria. The history buffer is left intact.
func (p *Plugin) handleBeforeLLMRequest(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	vp, ok := e.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}
	// Skip internal sub-flow requests (planner / classifier / summariser
	// / compaction / subagent). Main agent loops are processed even
	// though every agent main request now tags `_source = pluginID` for
	// cost attribution (Idea 09).
	if internalflow.SkipForCuration(req.Metadata) {
		return
	}

	p.mu.Lock()
	now := p.turn
	cleared := make([]events.MemoryToolResultCleared, 0)
	sections := make([]events.CurationSection, 0)

	// First pass: for each tool message in the request, decide.
	keep := make([]events.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role != "tool" || m.ToolCallID == "" {
			keep = append(keep, m)
			continue
		}

		// Already-cleared shortcut.
		if reason, isCleared := p.preCleared[m.ToolCallID]; isCleared {
			if p.dropStrategy == dropFullDrop {
				continue
			}
			rec := p.calls[m.ToolCallID]
			toolName := m.ToolCallID
			origSize := len(m.Content)
			if rec != nil {
				if rec.name != "" {
					toolName = rec.name
				}
				if rec.resultLen > 0 {
					origSize = rec.resultLen
				}
			}
			m.Content = envelopeMarker(m.ToolCallID, toolName, origSize, now)
			keep = append(keep, m)
			_ = reason // already-cleared events emitted on first clearing only
			continue
		}

		rec := p.calls[m.ToolCallID]
		// Heuristic decision.
		shouldClear, reason := p.decideClearLocked(rec, m, now)
		if !shouldClear {
			keep = append(keep, m)
			continue
		}

		toolName := m.ToolCallID
		origSize := len(m.Content)
		if rec != nil {
			if rec.name != "" {
				toolName = rec.name
			}
			if rec.resultLen > 0 {
				origSize = rec.resultLen
			}
			rec.cleared = true
		}
		p.preCleared[m.ToolCallID] = reason

		cleared = append(cleared, events.MemoryToolResultCleared{
			SchemaVersion: events.MemoryToolResultClearedVersion,
			ToolCallID:    m.ToolCallID,
			Tool:          toolName,
			OriginalSize:  origSize,
			ClearedAtTurn: now,
			Reason:        reason,
		})
		sections = append(sections, events.CurationSection{
			SectionID:   pluginID + "/" + m.ToolCallID,
			Kind:        "volatile",
			TokensDelta: -origSize / 4, // crude chars-per-token estimate
		})

		if p.dropStrategy == dropFullDrop {
			continue
		}
		m.Content = envelopeMarker(m.ToolCallID, toolName, origSize, now)
		keep = append(keep, m)
	}
	p.mu.Unlock()

	if len(cleared) == 0 {
		return
	}
	req.Messages = keep

	for _, ev := range cleared {
		_ = p.bus.Emit("memory.tool_result_cleared", ev)
	}
	_ = p.bus.Emit("memory.curated", events.MemoryCurated{
		SchemaVersion:    events.MemoryCuratedVersion,
		Layer:            "tool_result_clear",
		SectionsTouched:  sections,
		CacheInvalidates: false,
		AtTurn:           now,
	})
}

// decideClearLocked applies the staleness heuristics to a single tool
// result message. Caller holds p.mu.
func (p *Plugin) decideClearLocked(rec *callRecord, m events.Message, now int) (bool, string) {
	// Preserve hot kinds regardless of age.
	if rec != nil && rec.kind != "" && p.preserveKinds[rec.kind] {
		return false, reasonPreservedKind
	}

	// Subsequent-call: same name+args invoked again later — earlier result
	// is redundant. Drop regardless of size, but only when age is positive
	// (don't clear the very call we just observed).
	if rec != nil {
		key := rec.name + "|" + rec.argHash
		ids := p.argIndex[key]
		if len(ids) > 1 {
			latest := ids[len(ids)-1]
			if latest != rec.id && rec.turn < p.calls[latest].turn {
				return true, reasonSubsequentCall
			}
		}
	}

	// Age + size.
	resultLen := len(m.Content)
	if resultLen >= p.sizeBytesThreshold {
		callTurn := now
		if rec != nil {
			callTurn = rec.turn
		}
		if now-callTurn >= p.ageTurns {
			return true, reasonAge
		}
	}

	return false, ""
}

// --- helpers ---

// envelopeMarker returns the inline replacement body for a cleared tool
// result. Mirrors the form documented in idea.md so traces and journals
// surface a stable marker shape.
func envelopeMarker(id, tool string, originalSize, clearedAtTurn int) string {
	return fmt.Sprintf(
		`<tool_result id=%q tool=%q cleared="true" original_size=%q cleared_at=%q />`,
		id, tool, fmt.Sprintf("%d", originalSize), fmt.Sprintf("turn-%d", clearedAtTurn),
	)
}

// canonicalArgHash produces a deterministic short hash of a tool call's
// arguments so subsequent-call detection survives JSON key reordering.
func canonicalArgHash(args map[string]any) string {
	if len(args) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		raw, _ := json.Marshal(args[k])
		b.Write(raw)
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// classifyResult tags a tool result so preserve_recent_kinds can opt
// specific kinds out of clearing. v1 only distinguishes errors; future
// kinds (user_question, etc.) plug in here.
func classifyResult(res events.ToolResult) string {
	if res.Error != "" {
		return "error"
	}
	return "ok"
}

// intConfig parses an integer from a YAML map allowing both int and
// float64 representations. Returns (value, true) when the key is present
// with a usable type.
func intConfig(cfg map[string]any, key string, def int) (int, bool) {
	if v, ok := cfg[key].(int); ok {
		return v, true
	}
	if v, ok := cfg[key].(float64); ok {
		return int(v), true
	}
	_ = def
	return 0, false
}
