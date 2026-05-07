package sqlitefts

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin builds a plugin wired to a tempdir-backed storage manager.
// Returns the bus (so the test can emit events at it) and a cleanup func.
func newTestPlugin(t *testing.T) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	root := t.TempDir()
	mgr := storage.NewManager(root, "", func() string {
		return filepath.Join(root, "sessions", "test")
	}, nil)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := engine.NewEventBus()

	p := New().(*Plugin)
	ctx := engine.PluginContext{
		Bus:      bus,
		Logger:   logger,
		PluginID: pluginID,
		Storage: func(scope storage.Scope) (storage.Storage, error) {
			return mgr.Open(scope, pluginID)
		},
		Config: map[string]any{"scope": "session"},
	}
	if err := p.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	cleanup := func() {
		_ = p.Shutdown(context.Background())
		_ = mgr.Close()
	}
	return p, bus, cleanup
}

func TestUpsertAndQuery(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	docs := []events.LexicalDoc{
		{ID: "1", Content: "the quick brown fox jumps over the lazy dog",
			Metadata: map[string]string{"source": "/tmp/a.txt", "chunk_idx": "0"}},
		{ID: "2", Content: "ENOENT means the file does not exist",
			Metadata: map[string]string{"source": "/tmp/b.txt", "chunk_idx": "0"}},
		{ID: "3", Content: "func parseHeader returns the parsed HTTP header",
			Metadata: map[string]string{"source": "/tmp/c.go", "chunk_idx": "0"}},
	}
	up := &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: "test", Docs: docs}
	if err := bus.Emit("lexical.upsert", up); err != nil {
		t.Fatalf("emit upsert: %v", err)
	}
	if up.Error != "" {
		t.Fatalf("upsert error: %s", up.Error)
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "test", Query: "ENOENT", K: 5}
	if err := bus.Emit("lexical.query", q); err != nil {
		t.Fatalf("emit query: %v", err)
	}
	if q.Error != "" {
		t.Fatalf("query error: %s", q.Error)
	}
	if len(q.Matches) == 0 {
		t.Fatalf("expected at least one match for ENOENT")
	}
	if q.Matches[0].ID != "2" {
		t.Fatalf("top hit ID = %q, want 2", q.Matches[0].ID)
	}
	if q.Matches[0].Metadata["source"] != "/tmp/b.txt" {
		t.Fatalf("source metadata not propagated: %q", q.Matches[0].Metadata["source"])
	}
}

func TestQueryReturnsBM25RankedHits(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	docs := []events.LexicalDoc{
		{ID: "1", Content: "alpha beta gamma", Metadata: map[string]string{"source": "a"}},
		{ID: "2", Content: "alpha alpha alpha beta", Metadata: map[string]string{"source": "b"}},
		{ID: "3", Content: "delta epsilon zeta", Metadata: map[string]string{"source": "c"}},
	}
	up := &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: "test", Docs: docs}
	if err := bus.Emit("lexical.upsert", up); err != nil {
		t.Fatalf("emit upsert: %v", err)
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "test", Query: "alpha", K: 5}
	if err := bus.Emit("lexical.query", q); err != nil {
		t.Fatalf("emit query: %v", err)
	}
	if len(q.Matches) < 2 {
		t.Fatalf("expected at least 2 matches, got %d", len(q.Matches))
	}
	// Doc 2 has higher term frequency for "alpha" — should rank above doc 1.
	if q.Matches[0].ID != "2" {
		t.Fatalf("BM25 top hit = %q, want 2 (higher tf)", q.Matches[0].ID)
	}
}

func TestNamespaceIsolation(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	for _, ns := range []string{"a", "b"} {
		_ = bus.Emit("lexical.upsert", &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: ns,
			Docs: []events.LexicalDoc{{ID: "1", Content: "marker " + ns,
				Metadata: map[string]string{"source": ns}}},
		})
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "a", Query: "marker", K: 5}
	_ = bus.Emit("lexical.query", q)
	if len(q.Matches) != 1 || q.Matches[0].Metadata["source"] != "a" {
		t.Fatalf("namespace a got %v", q.Matches)
	}
}

func TestUpsertReplacesExistingID(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	for _, content := range []string{"original content xyz", "replacement content xyz"} {
		_ = bus.Emit("lexical.upsert", &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: "test",
			Docs: []events.LexicalDoc{{ID: "1", Content: content,
				Metadata: map[string]string{"source": "a"}}},
		})
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "test", Query: "xyz", K: 5}
	_ = bus.Emit("lexical.query", q)
	if len(q.Matches) != 1 {
		t.Fatalf("expected 1 hit after replace, got %d", len(q.Matches))
	}
	if q.Matches[0].Content != "replacement content xyz" {
		t.Fatalf("replace did not stick: content = %q", q.Matches[0].Content)
	}
}

func TestDeleteByID(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	_ = bus.Emit("lexical.upsert", &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: "test",
		Docs: []events.LexicalDoc{
			{ID: "1", Content: "keep me", Metadata: map[string]string{"source": "a"}},
			{ID: "2", Content: "delete me", Metadata: map[string]string{"source": "b"}},
		},
	})

	del := &events.LexicalDelete{SchemaVersion: events.LexicalDeleteVersion, Namespace: "test", IDs: []string{"2"}}
	_ = bus.Emit("lexical.delete", del)
	if del.Error != "" {
		t.Fatalf("delete error: %s", del.Error)
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "test", Query: "me", K: 5}
	_ = bus.Emit("lexical.query", q)
	if len(q.Matches) != 1 || q.Matches[0].ID != "1" {
		t.Fatalf("expected only doc 1 after delete, got %v", q.Matches)
	}
}

func TestNamespaceDrop(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	_ = bus.Emit("lexical.upsert", &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: "drop_me",
		Docs: []events.LexicalDoc{{ID: "1", Content: "marker",
			Metadata: map[string]string{"source": "a"}}},
	})

	drop := &events.LexicalNamespaceDrop{SchemaVersion: events.LexicalNamespaceDropVersion, Namespace: "drop_me"}
	_ = bus.Emit("lexical.namespace.drop", drop)
	if drop.Error != "" {
		t.Fatalf("drop error: %s", drop.Error)
	}

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "drop_me", Query: "marker", K: 5}
	_ = bus.Emit("lexical.query", q)
	if len(q.Matches) != 0 {
		t.Fatalf("expected 0 matches after drop, got %d", len(q.Matches))
	}
}

func TestUnknownNamespaceQuery(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "never_existed", Query: "anything", K: 5}
	if err := bus.Emit("lexical.query", q); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if q.Error != "" {
		t.Fatalf("unknown namespace should not error, got %q", q.Error)
	}
	if len(q.Matches) != 0 {
		t.Fatalf("unknown namespace should return zero matches, got %d", len(q.Matches))
	}
}

func TestSanitizedQueryToleratesSpecialCharacters(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	_ = bus.Emit("lexical.upsert", &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: "test",
		Docs: []events.LexicalDoc{{ID: "1", Content: "func parseHeader returns header",
			Metadata: map[string]string{"source": "a"}}},
	})

	// FTS5 special chars: parens / quotes / colon / asterisk
	q := &events.LexicalQuery{SchemaVersion: events.LexicalQueryVersion, Namespace: "test", Query: `"parseHeader" : (returns)`, K: 5}
	if err := bus.Emit("lexical.query", q); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if q.Error != "" {
		t.Fatalf("query: %s", q.Error)
	}
	if len(q.Matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(q.Matches))
	}
}
