package hitlsynthesizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// cacheFile is the per-plugin write-through JSONL cache. Sits in the
// session workspace under plugins/<pluginID>/cache.jsonl, matching the
// path convention used by memory/compaction and other persistent
// plugin state.
const cacheFile = "plugins/" + pluginID + "/cache.jsonl"

// cacheEntry is one record in the on-disk cache.jsonl.
type cacheEntry struct {
	Key        string `json:"key"`
	ActionKind string `json:"action_kind"`
	Prompt     string `json:"prompt"`
}

// buildCacheKey hashes ActionRef into a stable key partitioned by
// ActionKind. JSON serialization is deterministic (Go's encoding/json
// emits map keys in sorted order) so the same logical action shape
// always produces the same key.
func buildCacheKey(actionKind string, ref map[string]any) string {
	var refJSON []byte
	if len(ref) > 0 {
		// json.Marshal sorts map keys, giving us a stable hash input.
		refJSON, _ = json.Marshal(ref)
	}
	h := sha256.New()
	h.Write([]byte(actionKind))
	h.Write([]byte{0})
	h.Write(refJSON)
	return fmt.Sprintf("%s/%s", sanitizeKindForKey(actionKind), hex.EncodeToString(h.Sum(nil))[:16])
}

func sanitizeKindForKey(kind string) string {
	if kind == "" {
		return "_"
	}
	// Avoid path separators leaking into the key prefix; keep keys
	// readable in cache.jsonl dumps.
	kind = strings.ReplaceAll(kind, "/", "_")
	kind = strings.ReplaceAll(kind, "\\", "_")
	return kind
}

// cacheGet returns a cached prompt if present.
func (p *Plugin) cacheGet(key string) (string, bool) {
	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	v, ok := p.cache[key]
	return v, ok
}

// cachePut stores a prompt and writes through to cache.jsonl. Failures
// to persist are logged but non-fatal — the in-memory map remains
// authoritative for the running session.
func (p *Plugin) cachePut(key, prompt, actionKind string) {
	p.cacheMu.Lock()
	if existing, ok := p.cache[key]; ok && existing == prompt {
		p.cacheMu.Unlock()
		return
	}
	p.cache[key] = prompt
	p.cacheMu.Unlock()

	if p.session == nil {
		return
	}
	entry := cacheEntry{Key: key, ActionKind: actionKind, Prompt: prompt}
	data, err := json.Marshal(entry)
	if err != nil {
		p.logger.Warn("hitl_synthesizer: cache encode failed", "error", err)
		return
	}
	data = append(data, '\n')
	if err := p.session.AppendFile(cacheFile, data); err != nil {
		p.logger.Warn("hitl_synthesizer: cache append failed", "error", err)
	}
}

// loadCache hydrates the in-memory map from cache.jsonl. Missing file
// is not an error; malformed lines are skipped with a warning so a
// partial corruption doesn't poison the rest of the cache.
func (p *Plugin) loadCache() error {
	if !p.cacheEnabled || p.session == nil {
		return nil
	}
	if !p.session.FileExists(cacheFile) {
		return nil
	}
	data, err := p.session.ReadFile(cacheFile)
	if err != nil {
		return fmt.Errorf("read cache: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry cacheEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			p.logger.Warn("hitl_synthesizer: skipping malformed cache line", "error", err)
			continue
		}
		if entry.Key == "" || entry.Prompt == "" {
			continue
		}
		p.cache[entry.Key] = entry.Prompt
	}
	return nil
}
