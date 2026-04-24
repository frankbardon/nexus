// Package chromem implements the vector.store capability backed by
// philippgille/chromem-go — a pure-Go, in-memory vector store with JSON on-disk
// persistence. Namespaces map to chromem collections. Larger backends
// (sqlite-vec, pgvector, Qdrant) can be added as sibling plugins without
// changing the event surface.
package chromem

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	chromemgo "github.com/philippgille/chromem-go"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.vectorstore.chromem"
	pluginName = "Chromem-go Vector Store"
	version    = "0.1.0"
	defaultDir = "~/.nexus/vectors"
)

// Plugin advertises the vector.store capability. Namespaces map 1:1 to
// chromem collections backed by ./<path>/<namespace>/.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	path     string
	compress bool

	mu     sync.Mutex
	db     *chromemgo.DB
	unsubs []func()
}

// New creates a new chromem-go vector store plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "vector.store",
		Description: "Namespace-aware vector store (chromem-go, on-disk JSON persistence).",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.path = defaultDir
	if v, ok := ctx.Config["path"].(string); ok && v != "" {
		p.path = v
	}
	p.path = expandHome(p.path)

	if v, ok := ctx.Config["compress"].(bool); ok {
		p.compress = v
	}

	if err := os.MkdirAll(p.path, 0o755); err != nil {
		return fmt.Errorf("vectorstore/chromem: create path %q: %w", p.path, err)
	}

	db, err := chromemgo.NewPersistentDB(p.path, p.compress)
	if err != nil {
		return fmt.Errorf("vectorstore/chromem: open db at %q: %w", p.path, err)
	}
	p.db = db

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("vector.upsert", p.handleUpsert,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("vector.query", p.handleQuery,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("vector.delete", p.handleDelete,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("vector.namespace.drop", p.handleDropNamespace,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	p.logger.Info("vectorstore/chromem ready", "path", p.path, "collections", len(p.db.ListCollections()))
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
	return []engine.EventSubscription{
		{EventType: "vector.upsert", Priority: 50},
		{EventType: "vector.query", Priority: 50},
		{EventType: "vector.delete", Priority: 50},
		{EventType: "vector.namespace.drop", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string { return nil }

// getOrCreate returns the chromem collection for the namespace, creating the
// backing directory on first use. Collections are embedding-function-less
// because we always supply pre-computed vectors.
func (p *Plugin) getOrCreate(ns string) (*chromemgo.Collection, error) {
	if ns == "" {
		return nil, fmt.Errorf("namespace required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db.GetOrCreateCollection(ns, nil, nil)
}

func (p *Plugin) handleUpsert(event engine.Event[any]) {
	req, ok := event.Payload.(*events.VectorUpsert)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	coll, err := p.getOrCreate(req.Namespace)
	if err != nil {
		req.Error = err.Error()
		return
	}
	if len(req.Docs) == 0 {
		return
	}

	// chromem-go has no native upsert — delete existing IDs first so that
	// re-ingest replaces content rather than accumulating duplicates.
	ids := make([]string, 0, len(req.Docs))
	for _, d := range req.Docs {
		ids = append(ids, d.ID)
	}
	if err := coll.Delete(context.Background(), nil, nil, ids...); err != nil {
		// Delete errors on unknown IDs don't matter; log but continue.
		p.logger.Debug("pre-upsert delete (non-fatal)", "err", err)
	}

	embeddings := make([][]float32, len(req.Docs))
	metas := make([]map[string]string, len(req.Docs))
	contents := make([]string, len(req.Docs))
	for i, d := range req.Docs {
		embeddings[i] = d.Vector
		metas[i] = d.Metadata
		contents[i] = d.Content
	}

	if err := coll.Add(context.Background(), ids, embeddings, metas, contents); err != nil {
		req.Error = fmt.Errorf("upsert: %w", err).Error()
		return
	}
}

func (p *Plugin) handleQuery(event engine.Event[any]) {
	req, ok := event.Payload.(*events.VectorQuery)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	p.mu.Lock()
	coll := p.db.GetCollection(req.Namespace, nil)
	p.mu.Unlock()
	if coll == nil {
		// Missing namespace is not an error — just empty results.
		return
	}

	k := req.K
	if k <= 0 {
		k = 5
	}
	// chromem panics when k > collection size. Clamp.
	if count := coll.Count(); count == 0 {
		return
	} else if k > count {
		k = count
	}

	results, err := coll.QueryEmbedding(context.Background(), req.Vector, k, req.Filter, nil)
	if err != nil {
		req.Error = fmt.Errorf("query: %w", err).Error()
		return
	}
	matches := make([]events.VectorMatch, 0, len(results))
	for _, r := range results {
		matches = append(matches, events.VectorMatch{
			ID:         r.ID,
			Content:    r.Content,
			Metadata:   r.Metadata,
			Similarity: r.Similarity,
		})
	}
	req.Matches = matches
}

func (p *Plugin) handleDelete(event engine.Event[any]) {
	req, ok := event.Payload.(*events.VectorDelete)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	p.mu.Lock()
	coll := p.db.GetCollection(req.Namespace, nil)
	p.mu.Unlock()
	if coll == nil || len(req.IDs) == 0 {
		return
	}
	if err := coll.Delete(context.Background(), nil, nil, req.IDs...); err != nil {
		req.Error = fmt.Errorf("delete: %w", err).Error()
	}
}

func (p *Plugin) handleDropNamespace(event engine.Event[any]) {
	req, ok := event.Payload.(*events.VectorNamespaceDrop)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.db.DeleteCollection(req.Namespace); err != nil {
		req.Error = fmt.Errorf("drop namespace: %w", err).Error()
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
