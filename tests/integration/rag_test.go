//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestRAG_IngestAndQuery covers the primary RAG path end-to-end without any
// LLM involvement: ingest a file, embed-cache its chunks, query the vector
// store using one of the chunks as the query — expect a self-match with
// similarity close to 1.0 (the mock embedder maps identical text to
// identical vectors).
func TestRAG_IngestAndQuery(t *testing.T) {
	eng, vectorDir := bootRAGEngine(t)
	defer teardown(eng)

	// Single short line — ingest produces exactly one chunk equal to the
	// trimmed file content, so we can query with that same string and hit
	// a bit-for-bit identical vector (similarity = 1.0).
	const body = "feline mammals purr softly in sunlight"
	path := writeTestFile(t, "cats.md", body)

	req := &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: "kb"}
	if err := eng.Bus.Emit("rag.ingest", req); err != nil {
		t.Fatalf("emit rag.ingest: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("ingest error: %s", req.Error)
	}
	if req.Chunks != 1 {
		t.Fatalf("expected 1 chunk for short text, got %d", req.Chunks)
	}

	q := &events.VectorQuery{SchemaVersion: events.VectorQueryVersion, Namespace: "kb",
		Vector: mockVecFor(t, eng, body),
		K:      3,
	}
	if err := eng.Bus.Emit("vector.query", q); err != nil {
		t.Fatalf("emit vector.query: %v", err)
	}
	if q.Error != "" {
		t.Fatalf("query error: %s", q.Error)
	}
	if len(q.Matches) == 0 {
		t.Fatalf("expected ≥1 match, got 0")
	}
	if q.Matches[0].Similarity < 0.99 {
		t.Errorf("expected self-match similarity ≈1.0, got %f", q.Matches[0].Similarity)
	}
	if q.Matches[0].Metadata["source"] != path {
		t.Errorf("expected source=%q, got %q", path, q.Matches[0].Metadata["source"])
	}

	// Make sure the vector store file actually landed on disk.
	entries, err := os.ReadDir(vectorDir)
	if err != nil {
		t.Fatalf("read vector dir: %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("expected chromem to persist at least one entry under %s", vectorDir)
	}
}

// TestRAG_EmbeddingCache covers re-ingest: the second pass must resolve every
// chunk from the content-hash cache rather than calling the embeddings
// provider again.
func TestRAG_EmbeddingCache(t *testing.T) {
	eng, _ := bootRAGEngine(t)
	defer teardown(eng)

	path := writeTestFile(t, "dogs.md", strings.Repeat("canine loyal companion. ", 40))

	first := &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: "kb"}
	_ = eng.Bus.Emit("rag.ingest", first)
	if first.Error != "" {
		t.Fatalf("first ingest: %s", first.Error)
	}
	if first.SkippedCached != 0 {
		t.Errorf("expected zero cache hits on first ingest, got %d", first.SkippedCached)
	}

	second := &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: "kb"}
	_ = eng.Bus.Emit("rag.ingest", second)
	if second.Error != "" {
		t.Fatalf("second ingest: %s", second.Error)
	}
	if second.Chunks != first.Chunks {
		t.Errorf("expected %d chunks on re-ingest, got %d", first.Chunks, second.Chunks)
	}
	if second.SkippedCached != second.Chunks {
		t.Errorf("expected all %d chunks cached on re-ingest, got %d", second.Chunks, second.SkippedCached)
	}
}

// TestRAG_IngestDelete verifies that rag.ingest.delete drops every chunk
// produced from a path, so subsequent queries return nothing from that
// file even if other files still populate the namespace.
func TestRAG_IngestDelete(t *testing.T) {
	eng, _ := bootRAGEngine(t)
	defer teardown(eng)

	path := writeTestFile(t, "ephemeral.md", "this content will be removed")
	_ = eng.Bus.Emit("rag.ingest", &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: "kb"})

	q := &events.VectorQuery{SchemaVersion: events.VectorQueryVersion, Namespace: "kb",
		Vector: mockVecFor(t, eng, "this content will be removed"),
		K:      3,
	}
	_ = eng.Bus.Emit("vector.query", q)
	if len(q.Matches) == 0 {
		t.Fatalf("expected ≥1 match before delete, got 0")
	}

	del := &events.RAGIngestDelete{SchemaVersion: events.RAGIngestDeleteVersion, Path: path, Namespace: "kb"}
	_ = eng.Bus.Emit("rag.ingest.delete", del)
	if del.Error != "" {
		t.Fatalf("delete: %s", del.Error)
	}

	q2 := &events.VectorQuery{SchemaVersion: events.VectorQueryVersion, Namespace: "kb",
		Vector: mockVecFor(t, eng, "this content will be removed"),
		K:      3,
	}
	_ = eng.Bus.Emit("vector.query", q2)
	for _, m := range q2.Matches {
		if m.Metadata["source"] == path {
			t.Errorf("expected no hits for deleted path, found %q (sim %f)", path, m.Similarity)
		}
	}
}

// TestRAG_NamespaceIsolation verifies that queries against one namespace
// never leak documents from another namespace.
func TestRAG_NamespaceIsolation(t *testing.T) {
	eng, _ := bootRAGEngine(t)
	defer teardown(eng)

	p1 := writeTestFile(t, "kb-only.md", "namespace KB unique phrase alpha")
	p2 := writeTestFile(t, "docs-only.md", "namespace DOCS unique phrase beta")

	_ = eng.Bus.Emit("rag.ingest", &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: p1, Namespace: "kb"})
	_ = eng.Bus.Emit("rag.ingest", &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: p2, Namespace: "docs"})

	q := &events.VectorQuery{SchemaVersion: events.VectorQueryVersion, Namespace: "kb",
		Vector: mockVecFor(t, eng, "namespace DOCS unique phrase beta"),
		K:      5,
	}
	_ = eng.Bus.Emit("vector.query", q)
	for _, m := range q.Matches {
		if m.Metadata["source"] == p2 {
			t.Errorf("namespace leak: found %q in kb query", p2)
		}
	}

	// Drop the docs namespace; kb should still answer.
	drop := &events.VectorNamespaceDrop{SchemaVersion: events.VectorNamespaceDropVersion, Namespace: "docs"}
	_ = eng.Bus.Emit("vector.namespace.drop", drop)
	if drop.Error != "" {
		t.Fatalf("drop namespace: %s", drop.Error)
	}

	qDocs := &events.VectorQuery{SchemaVersion: events.VectorQueryVersion, Namespace: "docs",
		Vector: mockVecFor(t, eng, "anything"),
		K:      5,
	}
	_ = eng.Bus.Emit("vector.query", qDocs)
	if len(qDocs.Matches) != 0 {
		t.Errorf("expected no matches in dropped namespace, got %d", len(qDocs.Matches))
	}
}

// --- helpers ---

// bootRAGEngine boots a minimal engine wired with mock embeddings, chromem
// at a temp path, and the ingest plugin. Returns the engine and the vector
// store path (so callers can assert on persistence).
func bootRAGEngine(t *testing.T) (*engine.Engine, string) {
	t.Helper()
	vectorDir := t.TempDir()
	cacheDir := t.TempDir()
	yaml := fmt.Sprintf(`core:
  log_level: warn
plugins:
  active:
    - nexus.embeddings.mock
    - nexus.vectorstore.chromem
    - nexus.rag.ingest
  nexus.vectorstore.chromem:
    path: %q
  nexus.rag.ingest:
    chunker:
      size: 400
      overlap: 80
    cache_dir: %q
`, vectorDir, cacheDir)

	eng, err := engine.NewFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	allplugins.RegisterAll(eng.Registry)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	return eng, vectorDir
}

func teardown(eng *engine.Engine) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = eng.Stop(ctx)
}

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// mockVecFor asks the mock embedder for the vector of a given text, going
// through the bus so tests don't reach into the provider directly.
func mockVecFor(t *testing.T, eng *engine.Engine, text string) []float32 {
	t.Helper()
	req := &events.EmbeddingsRequest{SchemaVersion: events.EmbeddingsRequestVersion, Texts: []string{text}}
	if err := eng.Bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit embeddings.request: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("embed: %s", req.Error)
	}
	if len(req.Vectors) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(req.Vectors))
	}
	return req.Vectors[0]
}
