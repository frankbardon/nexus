// Package vector implements the memory.vector capability: a per-agent
// semantic-recall store separate from nexus.memory.longterm (which is a
// file-per-entry notes store). This plugin:
//
//   - Listens on io.input: embeds the latest user message, queries the
//     agent's namespace, stashes hits in an internal buffer that the
//     registered PromptSectionFunc renders into the system prompt.
//
//   - Auto-stores on memory.compacted: writes the summary message so that
//     context summaries remain recallable after compaction.
//
//   - Provides an explicit memory.vector.store event for plugins (or tools)
//     that want to deliberately record a piece of content.
//
// Fully independent of nexus.memory.longterm — separate code, separate
// capability, separate storage. They coexist: longterm is structured notes
// with keys; memory.vector is fuzzy semantic recall keyed by embedding.
package vector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/memory/internal/approval"
)

const (
	pluginID   = "nexus.memory.vector"
	pluginName = "Vector Memory"
	version    = "0.1.0"

	defaultTopK            = 5
	defaultSectionPriority = 45
	maxContentForEmbed     = 8192 // cap very long user inputs before embedding
)

// Plugin implements vector-backed memory.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	prompts *engine.PromptRegistry

	namespace           string
	topK                int
	minSimilarity       float32
	embeddingModel      string
	autoStoreCompaction bool
	autoStoreUserInput  bool // default false — conservative
	sectionPriority     int
	// hasHybrid mirrors the knowledge_search switch: when search.hybrid is
	// active, recall queries go through the orchestrator so memory benefits
	// from lexical + vector fusion. Disabled by default — opt-in via
	// `recall_via_hybrid: true` because per-input recall is latency-sensitive
	// and the lexical leg adds work even when most user inputs are
	// question-shaped.
	recallViaHybrid bool
	hasHybrid       bool

	// HITL approval gating for vector writes. Off by default.
	approvalEnabled       bool
	approvalDefaultChoice string
	approvalTimeout       time.Duration
	approvalNamespaceGlob string
	approvalSizeThreshold int

	mu          sync.Mutex
	lastQuery   string
	lastMatches []events.VectorMatch

	unsubs []func()
}

func New() engine.Plugin {
	return &Plugin{
		topK:                defaultTopK,
		autoStoreCompaction: true,
		sectionPriority:     defaultSectionPriority,
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "memory.vector",
		Description: "Per-agent semantic recall memory backed by the vector.store capability.",
	}}
}

func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{Capability: "embeddings.provider"},
		{Capability: "vector.store"},
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.prompts = ctx.Prompts

	// Namespace: explicit config > per-agent default keyed by InstanceID.
	if v, ok := ctx.Config["namespace"].(string); ok && v != "" {
		p.namespace = v
	} else {
		p.namespace = defaultNamespace(ctx.InstanceID)
	}
	if v, ok := ctx.Config["top_k"].(int); ok && v > 0 {
		p.topK = v
	}
	if v, ok := ctx.Config["top_k"].(float64); ok && v > 0 {
		p.topK = int(v)
	}
	if v, ok := ctx.Config["min_similarity"].(float64); ok {
		p.minSimilarity = float32(v)
	}
	if v, ok := ctx.Config["embedding_model"].(string); ok {
		p.embeddingModel = v
	}
	if v, ok := ctx.Config["auto_store_compaction"].(bool); ok {
		p.autoStoreCompaction = v
	}
	if v, ok := ctx.Config["auto_store_user_input"].(bool); ok {
		p.autoStoreUserInput = v
	}
	if v, ok := ctx.Config["section_priority"].(int); ok {
		p.sectionPriority = v
	}
	if v, ok := ctx.Config["recall_via_hybrid"].(bool); ok {
		p.recallViaHybrid = v
	}
	p.hasHybrid = len(ctx.Capabilities["search.hybrid"]) > 0
	p.parseApprovalConfig(ctx.Config["require_approval"])

	// Subscribe at priority 10 so the query+stash happens before the agent's
	// handler fires at priority 50. That way the prompt section sees fresh
	// matches when the agent builds the next llm.request.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compacted", p.handleCompacted,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.vector.store", p.handleExplicitStore,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	// System prompt section renders the last-recalled hits.
	if p.prompts != nil {
		p.prompts.Register(pluginID, p.sectionPriority, p.buildPromptSection)
	}

	p.logger.Info("vector memory initialized",
		"namespace", p.namespace,
		"top_k", p.topK,
		"auto_store_compaction", p.autoStoreCompaction,
		"auto_store_user_input", p.autoStoreUserInput,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.prompts != nil {
		p.prompts.Unregister(pluginID)
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 10},
		{EventType: "memory.compacted", Priority: 50},
		{EventType: "memory.vector.store", Priority: 50},
		// hitl.responded is dynamically subscribed by the approval helper
		// when require_approval.enabled and a write is pending. Declared
		// here for introspection completeness.
		{EventType: "hitl.responded", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"embeddings.request",
		"vector.query",
		"hybrid.query",
		"vector.upsert",
		"rag.retrieved",
		"hitl.requested",
	}
}

// handleInput runs on every user message: embeds it, queries, stashes hits
// for the next prompt section render. Optionally stores the input itself.
func (p *Plugin) handleInput(event engine.Event[any]) {
	in, ok := event.Payload.(events.UserInput)
	if !ok {
		return
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return
	}
	if len(content) > maxContentForEmbed {
		content = content[:maxContentForEmbed]
	}

	vec, err := p.embedOne(content)
	if err != nil {
		p.logger.Warn("vector memory: embed user input failed", "err", err)
		return
	}
	if len(vec) > 0 {
		p.queryAndStash(content, vec)
	}

	if p.autoStoreUserInput {
		p.storeDoc(content, "user", map[string]string{
			"session": in.SessionID,
		}, vec)
	}
}

// handleCompacted auto-stores the compaction summary so past context
// remains recallable after the history buffer trims it away.
func (p *Plugin) handleCompacted(event engine.Event[any]) {
	if !p.autoStoreCompaction {
		return
	}
	done, ok := event.Payload.(events.CompactionComplete)
	if !ok {
		return
	}
	// The first system message of the compacted sequence carries the summary
	// (see plugins/memory/compaction/plugin.go → finishCompaction).
	var summary string
	for _, m := range done.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "Prior Context") {
			summary = m.Content
			break
		}
	}
	if summary == "" {
		return
	}
	p.storeDoc(summary, "compaction", map[string]string{
		"backup_path": done.BackupPath,
	}, nil)
}

// handleExplicitStore handles memory.vector.store — an opt-in path for
// plugins or tools that want to record content deliberately.
func (p *Plugin) handleExplicitStore(event engine.Event[any]) {
	req, ok := event.Payload.(*events.VectorMemoryStore)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	src := req.Source
	if src == "" {
		src = "explicit"
	}
	if err := p.storeDoc(req.Content, src, req.Metadata, nil); err != nil {
		req.Error = err.Error()
	}
}

func (p *Plugin) queryAndStash(query string, vec []float32) {
	var matches []events.VectorMatch
	if p.recallViaHybrid && p.hasHybrid {
		h := &events.HybridQuery{
			Namespace: p.namespace,
			Query:     query,
			Vector:    vec, // pass pre-embedded vector to skip the embed call
			K:         p.topK,
		}
		_ = p.bus.Emit("hybrid.query", h)
		if h.Error != "" {
			p.logger.Warn("vector memory: hybrid query failed", "err", h.Error)
			return
		}
		matches = make([]events.VectorMatch, 0, len(h.Matches))
		for _, m := range h.Matches {
			matches = append(matches, events.VectorMatch{
				ID: m.ID, Content: m.Content, Metadata: m.Metadata,
				Similarity: m.Similarity, // raw vector sim if present, else 0
			})
		}
	} else {
		q := &events.VectorQuery{Namespace: p.namespace, Vector: vec, K: p.topK}
		_ = p.bus.Emit("vector.query", q)
		if q.Error != "" {
			p.logger.Warn("vector memory: query failed", "err", q.Error)
			return
		}
		matches = q.Matches
	}

	filtered := matches
	if p.minSimilarity > 0 {
		filtered = filtered[:0]
		for _, m := range matches {
			if m.Similarity >= p.minSimilarity {
				filtered = append(filtered, m)
			}
		}
	}
	p.mu.Lock()
	p.lastQuery = query
	p.lastMatches = filtered
	p.mu.Unlock()
	p.emitRetrieved(filtered)
}

// emitRetrieved publishes the retrieved-memory chunks for the citation
// plugin. TurnID is unknown at recall time (handleInput fires before
// agent.turn.start), so we leave it blank — the citation plugin treats
// blank-turn entries as ambient (carried forward to the next turn it sees).
func (p *Plugin) emitRetrieved(matches []events.VectorMatch) {
	if len(matches) == 0 {
		return
	}
	chunks := make([]events.RetrievedChunk, 0, len(matches))
	for _, m := range matches {
		chunks = append(chunks, events.RetrievedChunk{
			Source:    m.Metadata["source"],
			DocID:     m.ID,
			ChunkIdx:  m.Metadata["chunk_idx"],
			TrustTier: m.Metadata["trust_tier"],
		})
	}
	_ = p.bus.Emit("rag.retrieved", events.RetrievalContext{
		Source: pluginID,
		Chunks: chunks,
	})
}

// storeDoc embeds (if vec is nil) and upserts a single document.
func (p *Plugin) storeDoc(content, source string, extra map[string]string, vec []float32) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if p.shouldRequireApproval(content) {
		if err := p.gateWithApproval(content, source, extra); err != nil {
			return err
		}
	}
	if vec == nil {
		v, err := p.embedOne(content)
		if err != nil {
			return err
		}
		vec = v
	}
	meta := map[string]string{
		"source": source,
		"stored": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range extra {
		if v != "" {
			meta[k] = v
		}
	}
	id := docID(content, source, time.Now())
	up := &events.VectorUpsert{
		Namespace: p.namespace,
		Docs: []events.VectorDoc{{
			ID:       id,
			Vector:   vec,
			Content:  content,
			Metadata: meta,
		}},
	}
	_ = p.bus.Emit("vector.upsert", up)
	if up.Error != "" {
		return fmt.Errorf("upsert: %s", up.Error)
	}
	p.logger.Debug("vector memory: stored", "source", source, "id", id)
	return nil
}

// embedOne embeds a single string via the embeddings.provider capability.
func (p *Plugin) embedOne(text string) ([]float32, error) {
	req := &events.EmbeddingsRequest{Texts: []string{text}, Model: p.embeddingModel}
	_ = p.bus.Emit("embeddings.request", req)
	if req.Error != "" {
		return nil, fmt.Errorf("embed: %s", req.Error)
	}
	if len(req.Vectors) != 1 {
		return nil, fmt.Errorf("embed: expected 1 vector, got %d", len(req.Vectors))
	}
	return req.Vectors[0], nil
}

// buildPromptSection renders the last retrieved hits as an XML block.
// Empty output causes the PromptRegistry to skip the section — so a turn
// with no hits adds nothing to the prompt.
func (p *Plugin) buildPromptSection() string {
	p.mu.Lock()
	matches := p.lastMatches
	p.mu.Unlock()
	if len(matches) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<recalled_memory>\n")
	b.WriteString("These items were recalled from your vector memory based on the current user message. ")
	b.WriteString("Use them if relevant; ignore them if not.\n\n")
	for i, m := range matches {
		fmt.Fprintf(&b, "  <item rank=\"%d\" similarity=\"%.3f\" source=\"%s\">\n",
			i+1, m.Similarity, m.Metadata["source"])
		b.WriteString("    ")
		b.WriteString(strings.ReplaceAll(m.Content, "\n", "\n    "))
		b.WriteString("\n  </item>\n")
	}
	b.WriteString("</recalled_memory>")
	return b.String()
}

func defaultNamespace(instanceID string) string {
	if instanceID == "" {
		return "memory-default"
	}
	// InstanceID may contain slashes (e.g. "nexus.agent.subagent/researcher").
	// Replace them so the namespace is filesystem-safe.
	safe := strings.NewReplacer("/", "-", ":", "-").Replace(instanceID)
	return "memory-" + safe
}

// docID produces a reasonably unique ID keyed on content + source + time.
// Content-hash alone would collide on re-storing the same summary twice;
// including time lets history accumulate.
func docID(content, source string, t time.Time) string {
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte("\x00"))
	h.Write([]byte(source))
	h.Write([]byte("\x00"))
	h.Write([]byte(t.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h.Sum(nil))[:16]
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
	if v, ok := match["namespace_glob"].(string); ok {
		p.approvalNamespaceGlob = v
	}
	if v, ok := match["size_threshold_bytes"].(int); ok {
		p.approvalSizeThreshold = v
	} else if v, ok := match["size_threshold_bytes"].(float64); ok {
		p.approvalSizeThreshold = int(v)
	}
}

// shouldRequireApproval evaluates the match block against this plugin's
// fixed namespace and the pending content's size.
func (p *Plugin) shouldRequireApproval(content string) bool {
	if !p.approvalEnabled {
		return false
	}
	if p.approvalNamespaceGlob != "" {
		ok, _ := filepath.Match(p.approvalNamespaceGlob, p.namespace)
		if !ok {
			return false
		}
	}
	if p.approvalSizeThreshold > 0 && len(content) < p.approvalSizeThreshold {
		return false
	}
	return true
}

// gateWithApproval emits hitl.requested and blocks until the response.
// Returns a non-nil error when the operator rejects.
func (p *Plugin) gateWithApproval(content, source string, extra map[string]string) error {
	preview := content
	truncated := false
	const maxPreview = 2000
	if len(preview) > maxPreview {
		preview = preview[:maxPreview]
		truncated = true
	}
	actionRef := map[string]any{
		"namespace": p.namespace,
		"source":    source,
		"content":   preview,
		"size":      len(content),
	}
	if len(extra) > 0 {
		actionRef["metadata"] = extra
	}
	if truncated {
		actionRef["_truncated"] = true
	}

	prompt := fmt.Sprintf("Persist vector memory entry to namespace '%s'? [%d bytes, source=%s]",
		p.namespace, len(content), source)

	_, allowed, err := approval.RequestApproval(context.Background(), approval.Request{
		Bus:             p.bus,
		Logger:          p.logger,
		PluginID:        pluginID,
		ActionKind:      "memory.vector.write",
		ActionRef:       actionRef,
		Prompt:          prompt,
		DefaultChoiceID: p.approvalDefaultChoice,
		Timeout:         p.approvalTimeout,
	})
	if err != nil {
		p.logger.Error("vector: approval request failed", "namespace", p.namespace, "error", err)
		return fmt.Errorf("vector: approval: %w", err)
	}
	if !allowed {
		p.logger.Info("vector: write rejected by operator",
			"namespace", p.namespace,
			"source", source,
			"size", len(content),
		)
		return fmt.Errorf("vector memory write rejected by approval gate")
	}
	return nil
}
