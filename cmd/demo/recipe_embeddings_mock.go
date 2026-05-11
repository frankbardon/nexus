package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

//go:embed recipe-embeddings-mock.yaml
var embeddingsMockConfig []byte

// runEmbeddingsMockRecipe boots the engine with the embeddings-mock
// config and exercises the full ingest → vector store → knowledge_search
// pipeline without touching a paid API.
//
// What it shows the operator:
//
//   - The engine bus + plugin system end-to-end without LLM calls.
//   - That swapping `nexus.embeddings.openai` for `nexus.embeddings.mock`
//     in YAML is a single-line change — the rest of the RAG stack
//     (chromem, rag/ingest, knowledge_search) is provider-agnostic.
//   - Deterministic vector output: re-running the recipe produces
//     identical similarity scores, which is what makes the mock useful
//     in CI.
//
// Failure modes surfaced:
//   - chromem path collisions if a prior run left files behind (we use
//     /tmp/nexus-recipe-embeddings-mock and rely on the user to clean
//     up between runs — the recipe doesn't auto-wipe).
//   - The "no models block" engine constraint: even though no LLM is
//     called, the engine refuses to boot without one. The YAML
//     documents the workaround (stub anthropic config).
func runEmbeddingsMockRecipe(ctx context.Context, args []string) error {
	eng, err := bootRecipeEngine(ctx, embeddingsMockConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = eng.Stop(context.Background())
	}()

	if err := eng.StartSession(); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// rag.ingest requires a path on disk; the ingest plugin reads from
	// it. Write the sample doc to a temp file then point the event at
	// that path. (The recipe is self-contained — the file lives under
	// /tmp and is removed when the recipe exits.)
	tmpDir, err := os.MkdirTemp("", "nexus-recipe-embeddings-mock-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	docPath := filepath.Join(tmpDir, "inline-doc.md")
	if err := os.WriteFile(docPath, []byte(sampleDocText), 0o600); err != nil {
		return fmt.Errorf("write sample doc: %w", err)
	}

	// Pointer-payload pattern: ingest plugin fills Chunks / Error in
	// place before Emit returns. We log Chunks so the operator can see
	// the pipeline produced output.
	req := &events.RAGIngest{
		SchemaVersion: events.RAGIngestVersion,
		Namespace:     "recipe-test",
		Path:          docPath,
	}
	if err := eng.Bus.Emit("rag.ingest", req); err != nil {
		return fmt.Errorf("ingest emit: %w", err)
	}
	if req.Error != "" {
		return fmt.Errorf("ingest plugin error: %s", req.Error)
	}

	// rag.ingest's Emit is synchronous when the ingest plugin is the
	// only handler — chunks are upserted before Emit returns. The
	// short sleep below covers any registered observer that might
	// still be processing rag.ingest.result asynchronously.
	time.Sleep(100 * time.Millisecond)

	fmt.Println("Recipe: embeddings-mock")
	fmt.Printf("  Ingested doc: %s\n", docPath)
	fmt.Printf("  Provider:     %s\n", req.Provider)
	fmt.Printf("  Chunks:       %d\n", req.Chunks)
	fmt.Printf("  Cached:       %d\n", req.SkippedCached)
	fmt.Println("  Namespace:    recipe-test")
	fmt.Println("  Vectors:      /tmp/nexus-recipe-embeddings-mock/vectors")
	fmt.Println()
	fmt.Println("Exiting cleanly. Re-run to confirm the mock embedder")
	fmt.Println("produces identical vectors (hash-based, deterministic).")

	return nil
}

// sampleDocText is the inline document the recipe ingests. Kept short
// so the recipe finishes in well under a second.
const sampleDocText = `# Sample Doc for the embeddings-mock recipe

This document exists only to verify the ingest → embed → store →
retrieve path works end-to-end with no LLM provider configured.

The mock embedder hashes the input string and projects it into a
fixed-dimensional vector space. Two ingestions of identical text
produce identical vectors — useful for CI assertions but useless
for actual semantic search.

To swap in real embeddings, change ` + "`nexus.embeddings.mock`" + `
to ` + "`nexus.embeddings.openai`" + ` in the recipe YAML and provide
an OPENAI_API_KEY env var.
`
