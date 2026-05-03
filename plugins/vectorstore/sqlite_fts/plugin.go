// Package sqlitefts implements the search.lexical capability backed by
// SQLite FTS5. Each namespace becomes a virtual FTS5 table; ingestion writes
// chunks alongside namespace, doc_id, and metadata fields. Queries return
// BM25-ranked hits with metadata.
//
// The store consumes the engine's per-plugin storage capability
// (PluginContext.Storage). Default scope is session (per-conversation
// corpus); flip to agent or app scope via the `scope:` config knob for
// long-lived knowledge bases.
package sqlitefts

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.vectorstore.sqlite_fts"
	pluginName = "SQLite FTS5 Lexical Store"
	version    = "0.1.0"
)

// Plugin advertises the search.lexical capability. Namespaces map 1:1 to
// FTS5 virtual tables inside the plugin's scoped store.db.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	store  storage.Storage
	scope  storage.Scope

	mu              sync.Mutex
	namespacesReady map[string]bool
	unsubs          []func()
}

func New() engine.Plugin {
	return &Plugin{namespacesReady: make(map[string]bool)}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "search.lexical",
		Description: "Namespace-aware BM25 lexical search (SQLite FTS5).",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.scope = parseScope(ctx.Config["scope"])

	if ctx.Storage == nil {
		return fmt.Errorf("vectorstore/sqlite_fts: PluginContext.Storage is nil — engine wiring missing")
	}
	st, err := ctx.Storage(p.scope)
	if err != nil {
		return fmt.Errorf("vectorstore/sqlite_fts: open storage: %w", err)
	}
	p.store = st

	if _, err := p.store.DB().Exec(`CREATE TABLE IF NOT EXISTS lexical_namespaces (
		name TEXT PRIMARY KEY,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("vectorstore/sqlite_fts: create registry: %w", err)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("lexical.upsert", p.handleUpsert,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("lexical.query", p.handleQuery,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("lexical.delete", p.handleDelete,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("lexical.namespace.drop", p.handleDropNamespace,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("vectorstore/sqlite_fts ready", "scope", p.scope.String())
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
		{EventType: "lexical.upsert", Priority: 50},
		{EventType: "lexical.query", Priority: 50},
		{EventType: "lexical.delete", Priority: 50},
		{EventType: "lexical.namespace.drop", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string { return nil }

// parseScope converts the config "scope" string into a storage.Scope. Default
// is session. Unknown values fall back to session with a logged warning the
// next time the field is read (no logger here at parse time).
func parseScope(v any) storage.Scope {
	s, _ := v.(string)
	switch strings.ToLower(s) {
	case "agent":
		return storage.ScopeAgent
	case "app":
		return storage.ScopeApp
	default:
		return storage.ScopeSession
	}
}

// ensureNamespace creates the per-namespace FTS5 table on first use. The
// table layout encodes provenance fields as UNINDEXED so they do not pollute
// BM25 ranking but are returned with each match.
func (p *Plugin) ensureNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.namespacesReady[ns] {
		return nil
	}

	tbl := tableName(ns)
	stmt := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(
		doc_id UNINDEXED,
		source UNINDEXED,
		path_hash UNINDEXED,
		chunk_idx UNINDEXED,
		ingested_at UNINDEXED,
		metadata UNINDEXED,
		content,
		tokenize='unicode61 remove_diacritics 2'
	)`, tbl)
	if _, err := p.store.DB().Exec(stmt); err != nil {
		return fmt.Errorf("create %s: %w", tbl, err)
	}
	if _, err := p.store.DB().Exec(
		`INSERT OR IGNORE INTO lexical_namespaces(name) VALUES(?)`, ns,
	); err != nil {
		return fmt.Errorf("register %q: %w", ns, err)
	}
	p.namespacesReady[ns] = true
	return nil
}

// tableName turns a user-supplied namespace into a safe SQL identifier.
// FTS5 rejects names with spaces / hyphens / dots, so we hash-prefix and
// keep only alphanumerics + underscore. Two distinct namespace strings
// could theoretically collide; in practice the prefix dominates.
func tableName(ns string) string {
	var b strings.Builder
	b.WriteString("lex_")
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (p *Plugin) handleUpsert(event engine.Event[any]) {
	req, ok := event.Payload.(*events.LexicalUpsert)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if err := p.ensureNamespace(req.Namespace); err != nil {
		req.Error = err.Error()
		return
	}
	if len(req.Docs) == 0 {
		return
	}

	tbl := tableName(req.Namespace)
	err := p.store.Tx(func(tx *sql.Tx) error {
		// Delete-then-insert per ID gives upsert semantics on FTS5 virtual
		// tables, which do not support ON CONFLICT.
		delStmt, err := tx.Prepare(fmt.Sprintf(
			`DELETE FROM %s WHERE doc_id = ?`, tbl))
		if err != nil {
			return fmt.Errorf("prepare delete: %w", err)
		}
		defer delStmt.Close()

		insStmt, err := tx.Prepare(fmt.Sprintf(`INSERT INTO %s
			(doc_id, source, path_hash, chunk_idx, ingested_at, metadata, content)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, tbl))
		if err != nil {
			return fmt.Errorf("prepare insert: %w", err)
		}
		defer insStmt.Close()

		for _, d := range req.Docs {
			if _, err := delStmt.Exec(d.ID); err != nil {
				return fmt.Errorf("delete %q: %w", d.ID, err)
			}
			meta := d.Metadata
			if _, err := insStmt.Exec(
				d.ID,
				meta["source"],
				meta["path_hash"],
				meta["chunk_idx"],
				meta["ingested_at"],
				flattenMetadata(meta),
				d.Content,
			); err != nil {
				return fmt.Errorf("insert %q: %w", d.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		req.Error = err.Error()
	}
}

func (p *Plugin) handleQuery(event engine.Event[any]) {
	req, ok := event.Payload.(*events.LexicalQuery)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if req.Namespace == "" || req.Query == "" {
		return
	}
	// Missing namespace is not an error — empty results.
	tbl := tableName(req.Namespace)
	if !p.tableExists(tbl) {
		return
	}

	k := req.K
	if k <= 0 {
		k = 5
	}

	rows, err := p.store.DB().Query(fmt.Sprintf(`
		SELECT doc_id, source, path_hash, chunk_idx, ingested_at, metadata, content,
		       bm25(%s) AS score
		FROM %s
		WHERE %s MATCH ?
		ORDER BY bm25(%s)
		LIMIT ?`, tbl, tbl, tbl, tbl), sanitizeMatch(req.Query), k)
	if err != nil {
		req.Error = fmt.Errorf("query: %w", err).Error()
		return
	}
	defer rows.Close()

	matches := make([]events.LexicalMatch, 0, k)
	for rows.Next() {
		var (
			id, source, pathHash, chunkIdx, ingestedAt, metaStr, content string
			score                                                        float64
		)
		if err := rows.Scan(&id, &source, &pathHash, &chunkIdx, &ingestedAt, &metaStr, &content, &score); err != nil {
			req.Error = fmt.Errorf("scan: %w", err).Error()
			return
		}
		meta := unflattenMetadata(metaStr)
		if source != "" {
			meta["source"] = source
		}
		if pathHash != "" {
			meta["path_hash"] = pathHash
		}
		if chunkIdx != "" {
			meta["chunk_idx"] = chunkIdx
		}
		if ingestedAt != "" {
			meta["ingested_at"] = ingestedAt
		}
		// FTS5 bm25 returns a negative-of-relevance score (lower is better
		// after ORDER BY bm25(table)). Flip the sign so consumers that
		// treat higher == better behave consistently with vector similarity.
		matches = append(matches, events.LexicalMatch{
			ID:       id,
			Content:  content,
			Metadata: meta,
			Score:    float32(-score),
		})
	}
	if err := rows.Err(); err != nil {
		req.Error = err.Error()
		return
	}
	req.Matches = matches
}

func (p *Plugin) handleDelete(event engine.Event[any]) {
	req, ok := event.Payload.(*events.LexicalDelete)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if req.Namespace == "" || len(req.IDs) == 0 {
		return
	}
	tbl := tableName(req.Namespace)
	if !p.tableExists(tbl) {
		return
	}

	err := p.store.Tx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(fmt.Sprintf(`DELETE FROM %s WHERE doc_id = ?`, tbl))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, id := range req.IDs {
			if _, err := stmt.Exec(id); err != nil {
				return fmt.Errorf("delete %q: %w", id, err)
			}
		}
		return nil
	})
	if err != nil {
		req.Error = err.Error()
	}
}

func (p *Plugin) handleDropNamespace(event engine.Event[any]) {
	req, ok := event.Payload.(*events.LexicalNamespaceDrop)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if req.Namespace == "" {
		return
	}
	tbl := tableName(req.Namespace)

	p.mu.Lock()
	delete(p.namespacesReady, req.Namespace)
	p.mu.Unlock()

	if _, err := p.store.DB().Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tbl)); err != nil {
		req.Error = err.Error()
		return
	}
	if _, err := p.store.DB().Exec(`DELETE FROM lexical_namespaces WHERE name = ?`, req.Namespace); err != nil {
		req.Error = err.Error()
	}
}

// tableExists returns true when the FTS5 virtual table for a namespace
// has been created. Avoids the query path failing on unknown namespaces.
func (p *Plugin) tableExists(tbl string) bool {
	var count int
	err := p.store.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, tbl,
	).Scan(&count)
	return err == nil && count > 0
}

// flattenMetadata serializes a string→string map into a stable key=value
// representation. Lossless if no key contains '\x1f' or value contains '\x1e'.
// FTS5 cannot index a structured map, so we keep metadata as an UNINDEXED
// blob field.
func flattenMetadata(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Stable order so two equal maps serialize identically.
	sortStrings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(0x1e)
		}
		b.WriteString(k)
		b.WriteByte(0x1f)
		b.WriteString(m[k])
	}
	return b.String()
}

func unflattenMetadata(s string) map[string]string {
	out := make(map[string]string)
	if s == "" {
		return out
	}
	for _, pair := range strings.Split(s, "\x1e") {
		kv := strings.SplitN(pair, "\x1f", 2)
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
		}
	}
	return out
}

// sanitizeMatch turns a free-form query string into something FTS5 will
// accept. FTS5 treats unquoted strings as token sequences with implicit
// AND; problematic characters (parens, colons, double quotes, slashes)
// are replaced with spaces so an arbitrary user query never trips the
// tokenizer. For phrase / advanced syntax, callers can pre-quote.
func sanitizeMatch(q string) string {
	var b strings.Builder
	for _, r := range q {
		switch r {
		case '(', ')', '"', ':', '/', '\\', '*':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// sortStrings is a tiny sort.Strings replacement to avoid an extra import.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
