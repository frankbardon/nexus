// Package ingest implements a RAG ingestion plugin: chunks files, embeds
// them via the embeddings.provider capability, and upserts into the
// vector.store capability. One plugin, two entry points:
//
//   - Event mode: any plugin emits *events.RAGIngest on "rag.ingest".
//   - Watch mode: fsnotify watchers declared in config fire the same code
//     path on file writes/deletes.
//
// The CLI subcommand "nexus ingest" drives event mode from a minimal engine
// for offline bulk loads.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.rag.ingest"
	pluginName = "RAG Ingest"
	version    = "0.1.0"
)

// watchEntry is one configured directory to watch.
type watchEntry struct {
	Path      string
	Glob      string
	Namespace string
}

// Plugin handles both event-mode and watch-mode ingestion.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	chunker    *chunker
	cache      *embeddingCache
	contextual *contextualizer

	watches  []watchEntry
	watcher  *watcher // lazily started in Ready when len(watches) > 0
	backfill bool     // walk + ingest existing files at startup; on by default
	// hasLexical indicates that the search.lexical capability is active and
	// the ingest pipeline should dual-write each chunk into the lexical
	// store alongside the vector store. Captured from PluginContext.Capabilities
	// at Init so the per-ingest hot path stays free of capability lookups.
	hasLexical bool

	unsubs []func()
}

func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{Capability: "embeddings.provider"},
		{Capability: "vector.store"},
	}
}

func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	// Chunker config.
	size, overlap := 1000, 200
	if ch, ok := ctx.Config["chunker"].(map[string]any); ok {
		if v, ok := ch["size"].(int); ok {
			size = v
		}
		if v, ok := ch["overlap"].(int); ok {
			overlap = v
		}
	}
	p.chunker = newChunker(size, overlap)

	// Embedding cache. Default alongside the vector store at
	// ~/.nexus/vectors/_cache/; overridable.
	cacheDir := engine.ExpandPath("~/.nexus/vectors/_cache")
	if v, ok := ctx.Config["cache_dir"].(string); ok && v != "" {
		cacheDir = engine.ExpandPath(v)
	}
	cache, err := newEmbeddingCache(cacheDir)
	if err != nil {
		return fmt.Errorf("rag/ingest: cache: %w", err)
	}
	p.cache = cache

	// Backfill at startup (default true). Set to false when the watched
	// directories are large enough that re-walking them at every boot is
	// wasteful — note the embedding cache makes the actual embedding
	// work a no-op after the first run, so the cost is just the file
	// walk plus chunking.
	p.backfill = true
	if v, ok := ctx.Config["backfill"].(bool); ok {
		p.backfill = v
	}

	// Watch entries.
	if raw, ok := ctx.Config["watch"].([]any); ok {
		for _, r := range raw {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			e := watchEntry{}
			if v, ok := m["path"].(string); ok {
				e.Path = engine.ExpandPath(v)
			}
			if v, ok := m["glob"].(string); ok {
				e.Glob = v
			}
			if v, ok := m["namespace"].(string); ok {
				e.Namespace = v
			}
			if e.Path == "" || e.Namespace == "" {
				p.logger.Warn("skipping watch entry: path and namespace required", "entry", m)
				continue
			}
			p.watches = append(p.watches, e)
		}
	}

	p.hasLexical = len(ctx.Capabilities["search.lexical"]) > 0

	// Optional contextual retrieval: per-chunk LLM-generated situating
	// prefix to lift retrieval recall (Anthropic ~49% reduction in
	// failures on internal corpora). Disabled by default.
	contextualCfg, _ := ctx.Config["contextual_retrieval"].(map[string]any)
	contextualizer, err := newContextualizer(p.bus, p.logger, contextualCfg, cacheDir)
	if err != nil {
		return fmt.Errorf("rag/ingest: contextual: %w", err)
	}
	p.contextual = contextualizer

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("rag.ingest", p.handleIngest,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("rag.ingest.delete", p.handleDelete,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Ready() error {
	if len(p.watches) == 0 {
		return nil
	}

	// Backfill: walk each watched directory at startup and ingest any
	// pre-existing matching files. fsnotify only fires on future events,
	// so without this a freshly-pointed input_dir full of docs stays
	// invisible until the user touches each file. The embedding cache
	// (hash → vector, keyed per model) makes re-ingest at every boot a
	// no-op after the first run, so default on is safe.
	if p.backfill {
		go p.runBackfill()
	}

	w, err := newWatcher(p.logger, p.watches, func(path, ns string, removed bool) {
		if removed {
			del := &events.RAGIngestDelete{SchemaVersion: events.RAGIngestDeleteVersion, Path: path, Namespace: ns}
			_ = p.bus.Emit("rag.ingest.delete", del)
			return
		}
		req := &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: ns}
		_ = p.bus.Emit("rag.ingest", req)
	})
	if err != nil {
		return fmt.Errorf("rag/ingest: watcher: %w", err)
	}
	p.watcher = w
	return nil
}

// runBackfill walks each watch entry's path and emits rag.ingest for
// every file that matches the entry's glob. Runs in its own goroutine
// so it does not block Ready / engine boot. Skips entries whose path
// does not exist or is not a directory (with a warning) — that's the
// normal case when the user hasn't configured input_dir yet.
func (p *Plugin) runBackfill() {
	for _, e := range p.watches {
		if e.Path == "" {
			continue
		}
		info, err := os.Stat(e.Path)
		if err != nil {
			p.logger.Warn("rag/ingest backfill: skipping path", "path", e.Path, "err", err)
			continue
		}
		if !info.IsDir() {
			continue
		}

		count := 0
		walkErr := filepath.WalkDir(e.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if e.Glob != "" {
				rel, err := filepath.Rel(e.Path, path)
				if err != nil {
					rel = path
				}
				ok1, _ := filepath.Match(e.Glob, rel)
				ok2, _ := filepath.Match(e.Glob, filepath.Base(path))
				if !ok1 && !ok2 {
					return nil
				}
			}
			req := &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: e.Namespace}
			_ = p.bus.Emit("rag.ingest", req)
			count++
			return nil
		})
		if walkErr != nil {
			p.logger.Warn("rag/ingest backfill: walk failed", "path", e.Path, "err", walkErr)
			continue
		}
		p.logger.Info("rag/ingest backfill complete", "path", e.Path, "namespace", e.Namespace, "files", count)
	}
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.watcher != nil {
		p.watcher.close()
	}
	if p.contextual != nil {
		p.contextual.close()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "rag.ingest", Priority: 50},
		{EventType: "rag.ingest.delete", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"embeddings.request",
		"vector.upsert",
		"vector.delete",
		"lexical.upsert",
		"lexical.delete",
		"rag.ingest",
		"rag.ingest.delete",
		"rag.ingest.result",
	}
}

func (p *Plugin) handleIngest(event engine.Event[any]) {
	req, ok := event.Payload.(*events.RAGIngest)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if err := p.ingest(req); err != nil {
		req.Error = err.Error()
	}

	// Notification event for observers.
	_ = p.bus.Emit("rag.ingest.result", req)
}

func (p *Plugin) handleDelete(event engine.Event[any]) {
	req, ok := event.Payload.(*events.RAGIngestDelete)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if err := p.delete(req); err != nil {
		req.Error = err.Error()
	}
}

// ingest reads the file, chunks it, embeds uncached chunks via the
// embeddings.provider capability, and upserts everything into vector.store.
func (p *Plugin) ingest(req *events.RAGIngest) error {
	if req.Path == "" {
		return fmt.Errorf("path required")
	}
	if req.Namespace == "" {
		return fmt.Errorf("namespace required")
	}

	data, err := os.ReadFile(req.Path)
	if err != nil {
		return fmt.Errorf("read %q: %w", req.Path, err)
	}
	docText := string(data)
	chunks := p.chunker.chunk(docText)
	if len(chunks) == 0 {
		return nil
	}

	// Apply optional contextual retrieval: prefix each chunk with an
	// LLM-generated situating paragraph. The text used for embedding +
	// lexical indexing is `prefix + "\n\n" + chunk`; the original chunk is
	// preserved for storage so retrieved Content stays human-readable.
	embedTexts := make([]string, len(chunks))
	for i, c := range chunks {
		if p.contextual != nil {
			if prefix := p.contextual.Prefix(docText, c); prefix != "" {
				embedTexts[i] = prefix + "\n\n" + c
				continue
			}
		}
		embedTexts[i] = c
	}

	// Split into cached + uncached. Cache key uses the embed-text (which
	// includes the contextual prefix) so re-ingest with contextual on/off
	// produces independent cache entries.
	vectors := make([][]float32, len(chunks))
	missing := make([]int, 0, len(chunks))
	missingText := make([]string, 0, len(chunks))

	for i, c := range embedTexts {
		if v := p.cache.Get(c); v != nil {
			vectors[i] = v
			continue
		}
		missing = append(missing, i)
		missingText = append(missingText, c)
	}

	if len(missing) > 0 {
		embReq := &events.EmbeddingsRequest{SchemaVersion: events.EmbeddingsRequestVersion, Texts: missingText}
		_ = p.bus.Emit("embeddings.request", embReq)
		if embReq.Error != "" {
			return fmt.Errorf("embed: %s", embReq.Error)
		}
		if len(embReq.Vectors) != len(missing) {
			return fmt.Errorf("embed: expected %d vectors, got %d", len(missing), len(embReq.Vectors))
		}
		for j, idx := range missing {
			vectors[idx] = embReq.Vectors[j]
			if err := p.cache.Put(missingText[j], embReq.Vectors[j]); err != nil {
				p.logger.Debug("cache write failed (non-fatal)", "err", err)
			}
		}
	}

	ingestedAt := time.Now().UTC().Format(time.RFC3339Nano)
	docs := make([]events.VectorDoc, len(chunks))
	lexDocs := make([]events.LexicalDoc, 0, len(chunks))
	pathHash := pathKey(req.Path)
	for i, c := range chunks {
		meta := map[string]string{
			"source":      req.Path,
			"path_hash":   pathHash,
			"chunk_idx":   fmt.Sprintf("%d", i),
			"chunk_size":  fmt.Sprintf("%d", len(c)),
			"ingested_at": ingestedAt,
		}
		for k, v := range req.Metadata {
			meta[k] = v
		}
		docID := fmt.Sprintf("%s-%d", pathHash, i)
		docs[i] = events.VectorDoc{
			ID:       docID,
			Vector:   vectors[i],
			Content:  c,
			Metadata: meta,
		}
		if p.hasLexical {
			lexDocs = append(lexDocs, events.LexicalDoc{
				ID:       docID,
				Content:  c,
				Metadata: meta,
			})
		}
	}

	up := &events.VectorUpsert{SchemaVersion: events.VectorUpsertVersion, Namespace: req.Namespace, Docs: docs}
	_ = p.bus.Emit("vector.upsert", up)
	if up.Error != "" {
		return fmt.Errorf("upsert: %s", up.Error)
	}

	if p.hasLexical && len(lexDocs) > 0 {
		lexUp := &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: req.Namespace, Docs: lexDocs}
		_ = p.bus.Emit("lexical.upsert", lexUp)
		if lexUp.Error != "" {
			return fmt.Errorf("lexical upsert: %s", lexUp.Error)
		}
	}

	req.Chunks = len(chunks)
	req.SkippedCached = len(chunks) - len(missing)

	p.logger.Info("ingested",
		"path", req.Path,
		"namespace", req.Namespace,
		"chunks", req.Chunks,
		"skipped_cached", req.SkippedCached,
		"lexical", p.hasLexical,
	)
	return nil
}

// delete removes every chunk whose ID starts with the path hash for this
// file, by asking the vector store to drop by the deterministic ID scheme
// we use on ingest. We don't know how many chunks were stored, so we delete
// a generous upper bound; chromem ignores unknown IDs.
func (p *Plugin) delete(req *events.RAGIngestDelete) error {
	if req.Path == "" || req.Namespace == "" {
		return fmt.Errorf("path and namespace required")
	}
	const maxChunks = 4096
	pathHash := pathKey(req.Path)
	ids := make([]string, 0, maxChunks)
	for i := 0; i < maxChunks; i++ {
		ids = append(ids, fmt.Sprintf("%s-%d", pathHash, i))
	}
	del := &events.VectorDelete{SchemaVersion: events.VectorDeleteVersion, Namespace: req.Namespace, IDs: ids}
	_ = p.bus.Emit("vector.delete", del)
	if del.Error != "" {
		return fmt.Errorf("delete: %s", del.Error)
	}
	if p.hasLexical {
		lexDel := &events.LexicalDelete{SchemaVersion: events.LexicalDeleteVersion, Namespace: req.Namespace, IDs: ids}
		_ = p.bus.Emit("lexical.delete", lexDel)
		if lexDel.Error != "" {
			return fmt.Errorf("lexical delete: %s", lexDel.Error)
		}
	}
	p.logger.Info("deleted file chunks", "path", req.Path, "namespace", req.Namespace)
	return nil
}

func pathKey(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:16]
}
