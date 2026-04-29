package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// cacheState holds prompt-cache configuration and the in-memory key map.
//
//	cache:
//	  enabled: true
//	  min_tokens: 32768
//	  ttl: "1h"
//	  max_entries: 64
//
// Caching is conservative: only the stable prefix (system instruction + tool
// declarations + initial user/model turns up to the first tool exchange) is
// hashed and cached. The trailing delta is sent on every request.
type cacheState struct {
	enabled    bool
	minTokens  int
	ttl        time.Duration
	maxEntries int
	logger     *slog.Logger

	mu      sync.Mutex
	entries map[string]*cacheEntry // prefix-hash -> entry
	order   []string               // LRU eviction order, oldest first
}

type cacheEntry struct {
	name    string // resource name returned by cachedContents.create
	expires time.Time
}

func newCacheState(cfg map[string]any, logger *slog.Logger) *cacheState {
	cs := &cacheState{
		minTokens:  32768,
		ttl:        time.Hour,
		maxEntries: 64,
		logger:     logger,
		entries:    make(map[string]*cacheEntry),
	}

	raw, ok := cfg["cache"].(map[string]any)
	if !ok {
		return cs
	}

	if v, ok := raw["enabled"].(bool); ok {
		cs.enabled = v
	}
	if v, ok := raw["min_tokens"].(int); ok && v > 0 {
		cs.minTokens = v
	}
	if v, ok := raw["max_entries"].(int); ok && v > 0 {
		cs.maxEntries = v
	}
	if v, ok := raw["ttl"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cs.ttl = d
		}
	}

	return cs
}

// lookup returns a cached_content resource name when the request prefix
// matches a live cache entry. Returns "" when no cache should be applied.
//
// This is a pure-read lookup; population happens out-of-band via populate().
// The current implementation is a stub: it computes the prefix hash and
// returns a hit when present, but never auto-populates (auto-population
// requires a token-count probe and a synchronous create call). Callers can
// pre-populate via a future explicit API; for now this preserves the engine
// hook so cached prompts work when the user manually wires them.
func (cs *cacheState) lookup(model, system string, tools []events.ToolDef, contents []map[string]any) string {
	if cs == nil || !cs.enabled {
		return ""
	}

	hash := cs.prefixHash(model, system, tools, contents)

	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry, ok := cs.entries[hash]
	if !ok {
		return ""
	}
	if time.Now().After(entry.expires) {
		delete(cs.entries, hash)
		cs.removeOrder(hash)
		return ""
	}
	return entry.name
}

// populate stores a cachedContents resource name against a prefix hash. Called
// by integrations that explicitly create a cache entry (e.g. a future
// admin tool or the cache plugin itself).
func (cs *cacheState) populate(model, system string, tools []events.ToolDef, contents []map[string]any, name string, ttl time.Duration) {
	if cs == nil || !cs.enabled {
		return
	}
	hash := cs.prefixHash(model, system, tools, contents)
	if ttl == 0 {
		ttl = cs.ttl
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.entries[hash] = &cacheEntry{name: name, expires: time.Now().Add(ttl)}
	cs.order = append(cs.order, hash)
	for len(cs.order) > cs.maxEntries {
		oldest := cs.order[0]
		cs.order = cs.order[1:]
		delete(cs.entries, oldest)
	}
}

// invalidate removes a cache entry by name (used when a 404 indicates Google-
// side expiry).
func (cs *cacheState) invalidate(name string) {
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for hash, entry := range cs.entries {
		if entry.name == name {
			delete(cs.entries, hash)
			cs.removeOrder(hash)
			return
		}
	}
}

func (cs *cacheState) removeOrder(hash string) {
	for i, h := range cs.order {
		if h == hash {
			cs.order = append(cs.order[:i], cs.order[i+1:]...)
			return
		}
	}
}

// prefixHash hashes the request prefix that would be cached. Order: model,
// system, tool declarations (sorted by name), and the leading run of contents
// up to (but not including) any functionResponse turn (those represent the
// dynamic tail).
func (cs *cacheState) prefixHash(model, system string, tools []events.ToolDef, contents []map[string]any) string {
	type toolKey struct {
		Name        string
		Description string
		Params      map[string]any
	}

	keys := make([]toolKey, 0, len(tools))
	for _, t := range tools {
		keys = append(keys, toolKey{Name: t.Name, Description: t.Description, Params: t.Parameters})
	}
	// Stable order: same input set produces the same hash even if tool slice
	// order varies between calls.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1].Name > keys[j].Name; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}

	prefixContents := make([]map[string]any, 0, len(contents))
	for _, c := range contents {
		if hasFunctionResponsePart(c) {
			break
		}
		prefixContents = append(prefixContents, c)
	}

	payload := struct {
		Model    string
		System   string
		Tools    []toolKey
		Contents []map[string]any
	}{
		Model:    model,
		System:   system,
		Tools:    keys,
		Contents: prefixContents,
	}

	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hasFunctionResponsePart(c map[string]any) bool {
	parts, ok := c["parts"].([]map[string]any)
	if !ok {
		// Json round-trip might normalize to []any.
		if alt, ok := c["parts"].([]any); ok {
			for _, p := range alt {
				if pm, ok := p.(map[string]any); ok {
					if _, ok := pm["functionResponse"]; ok {
						return true
					}
				}
			}
			return false
		}
		return false
	}
	for _, p := range parts {
		if _, ok := p["functionResponse"]; ok {
			return true
		}
	}
	return false
}

// createCachedContent calls the cachedContents API to create a new cache
// entry. Exposed for callers that want to pre-populate the cache; not used
// by the auto-cache path (which stays read-only — see lookup()).
func (p *Plugin) createCachedContent(ctx context.Context, model, system string, tools []events.ToolDef, contents []map[string]any, ttl time.Duration) (string, error) {
	if !p.cache.enabled {
		return "", fmt.Errorf("cache disabled")
	}
	return p.createCachedContentAt(ctx, p.auth.cachedContentsURL(), model, system, tools, contents, ttl)
}

// createCachedContentAt is the test-friendly variant: it posts to an explicit
// URL rather than the one resolved from authState.
func (p *Plugin) createCachedContentAt(ctx context.Context, url, model, system string, tools []events.ToolDef, contents []map[string]any, ttl time.Duration) (string, error) {
	body := map[string]any{
		"model": fmt.Sprintf("models/%s", model),
	}
	if system != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(tools) > 0 {
		decls := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  sanitizeSchemaForGemini(t.Parameters),
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	if len(contents) > 0 {
		body["contents"] = contents
	}
	if ttl > 0 {
		body["ttl"] = fmt.Sprintf("%ds", int(ttl.Seconds()))
	}

	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := p.auth.applyAuth(ctx, req, p.client); err != nil {
		return "", err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cachedContents.create failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse cachedContents response: %w", err)
	}
	if out.Name == "" {
		return "", fmt.Errorf("empty cache name in response: %s", string(respBody))
	}

	p.cache.populate(model, system, tools, contents, out.Name, ttl)
	return out.Name, nil
}
