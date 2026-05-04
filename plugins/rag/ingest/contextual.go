package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// contextualizer wraps Anthropic's "contextual retrieval" pattern: ask a
// cheap LLM to produce a 1-paragraph situating prefix for each chunk before
// it is embedded. Adds one LLM round-trip per chunk during ingest; cached
// by sha256(doc-window || chunk) so re-ingests skip the call.
//
// Wired into rag/ingest behind the contextual_retrieval.enabled flag.
type contextualizer struct {
	bus            engine.EventBus
	logger         *slog.Logger
	enabled        bool
	model          string
	modelRole      string
	maxDocChars    int
	maxPrefixChars int
	timeout        time.Duration
	cache          *prefixCache

	mu      sync.Mutex
	pending map[string]chan string
	seq     atomic.Uint64

	unsub func()
}

func newContextualizer(bus engine.EventBus, logger *slog.Logger, cfg map[string]any, cacheDir string) (*contextualizer, error) {
	c := &contextualizer{
		bus:            bus,
		logger:         logger,
		maxDocChars:    2000,
		maxPrefixChars: 400,
		timeout:        30 * time.Second,
		pending:        make(map[string]chan string),
	}
	if cfg == nil {
		return c, nil
	}
	if v, ok := cfg["enabled"].(bool); ok {
		c.enabled = v
	}
	if v, ok := cfg["model"].(string); ok && v != "" {
		c.modelRole = v
	}
	if v, ok := cfg["model_id"].(string); ok && v != "" {
		c.model = v
	}
	if v, ok := cfg["max_chars_doc_window"].(int); ok && v > 0 {
		c.maxDocChars = v
	}
	if v, ok := cfg["max_chars_prefix"].(int); ok && v > 0 {
		c.maxPrefixChars = v
	}
	if v, ok := cfg["timeout_ms"].(int); ok && v > 0 {
		c.timeout = time.Duration(v) * time.Millisecond
	}
	if c.enabled {
		cache, err := newPrefixCache(filepath.Join(cacheDir, "_prefix"))
		if err != nil {
			return nil, fmt.Errorf("contextual prefix cache: %w", err)
		}
		c.cache = cache
		c.unsub = bus.Subscribe("llm.response", c.handleLLMResponse, engine.WithPriority(99))
	}
	return c, nil
}

func (c *contextualizer) close() {
	if c.unsub != nil {
		c.unsub()
	}
}

// Prefix returns a context paragraph for the given chunk. Returns empty
// string when contextualization is disabled or the LLM call fails — the
// caller falls back to the raw chunk in that case.
func (c *contextualizer) Prefix(docContext, chunk string) string {
	if !c.enabled {
		return ""
	}
	if c.cache != nil {
		if v := c.cache.Get(docContext, chunk); v != "" {
			return v
		}
	}
	prefix, err := c.generate(docContext, chunk)
	if err != nil {
		c.logger.Warn("contextual retrieval: skipping prefix", "err", err)
		return ""
	}
	if c.cache != nil && prefix != "" {
		_ = c.cache.Put(docContext, chunk, prefix)
	}
	return prefix
}

// generate issues one LLM round-trip and waits for the response via the
// metadata-correlated chan. The bus is synchronous — when bus.Emit returns
// the response handler has already run — but the chan + select pattern
// keeps the code resilient to future bus changes.
func (c *contextualizer) generate(docContext, chunk string) (string, error) {
	id := fmt.Sprintf("ctxreq-%d", c.seq.Add(1))
	respCh := make(chan string, 1)
	c.mu.Lock()
	c.pending[id] = respCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	prompt := buildContextPrompt(docContext, chunk, c.maxDocChars)
	req := events.LLMRequest{
		Role:  c.modelRole,
		Model: c.model,
		Messages: []events.Message{
			{Role: "system", Content: contextSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Stream: false,
		Metadata: map[string]any{
			"_source":   "rag.ingest.contextual",
			"_ctxreq":   id,
			"task_kind": "rag_contextualize",
		},
		Tags: map[string]string{"source_plugin": "nexus.rag.ingest"},
	}
	if err := c.bus.Emit("llm.request", req); err != nil {
		return "", fmt.Errorf("emit llm.request: %w", err)
	}
	select {
	case prefix := <-respCh:
		prefix = strings.TrimSpace(prefix)
		if c.maxPrefixChars > 0 && len(prefix) > c.maxPrefixChars {
			prefix = prefix[:c.maxPrefixChars]
		}
		return prefix, nil
	case <-time.After(c.timeout):
		return "", fmt.Errorf("timeout after %s waiting for llm.response", c.timeout)
	}
}

// handleLLMResponse routes contextual-prefix responses back to the waiting
// generate call. Non-matching responses fall through silently — other
// llm.response subscribers handle them.
func (c *contextualizer) handleLLMResponse(ev engine.Event[any]) {
	resp, ok := ev.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	if resp.Metadata == nil {
		return
	}
	id, ok := resp.Metadata["_ctxreq"].(string)
	if !ok || id == "" {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- resp.Content:
	default:
	}
}

// contextSystemPrompt is the per-chunk situating instruction. Lifted from
// Anthropic's contextual retrieval blog post and trimmed.
const contextSystemPrompt = `You are summarizing a chunk of a longer document for retrieval purposes.
Given the surrounding document context and the specific chunk, produce a
short standalone paragraph (1-3 sentences) that situates the chunk within
the broader document. Mention what section, topic, or function the chunk
belongs to. Do not repeat the chunk's content; only add the situating
context. Output the paragraph and nothing else.`

func buildContextPrompt(docContext, chunk string, maxDocChars int) string {
	if maxDocChars > 0 && len(docContext) > maxDocChars {
		docContext = docContext[:maxDocChars]
	}
	var b strings.Builder
	b.WriteString("<document_context>\n")
	b.WriteString(docContext)
	b.WriteString("\n</document_context>\n\n<chunk>\n")
	b.WriteString(chunk)
	b.WriteString("\n</chunk>")
	return b.String()
}

// prefixCache stores generated context prefixes on disk, keyed by
// sha256(docContext || chunk). Avoids re-running the LLM on re-ingest of
// the same source files.
type prefixCache struct {
	dir string
}

func newPrefixCache(dir string) (*prefixCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &prefixCache{dir: dir}, nil
}

func (c *prefixCache) key(docContext, chunk string) string {
	h := sha256.New()
	h.Write([]byte(docContext))
	h.Write([]byte{0})
	h.Write([]byte(chunk))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *prefixCache) path(hash string) string {
	return filepath.Join(c.dir, hash[:2], hash+".txt")
}

func (c *prefixCache) Get(docContext, chunk string) string {
	data, err := os.ReadFile(c.path(c.key(docContext, chunk)))
	if err != nil {
		return ""
	}
	return string(data)
}

func (c *prefixCache) Put(docContext, chunk, prefix string) error {
	hash := c.key(docContext, chunk)
	p := c.path(hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(prefix), 0o644)
}
