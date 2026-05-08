//go:build integration

// Cross-provider RAG integration test.
//
// Spec (T3.4): ingest documents, route retrieval through one provider's
// embeddings, synthesize an answer with another provider's LLM. Assert
// rag.retrieved + llm.response.cited fire.
//
// Simplification vs the original spec: the spec implies "two distinct LLM
// providers — embeddings from one, synthesis from another". No shipped
// embedding adapter is mock-friendly except nexus.embeddings.mock, so
// running, e.g., OpenAI embeddings + Anthropic synthesis would require live
// API keys and defeat the mock-mode constraint. Instead we exercise two
// distinct provider plugins:
//
//   - nexus.embeddings.mock      — supplies vectors for ingest + query embed
//   - nexus.llm.openai (mocked)  — supplies the synthesis LLM
//
// That keeps "cross-provider" honest at the plugin/capability level (two
// separate provider plugins handle the two halves of the RAG pipeline)
// without forcing two real LLM vendors into the same offline test.

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestCrossProviderRAG_IngestRetrieveCite drives the full cross-provider RAG
// pipeline end-to-end:
//
//  1. Pre-populate a knowledge base file on disk.
//  2. Override the chromem path and cache_dir to per-test temp dirs.
//  3. Patch the second mock LLM response so its <cite source="..."/> points
//     at the ingested file's absolute path (so the citation parser sees a
//     real source value).
//  4. Subscribe pre-Run on io.session.start to emit rag.ingest synchronously
//     once plugins are wired up but before the agent's first turn fires.
//  5. Run the harness. Mock turn 1 issues a knowledge_search tool call;
//     turn 2 emits the cited synthesis answer.
//  6. Assert rag.retrieved + llm.response.cited both fire and that the
//     cited response carries our citation back through.
func TestCrossProviderRAG_IngestRetrieveCite(t *testing.T) {
	// 1. Pre-populate the knowledge base with one short document. The body
	// stays short so the chunker emits a single chunk we can deterministically
	// reference from the mock response.
	const kbBody = "Nexus is a modular AI agent harness written in pure Go."
	kbDir := t.TempDir()
	kbPath := filepath.Join(kbDir, "nexus.md")
	if err := os.WriteFile(kbPath, []byte(kbBody), 0o644); err != nil {
		t.Fatalf("write kb file: %v", err)
	}

	// 2. Per-test vector store + cache to keep state isolated.
	vectorDir := t.TempDir()
	cacheDir := t.TempDir()

	// 3. Build a config override that:
	//    - rewrites chromem.path / ingest.cache_dir to temp dirs
	//    - replaces the KB_DOC_PATH placeholder in the second mock response
	//      with the absolute path we just ingested, so the citation parser
	//      sees the same source string knowledge_search emits.
	configPath := buildCrossProviderRAGConfig(t,
		"configs/test-cross-provider-rag.yaml",
		vectorDir, cacheDir, kbPath)

	h := testharness.New(t, configPath, testharness.WithTimeout(20*time.Second))

	// 4. Wire up retrieval+citation observers BEFORE Run so we don't miss
	// emissions that the test IO collector also sees but typed-cast verifies
	// here. Also use this subscription to inject rag.ingest synchronously
	// once plugins are wired up.
	bus := h.Bus()
	var (
		retrieved  []events.RetrievalContext
		cited      []events.CitedResponse
		ingestErr  string
		ingestPath string
	)
	bus.Subscribe("rag.retrieved", func(ev engine.Event[any]) {
		if rc, ok := ev.Payload.(events.RetrievalContext); ok {
			retrieved = append(retrieved, rc)
		}
	}, engine.WithPriority(99))
	bus.Subscribe("llm.response.cited", func(ev engine.Event[any]) {
		if cr, ok := ev.Payload.(events.CitedResponse); ok {
			cited = append(cited, cr)
		}
	}, engine.WithPriority(99))

	// io.session.start dispatches synchronously inside the test IO plugin's
	// Ready before its feedInputs goroutine starts driving inputs, so this
	// is a safe pre-input hook for ingest.
	bus.Subscribe("io.session.start", func(_ engine.Event[any]) {
		req := &events.RAGIngest{
			SchemaVersion: events.RAGIngestVersion,
			Path:          kbPath,
			Namespace:     "kb",
		}
		if err := bus.Emit("rag.ingest", req); err != nil {
			ingestErr = err.Error()
			return
		}
		if req.Error != "" {
			ingestErr = req.Error
			return
		}
		ingestPath = req.Path
	}, engine.WithPriority(99))

	// 5. Run.
	h.Run()

	// 6. Assertions.
	if ingestErr != "" {
		t.Fatalf("rag.ingest failed: %s", ingestErr)
	}
	if ingestPath == "" {
		t.Fatalf("rag.ingest never ran (io.session.start may not have fired)")
	}

	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.openai",
		"nexus.agent.react",
		"nexus.embeddings.mock",
		"nexus.vectorstore.chromem",
		"nexus.rag.ingest",
		"nexus.tool.knowledge_search",
		"nexus.rag.citations",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")

	// Core spec: the two pivot events must fire.
	h.AssertEventEmitted("rag.retrieved")
	h.AssertEventEmitted("llm.response.cited")

	// The knowledge_search tool must have been invoked by the mock LLM.
	h.AssertToolCalled("knowledge_search")

	// rag.retrieved must carry the chunk(s) sourced from our ingested file.
	if len(retrieved) == 0 {
		t.Fatalf("expected at least one rag.retrieved event")
	}
	var foundSource bool
	for _, rc := range retrieved {
		for _, c := range rc.Chunks {
			if c.Source == kbPath {
				foundSource = true
			}
		}
	}
	if !foundSource {
		t.Errorf("rag.retrieved did not surface ingested source %q; got %+v", kbPath, retrieved)
	}

	// llm.response.cited must reflect the <cite/> tag the mock LLM produced.
	if len(cited) == 0 {
		t.Fatalf("expected at least one llm.response.cited event")
	}
	var citedAtPath bool
	for _, cr := range cited {
		for _, ref := range cr.Citations {
			if ref.Source == kbPath {
				citedAtPath = true
			}
		}
		// Inline cite tags must have been stripped from the user-facing text.
		if strings.Contains(cr.Text, "<cite") {
			t.Errorf("citations plugin left raw <cite> tag in user-visible text: %q", cr.Text)
		}
	}
	if !citedAtPath {
		t.Errorf("llm.response.cited never carried a citation pointing at %q; got %+v", kbPath, cited)
	}
}

// buildCrossProviderRAGConfig reads the base YAML, rewrites chromem.path,
// rag.ingest.cache_dir, and the KB_DOC_PATH placeholder embedded in the
// second mock_response, then writes the result to a temp file and returns
// its path.
//
// Done by string substitution rather than YAML re-marshal so the inline
// arguments JSON in mock_responses (which copyConfig would otherwise touch
// at the YAML layer only) survives unchanged.
func buildCrossProviderRAGConfig(t *testing.T, basePath, vectorDir, cacheDir, kbPath string) string {
	t.Helper()
	root := findRoot(t)
	abs := filepath.Join(root, basePath)
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", abs, err)
	}
	body := string(raw)

	// Rewrite the chromem path. Match the exact literal in the YAML to keep
	// this substitution targeted.
	const chromemMarker = "path: ~/.nexus/test-sessions/cross-provider-rag-vectors"
	if !strings.Contains(body, chromemMarker) {
		t.Fatalf("could not find chromem path marker %q in base config", chromemMarker)
	}
	body = strings.Replace(body, chromemMarker, "path: "+vectorDir, 1)

	// Insert cache_dir under nexus.rag.ingest's chunker block.
	const ingestMarker = "      overlap: 80\n    # cache_dir overridden by the test as well."
	if !strings.Contains(body, ingestMarker) {
		t.Fatalf("could not find ingest cache_dir marker in base config")
	}
	body = strings.Replace(body, ingestMarker,
		"      overlap: 80\n    cache_dir: "+cacheDir, 1)

	// Replace the citation source placeholder.
	if !strings.Contains(body, "KB_DOC_PATH") {
		t.Fatalf("could not find KB_DOC_PATH placeholder in base config")
	}
	body = strings.ReplaceAll(body, "KB_DOC_PATH", kbPath)

	out := filepath.Join(t.TempDir(), "test-cross-provider-rag.yaml")
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		t.Fatalf("write rewritten config: %v", err)
	}
	return out
}
