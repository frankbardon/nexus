// Package docssearch registers the user-facing "search_docs" tool. The tool
// runs a BM25 keyword lookup against a single namespace via the search.lexical
// capability (SQLite FTS5) and returns the top sections with source file and
// heading for citation. No embeddings, no vector store — pure lexical search.
package docssearch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.docs_search"
	pluginName = "Docs Search Tool"
	version    = "0.1.0"

	defaultToolName  = "search_docs"
	defaultNamespace = "docs"
	defaultTopK      = 5
	maxTopK          = 50
)

// Plugin registers a search_docs tool backed by the lexical store.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	toolName  string
	namespace string
	topK      int

	unsubs []func()
}

func New() engine.Plugin {
	return &Plugin{toolName: defaultToolName, namespace: defaultNamespace, topK: defaultTopK}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{{Capability: "search.lexical"}}
}

func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["tool_name"].(string); ok && v != "" {
		p.toolName = v
	}
	if v, ok := ctx.Config["namespace"].(string); ok && v != "" {
		p.namespace = v
	}
	if v, ok := ctx.Config["top_k"].(int); ok && v > 0 {
		p.topK = v
	}
	if v, ok := ctx.Config["top_k"].(float64); ok && v > 0 {
		p.topK = int(v)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Ready() error {
	desc := fmt.Sprintf(
		"Search the approved documentation by keyword (BM25 full-text). Returns "+
			"the most relevant doc sections, each with its source file and heading "+
			"for citation. ALWAYS call this before answering a documentation "+
			"question, and cite results as `source#heading`. Searches the %q namespace.",
		p.namespace)
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        p.toolName,
		Description: desc,
		Class:       "research",
		Subclass:    "search",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Keyword query. Use the key terms you expect to appear in the docs.",
				},
				"k": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("Max sections to return (default %d).", p.topK),
				},
			},
			"required": []string{"query"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{{EventType: "tool.invoke", Priority: 50}}
}

func (p *Plugin) Emissions() []string {
	return []string{"lexical.query", "before:tool.result", "tool.result", "tool.register"}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != p.toolName {
		return
	}
	p.handle(tc)
}

func (p *Plugin) handle(tc events.ToolCall) {
	query, _ := tc.Arguments["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		p.emit(tc, "", "query argument required")
		return
	}

	k := p.topK
	if v, ok := tc.Arguments["k"].(float64); ok && v > 0 {
		k = int(v)
	}
	if v, ok := tc.Arguments["k"].(int); ok && v > 0 {
		k = v
	}
	if k > maxTopK {
		k = maxTopK
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: p.namespace, Query: query, K: k}
	if err := p.bus.Emit("lexical.query", q); err != nil {
		p.emit(tc, "", fmt.Sprintf("search: %v", err))
		return
	}
	if q.Error != "" {
		p.emit(tc, "", fmt.Sprintf("search: %s", q.Error))
		return
	}

	matches := q.Matches
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	if len(matches) > k {
		matches = matches[:k]
	}

	output, err := formatOutput(query, matches)
	if err != nil {
		p.emit(tc, "", err.Error())
		return
	}
	p.emit(tc, output, "")
}

// formatOutput renders matches as a stable JSON document the LLM can read and
// cite. Source and heading come from the metadata the indexer attached.
func formatOutput(query string, matches []events.LexicalMatch) (string, error) {
	type item struct {
		Rank    int     `json:"rank"`
		Source  string  `json:"source"`
		Heading string  `json:"heading,omitempty"`
		Score   float32 `json:"score"`
		Content string  `json:"content"`
	}
	out := struct {
		Query   string `json:"query"`
		Count   int    `json:"count"`
		Results []item `json:"results"`
	}{Query: query, Count: len(matches)}

	for i, m := range matches {
		out.Results = append(out.Results, item{
			Rank:    i + 1,
			Source:  m.Metadata["source"],
			Heading: m.Metadata["heading"],
			Score:   m.Score,
			Content: m.Content,
		})
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format: %w", err)
	}
	return string(buf), nil
}

func (p *Plugin) emit(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
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
