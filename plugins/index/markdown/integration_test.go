package markdownindex_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
	markdownindex "github.com/frankbardon/nexus/plugins/index/markdown"
	sqlitefts "github.com/frankbardon/nexus/plugins/vectorstore/sqlite_fts"
)

// TestIndexThenQuery wires the real sqlite_fts lexical store and the docs
// indexer on one bus, indexes a temp dir of fixtures, and asserts a BM25 query
// returns the right section — the pure lexical path, no embeddings.
func TestIndexThenQuery(t *testing.T) {
	docsDir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(docsDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	write("bera_score.md", "---\ntitle: \"BERA Score\"\ntype: reference\ntags:\n  - bera-metrics\n---\n"+
		"# BERA Score\n\nIntro.\n\n"+
		"## How It Is Calculated\n\nThe BERA Score is a percentile rank from 0 to 100 computed across the panel.\n\n"+
		"## Why This Matters\n\nIt summarizes brand equity in one number.\n")
	write("today_score.md", "---\ntitle: \"Today Score\"\ntype: reference\ntags:\n  - bera-metrics\n---\n"+
		"# Today Score\n\n## Definition\n\nToday Score combines Familiarity and Regard to show current brand health.\n")

	store := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := engine.NewEventBus()

	root := t.TempDir()
	mgr := storage.NewManager(root, "", func() string {
		return filepath.Join(root, "sessions", "test")
	}, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	// Lexical store: provides search.lexical.
	lex := sqlitefts.New()
	lexCtx := engine.PluginContext{
		Bus:      bus,
		Logger:   store,
		PluginID: "nexus.vectorstore.sqlite_fts",
		Storage: func(scope storage.Scope) (storage.Storage, error) {
			return mgr.Open(scope, "nexus.vectorstore.sqlite_fts")
		},
		Config: map[string]any{"scope": "session"},
	}
	if err := lex.Init(lexCtx); err != nil {
		t.Fatalf("lex init: %v", err)
	}
	t.Cleanup(func() { _ = lex.Shutdown(context.Background()) })

	// Indexer: depends on the search.lexical capability being live.
	idx := markdownindex.New()
	idxCtx := engine.PluginContext{
		Bus:          bus,
		Logger:       store,
		PluginID:     "nexus.index.markdown",
		Config:       map[string]any{"docs_dir": docsDir, "namespace": "docs", "glob": "*.md"},
		Capabilities: map[string][]string{"search.lexical": {"nexus.vectorstore.sqlite_fts"}},
	}
	if err := idx.Init(idxCtx); err != nil {
		t.Fatalf("idx init: %v", err)
	}
	if err := lex.Ready(); err != nil {
		t.Fatalf("lex ready: %v", err)
	}
	if err := idx.Ready(); err != nil { // walks docsDir, emits lexical.upsert
		t.Fatalf("idx ready: %v", err)
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "docs", Query: "percentile rank calculated", K: 5}
	if err := bus.Emit("lexical.query", q); err != nil {
		t.Fatalf("emit query: %v", err)
	}
	if q.Error != "" {
		t.Fatalf("query error: %s", q.Error)
	}
	if len(q.Matches) == 0 {
		t.Fatalf("expected matches for calculation query")
	}
	top := q.Matches[0]
	if top.Metadata["source"] != "bera_score.md" {
		t.Errorf("top source = %q, want bera_score.md", top.Metadata["source"])
	}
	if top.Metadata["heading"] != "How It Is Calculated" {
		t.Errorf("top heading = %q, want 'How It Is Calculated'", top.Metadata["heading"])
	}

	// A term unique to the other doc routes to it.
	q2 := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "docs", Query: "Familiarity Regard current health", K: 5}
	if err := bus.Emit("lexical.query", q2); err != nil {
		t.Fatalf("emit query2: %v", err)
	}
	if len(q2.Matches) == 0 || q2.Matches[0].Metadata["source"] != "today_score.md" {
		t.Fatalf("expected today_score.md top hit, got %+v", q2.Matches)
	}
}
