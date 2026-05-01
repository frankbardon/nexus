package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/events"
)

// ToolCache persists tool.result payloads keyed by sha256(tool_id ||
// canonical_args) under <session>/journal/cache/<tool_id>/<sha>.json. It
// is the durable, args-addressable companion to ReplayState's FIFO queue
// — replay tries the cache first (deterministic order-independent
// lookup) and falls back to the FIFO stash on miss.
//
// The cache is bus-driven: a single subscription pair (tool.invoke +
// tool.result) records every tool that completes. Tools do not need to
// know about it, but the cache itself is exposed via ReplayState so the
// short-circuit helper can read on replay.
type ToolCache struct {
	rootDir string
	logger  *slog.Logger

	// pendingArgs maps a tool.invoke's call ID to the canonical args hash
	// computed at invoke time. tool.result's ID closes the loop and the
	// cache writes <tool>/<hash>.json. Cleared on close-out.
	mu          sync.Mutex
	pendingArgs map[string]pendingEntry
}

type pendingEntry struct {
	toolID   string
	cacheKey string
}

// NewToolCache constructs a cache rooted at <session>/journal/cache. The
// directory is created lazily on first write — empty sessions do not
// produce a cache subtree.
func NewToolCache(rootDir string, logger *slog.Logger) *ToolCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &ToolCache{
		rootDir:     rootDir,
		logger:      logger,
		pendingArgs: make(map[string]pendingEntry),
	}
}

// Install registers the cache as a bus subscriber so live tool.invoke /
// tool.result pairs auto-populate the cache. Returns the unsubscribe
// functions for the engine's run-scoped teardown.
func (c *ToolCache) Install(bus EventBus) []func() {
	if c == nil || bus == nil {
		return nil
	}
	return []func(){
		bus.Subscribe("tool.invoke", c.handleInvoke,
			WithPriority(95), WithSource("nexus.toolcache")),
		bus.Subscribe("tool.result", c.handleResult,
			WithPriority(95), WithSource("nexus.toolcache")),
	}
}

func (c *ToolCache) handleInvoke(ev Event[any]) {
	tc, ok := ev.Payload.(events.ToolCall)
	if !ok || tc.ID == "" || tc.Name == "" {
		return
	}
	key, err := canonicalArgsHash(tc.Name, tc.Arguments)
	if err != nil {
		c.logger.Debug("toolcache: skipping invoke; canonicalize failed",
			"tool", tc.Name, "error", err)
		return
	}
	c.mu.Lock()
	c.pendingArgs[tc.ID] = pendingEntry{toolID: tc.Name, cacheKey: key}
	c.mu.Unlock()
}

func (c *ToolCache) handleResult(ev Event[any]) {
	res, ok := ev.Payload.(events.ToolResult)
	if !ok || res.ID == "" {
		return
	}
	c.mu.Lock()
	pending, exists := c.pendingArgs[res.ID]
	delete(c.pendingArgs, res.ID)
	c.mu.Unlock()
	if !exists {
		// Replay-emitted results have no preceding invoke record (the
		// short-circuit emits directly); skipping them is the right call
		// — we are not producing new ground truth, just replaying old.
		return
	}
	if err := c.write(pending.toolID, pending.cacheKey, res); err != nil {
		c.logger.Warn("toolcache: failed to persist result",
			"tool", pending.toolID, "error", err)
	}
}

// Lookup attempts to return a cached result for the given tool name +
// canonical args. Returns ok=false on cache miss, malformed JSON, or a
// canonicalize failure (the caller falls back to the FIFO stash).
func (c *ToolCache) Lookup(toolID string, args map[string]any) (events.ToolResult, bool) {
	if c == nil {
		return events.ToolResult{}, false
	}
	key, err := canonicalArgsHash(toolID, args)
	if err != nil {
		return events.ToolResult{}, false
	}
	path := c.pathFor(toolID, key)
	data, err := os.ReadFile(path)
	if err != nil {
		return events.ToolResult{}, false
	}
	var res events.ToolResult
	if err := json.Unmarshal(data, &res); err != nil {
		c.logger.Warn("toolcache: malformed cache file ignored",
			"path", path, "error", err)
		return events.ToolResult{}, false
	}
	return res, true
}

// write serializes the result to disk. Idempotent — same args + tool +
// result writes the same bytes; same args + different result overwrites.
// Overwrites are the price of args-keyed cache when the same args
// produce different outputs across runs (e.g. shell "date").
func (c *ToolCache) write(toolID, key string, res events.ToolResult) error {
	path := c.pathFor(toolID, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir cache subtree: %w", err)
	}
	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal tool result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write cache file: %w", err)
	}
	return nil
}

func (c *ToolCache) pathFor(toolID, key string) string {
	// Replace path-unsafe characters in the tool ID. Tool IDs are dotted
	// ("file.read"), so no traversal risk; sanitize defensively anyway.
	safe := strings.ReplaceAll(toolID, "..", "_")
	safe = strings.ReplaceAll(safe, "/", "_")
	return filepath.Join(c.rootDir, safe, key+".json")
}

// canonicalArgsHash produces a stable sha256 hex of (tool_id, args) for
// use as a cache key. Stability rules:
//   - JSON-serialize the args map with sorted keys and no whitespace.
//   - Prefix with the tool ID so two tools with identical args do not
//     collide.
//   - Empty / nil args produce a stable hash; the tool ID still varies.
func canonicalArgsHash(toolID string, args map[string]any) (string, error) {
	canon, err := canonicalizeJSON(args)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(toolID))
	h.Write([]byte("|"))
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalizeJSON marshals v with sorted-key recursion. Stability matters
// because Go's encoding/json sorts map keys, but only at the top level;
// nested maps would otherwise hash differently across runs depending on
// iteration order of the original map. The custom walk forces a total
// order on every nested map.
func canonicalizeJSON(v any) ([]byte, error) {
	canon := canonicalize(v)
	return json.Marshal(canon)
}

func canonicalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys)*2)
		for _, k := range keys {
			out = append(out, k, canonicalize(x[k]))
		}
		// Wrap in a slice so the canonical form is unambiguous: a map and
		// a 2N-element slice with the same key/value content marshal
		// differently, so they cannot collide.
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = canonicalize(item)
		}
		return out
	default:
		return x
	}
}
