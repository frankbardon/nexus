// Package markdownindex implements a lexical-only markdown indexer. It walks
// a directory of markdown (.md) files, parses YAML frontmatter, splits each file
// on `##` headings, and upserts one record per section into the search.lexical
// capability (BM25 / SQLite FTS5). No embeddings, no vector store — the index
// is pure keyword search over the filesystem.
//
// Frontmatter title and tags are folded into each section's indexed text so
// brand and topic terms remain discoverable from any section. Indexing runs
// once at Ready; re-running re-upserts deterministic IDs in place.
package markdownindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.index.markdown"
	pluginName = "Markdown Lexical Indexer"
	version    = "0.1.0"

	defaultNamespace = "docs"
	defaultGlob      = "*.md"
)

// Plugin walks docsDir at Ready and writes section records to the lexical store.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	docsDir   string
	namespace string
	glob      string

	hasLexical bool
}

func New() engine.Plugin {
	return &Plugin{namespace: defaultNamespace, glob: defaultGlob}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{{Capability: "search.lexical"}}
}

func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["docs_dir"].(string); ok && v != "" {
		p.docsDir = engine.ExpandPath(v)
	}
	if v, ok := ctx.Config["namespace"].(string); ok && v != "" {
		p.namespace = v
	}
	if v, ok := ctx.Config["glob"].(string); ok && v != "" {
		p.glob = v
	}
	if p.docsDir == "" {
		return fmt.Errorf("markdown index: docs_dir is required")
	}
	p.hasLexical = len(ctx.Capabilities["search.lexical"]) > 0
	return nil
}

// Ready walks docsDir and indexes every matching file. It runs after all
// plugins Init, so the search.lexical provider is subscribed and ready.
func (p *Plugin) Ready() error {
	if !p.hasLexical {
		return fmt.Errorf("markdown index: search.lexical capability not active")
	}
	info, err := os.Stat(p.docsDir)
	if err != nil {
		return fmt.Errorf("markdown index: docs_dir %q: %w", p.docsDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("markdown index: docs_dir %q is not a directory", p.docsDir)
	}

	files, sections := 0, 0
	walkErr := filepath.WalkDir(p.docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ok, _ := filepath.Match(p.glob, filepath.Base(path)); !ok {
			return nil
		}
		n, ierr := p.indexFile(path)
		if ierr != nil {
			p.logger.Warn("markdown index: skipping file", "path", path, "err", ierr)
			return nil
		}
		files++
		sections += n
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("markdown index: walk: %w", walkErr)
	}
	p.logger.Info("markdown index complete",
		"dir", p.docsDir, "namespace", p.namespace, "files", files, "sections", sections)
	return nil
}

// indexFile reads one markdown file, splits it into sections, and emits a
// single lexical.upsert. Returns the number of sections indexed.
func (p *Plugin) indexFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}
	fm, body := splitFrontmatter(string(data))
	secs := splitSections(body)
	if len(secs) == 0 {
		return 0, nil
	}

	rel, err := filepath.Rel(p.docsDir, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	title := fm.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	tags := strings.Join(fm.Tags, " ")
	prefix := strings.TrimSpace(title + "\n" + tags)

	pathHash := pathKey(path)
	docs := make([]events.LexicalDoc, 0, len(secs))
	for i, s := range secs {
		// Fold title + tags into the stored/indexed content so a section is
		// findable by brand/topic even when those terms only appear in the
		// frontmatter. Kept short; surfaces as a header in tool output.
		content := prefix + "\n\n" + s.Text
		docs = append(docs, events.LexicalDoc{
			ID:      fmt.Sprintf("%s-%d", pathHash, i),
			Content: content,
			Metadata: map[string]string{
				"source":      rel,
				"path":        path,
				"title":       title,
				"heading":     s.Heading,
				"section_idx": fmt.Sprintf("%d", i),
				"tags":        tags,
				"type":        fm.Type,
			},
		})
	}

	up := &events.LexicalUpsert{SchemaVersion: events.LexicalUpsertVersion, Namespace: p.namespace, Docs: docs}
	if err := p.bus.Emit("lexical.upsert", up); err != nil {
		return 0, fmt.Errorf("emit upsert: %w", err)
	}
	if up.Error != "" {
		return 0, fmt.Errorf("upsert: %s", up.Error)
	}
	return len(docs), nil
}

// pathKey returns a stable 16-hex-char id derived from the absolute path, so
// re-indexing the same file replaces its records instead of duplicating them.
func pathKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:16]
}

func (p *Plugin) Shutdown(_ context.Context) error         { return nil }
func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }
func (p *Plugin) Emissions() []string                       { return []string{"lexical.upsert"} }
