// Package retrieve registers the user-facing "retrieve" tool. The tool
// embeds the query via the embeddings.provider capability and runs a
// nearest-neighbor lookup against one or more namespaces via the
// vector.store capability. Namespaces are config-constrained — the LLM
// can narrow within the configured allow-list but cannot reach into
// other stores.
package retrieve

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
	pluginID   = "nexus.tool.retrieve"
	pluginName = "RAG Retrieve Tool"
	version    = "0.1.0"

	defaultToolName = "retrieve"
	defaultTopK     = 5
	maxTopK         = 50
)

// retrievedHit pairs a vector-store match with the namespace it came from so
// cross-namespace results can be merged and ranked together.
type retrievedHit struct {
	namespace string
	match     events.VectorMatch
}

// Plugin registers a retrieve tool that queries configured namespaces.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	toolName         string
	allowedNS        []string // master list; empty means "any namespace" (rare)
	defaultNS        []string // used when the LLM doesn't pick
	topK             int
	includeMetadata  bool
	embeddingModel   string // optional pin; otherwise provider default

	unsubs []func()
}

func New() engine.Plugin {
	return &Plugin{
		toolName:        defaultToolName,
		topK:            defaultTopK,
		includeMetadata: true,
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{Capability: "embeddings.provider"},
		{Capability: "vector.store"},
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["tool_name"].(string); ok && v != "" {
		p.toolName = v
	}
	if v, ok := ctx.Config["top_k"].(int); ok && v > 0 {
		p.topK = v
	}
	if v, ok := ctx.Config["top_k"].(float64); ok && v > 0 {
		p.topK = int(v)
	}
	if v, ok := ctx.Config["include_metadata"].(bool); ok {
		p.includeMetadata = v
	}
	if v, ok := ctx.Config["embedding_model"].(string); ok {
		p.embeddingModel = v
	}
	p.allowedNS = configStrings(ctx.Config["namespaces"])
	p.defaultNS = configStrings(ctx.Config["default_namespaces"])
	if len(p.defaultNS) == 0 {
		p.defaultNS = p.allowedNS
	}
	if len(p.allowedNS) == 0 {
		return fmt.Errorf("retrieve: at least one namespace must be configured under 'namespaces'")
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Ready() error {
	desc := fmt.Sprintf(
		"Retrieve relevant chunks from the knowledge base using semantic search. "+
			"Available namespaces: %s. Default: %s. Returns top-k chunks ranked by "+
			"similarity, with source paths for citation.",
		strings.Join(p.allowedNS, ", "),
		strings.Join(p.defaultNS, ", "),
	)
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        p.toolName,
		Description: desc,
		Class:       "research",
		Subclass:    "retrieve",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The semantic query to search for. Write it as you would to a search engine.",
				},
				"namespaces": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": fmt.Sprintf("Optional subset of namespaces to search. Must be a subset of: %s.", strings.Join(p.allowedNS, ", ")),
				},
				"k": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("Max number of chunks to return (default %d).", p.topK),
				},
			},
			"required": []string{"query"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"embeddings.request",
		"vector.query",
		"before:tool.result",
		"tool.result",
		"tool.register",
	}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.Name != p.toolName {
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

	// Resolve namespaces: LLM-picked ∩ allow-list, else default.
	requested := argStrings(tc.Arguments["namespaces"])
	namespaces := p.resolveNamespaces(requested)
	if len(namespaces) == 0 {
		p.emit(tc, "", "no valid namespaces selected")
		return
	}

	// Embed the query.
	embReq := &events.EmbeddingsRequest{Texts: []string{query}, Model: p.embeddingModel}
	_ = p.bus.Emit("embeddings.request", embReq)
	if embReq.Error != "" {
		p.emit(tc, "", fmt.Sprintf("embed query: %s", embReq.Error))
		return
	}
	if len(embReq.Vectors) != 1 {
		p.emit(tc, "", "embed query: no vector returned")
		return
	}
	vec := embReq.Vectors[0]

	// Query each namespace, merge by similarity.
	all := make([]retrievedHit, 0, k*len(namespaces))
	for _, ns := range namespaces {
		q := &events.VectorQuery{Namespace: ns, Vector: vec, K: k}
		_ = p.bus.Emit("vector.query", q)
		if q.Error != "" {
			p.logger.Warn("retrieve: namespace query failed", "namespace", ns, "err", q.Error)
			continue
		}
		for _, m := range q.Matches {
			all = append(all, retrievedHit{namespace: ns, match: m})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].match.Similarity > all[j].match.Similarity })
	if len(all) > k {
		all = all[:k]
	}

	output, err := p.formatOutput(query, all)
	if err != nil {
		p.emit(tc, "", err.Error())
		return
	}
	p.emit(tc, output, "")
}

func (p *Plugin) formatOutput(query string, hits []retrievedHit) (string, error) {
	type outItem struct {
		Rank       int               `json:"rank"`
		Namespace  string            `json:"namespace"`
		Similarity float32           `json:"similarity"`
		Source     string            `json:"source,omitempty"`
		ChunkIdx   string            `json:"chunk_idx,omitempty"`
		Content    string            `json:"content"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	out := struct {
		Query   string    `json:"query"`
		Results []outItem `json:"results"`
	}{Query: query}

	for i, h := range hits {
		item := outItem{
			Rank:       i + 1,
			Namespace:  h.namespace,
			Similarity: h.match.Similarity,
			Content:    h.match.Content,
			Source:     h.match.Metadata["source"],
			ChunkIdx:   h.match.Metadata["chunk_idx"],
		}
		if p.includeMetadata {
			item.Metadata = h.match.Metadata
		}
		out.Results = append(out.Results, item)
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format: %w", err)
	}
	return string(buf), nil
}

func (p *Plugin) resolveNamespaces(requested []string) []string {
	if len(requested) == 0 {
		return p.defaultNS
	}
	allow := make(map[string]bool, len(p.allowedNS))
	for _, ns := range p.allowedNS {
		allow[ns] = true
	}
	out := make([]string, 0, len(requested))
	for _, ns := range requested {
		if allow[ns] {
			out = append(out, ns)
		}
	}
	return out
}

func (p *Plugin) emit(tc events.ToolCall, output, errMsg string) {
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

func configStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func argStrings(v any) []string {
	switch vv := v.(type) {
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return vv
	case string:
		if vv == "" {
			return nil
		}
		return []string{vv}
	}
	return nil
}
