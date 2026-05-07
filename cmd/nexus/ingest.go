package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/events"
)

// runIngest is the "nexus ingest" subcommand: boot a minimal engine with
// only the primitives + ingest plugin active, walk the input path, emit
// rag.ingest events, print a summary, exit. Useful for offline bulk loads
// without a running agent.
func runIngest(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	namespace := fs.String("namespace", "", "target namespace in the vector store (required)")
	glob := fs.String("glob", "", "optional filename glob (matched against path and basename)")
	concurrency := fs.Int("concurrency", 4, "max files ingested in parallel")
	chunkSize := fs.Int("chunk-size", 1000, "chunker target size")
	chunkOverlap := fs.Int("chunk-overlap", 200, "chunker overlap")
	vectorPath := fs.String("vector-path", "", "override vector store path (default: ~/.nexus/vectors)")
	cachePath := fs.String("cache-path", "", "override embedding cache path (default: ~/.nexus/vectors/_cache)")
	embeddingsModel := fs.String("model", "", "embedding model (default: text-embedding-3-small)")
	lexical := fs.Bool("lexical", false, "also write each chunk into the SQLite FTS5 lexical store (re-runnable; embedding cache makes the vector pass a no-op so this also serves as 'reindex into the new lexical store')")
	lexicalScope := fs.String("lexical-scope", "app", "scope for the lexical store when --lexical is set: app, agent, session")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus ingest --namespace=NAME [flags] PATH [PATH...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nerror: --namespace is required")
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nerror: at least one PATH is required")
		return 2
	}

	cfg := buildIngestConfig(engine.ExpandPath(*vectorPath), engine.ExpandPath(*cachePath), *embeddingsModel, *chunkSize, *chunkOverlap, *lexical, *lexicalScope)
	eng, err := engine.NewFromBytes(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create engine: %v\n", err)
		return 1
	}
	allplugins.RegisterAll(eng.Registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Boot(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "engine boot failed: %v\n", err)
		return 1
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = eng.Stop(stopCtx)
	}()

	// Enumerate all files matching the glob.
	var files []string
	for _, p := range paths {
		expanded := engine.ExpandPath(p)
		got, err := walkIngestPath(expanded, *glob)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk %q: %v\n", expanded, err)
			return 1
		}
		files = append(files, got...)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no files matched")
		return 0
	}

	// Bounded-concurrency fan-out of rag.ingest events.
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	var okCount, failCount int32
	var totalChunks, totalCached int64
	for _, f := range files {
		sem <- struct{}{}
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			defer func() { <-sem }()
			req := &events.RAGIngest{SchemaVersion: events.RAGIngestVersion, Path: path, Namespace: *namespace}
			_ = eng.Bus.Emit("rag.ingest", req)
			if req.Error != "" {
				atomic.AddInt32(&failCount, 1)
				fmt.Fprintf(os.Stderr, "FAIL %s: %s\n", path, req.Error)
				return
			}
			atomic.AddInt32(&okCount, 1)
			atomic.AddInt64(&totalChunks, int64(req.Chunks))
			atomic.AddInt64(&totalCached, int64(req.SkippedCached))
			fmt.Printf("OK   %s  (%d chunks, %d cached)\n", path, req.Chunks, req.SkippedCached)
		}(f)
	}
	wg.Wait()

	fmt.Printf("\ningested %d file(s), %d failed; %d chunks total (%d from cache)\n",
		okCount, failCount, totalChunks, totalCached)
	if failCount > 0 {
		return 1
	}
	return 0
}

// walkIngestPath mirrors the plugin's walkPath logic but lives here so the
// CLI doesn't depend on package-internal helpers. If path is a file, return
// it verbatim (glob optional). If it's a directory, walk it.
func walkIngestPath(root, glob string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{root}, nil
	}
	var files []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if glob == "" {
			files = append(files, p)
			return nil
		}
		if rel, relErr := filepath.Rel(root, p); relErr == nil {
			if ok, _ := filepath.Match(glob, rel); ok {
				files = append(files, p)
				return nil
			}
		}
		if ok, _ := filepath.Match(glob, filepath.Base(p)); ok {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// buildIngestConfig assembles a minimal YAML config for the ingest CLI:
// embeddings.provider (OpenAI), vector.store (chromem), and the ingest
// plugin. No agent, no LLM, no IO. Per-plugin configs live flat under
// "plugins:" with the plugin ID as the key (see pkg/engine/config.go).
//
// When lexical is true the sqlite_fts plugin is added to the active set,
// which causes rag/ingest to dual-write each chunk into the lexical store.
// Re-running ingest with --lexical against an already-vector-indexed corpus
// is the "reindex" path: the embedding cache short-circuits vector
// re-embedding while the lexical store is freshly populated.
func buildIngestConfig(vectorPath, cachePath, model string, chunkSize, chunkOverlap int, lexical bool, lexicalScope string) []byte {
	var b []byte
	b = append(b, "core:\n  log_level: info\n"...)
	b = append(b, "plugins:\n"...)
	b = append(b, "  active:\n"...)
	b = append(b, "    - nexus.embeddings.openai\n"...)
	b = append(b, "    - nexus.vectorstore.chromem\n"...)
	if lexical {
		b = append(b, "    - nexus.vectorstore.sqlite_fts\n"...)
	}
	b = append(b, "    - nexus.rag.ingest\n"...)

	if model != "" {
		b = append(b, "  nexus.embeddings.openai:\n"...)
		b = append(b, fmt.Sprintf("    model: %q\n", model)...)
	}
	if vectorPath != "" {
		b = append(b, "  nexus.vectorstore.chromem:\n"...)
		b = append(b, fmt.Sprintf("    path: %q\n", vectorPath)...)
	}
	if lexical {
		b = append(b, "  nexus.vectorstore.sqlite_fts:\n"...)
		b = append(b, fmt.Sprintf("    scope: %q\n", lexicalScope)...)
	}
	b = append(b, "  nexus.rag.ingest:\n"...)
	b = append(b, "    chunker:\n"...)
	b = append(b, fmt.Sprintf("      size: %d\n", chunkSize)...)
	b = append(b, fmt.Sprintf("      overlap: %d\n", chunkOverlap)...)
	if cachePath != "" {
		b = append(b, fmt.Sprintf("    cache_dir: %q\n", cachePath)...)
	}
	return b
}
