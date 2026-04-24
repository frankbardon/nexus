package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// embeddingCache maps content-hash to the vector returned for that content.
// Avoids re-embedding unchanged chunks on reingest. Persistence layout:
//
//	<dir>/<hash2>/<hash>.json
//
// Two-byte directory shard keeps listdir fast even with many entries.
//
// The cache is deliberately not keyed by embedding model: the provider
// round-trips an echo of the actual model in EmbeddingsRequest.Model, but
// the plugin doesn't have that value on Get (before any provider call has
// happened). Users who switch embedding models should drop the cache
// directory — mixing vectors from two models in one namespace is wrong
// anyway, so callers would need to re-ingest either way.
type embeddingCache struct {
	dir string

	mu  sync.Mutex
	hit int
	mis int
}

func newEmbeddingCache(dir string) (*embeddingCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	return &embeddingCache{dir: dir}, nil
}

// key hashes the content. Same content ⇒ same hash ⇒ same cached vector.
func (c *embeddingCache) key(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (c *embeddingCache) path(hash string) string {
	return filepath.Join(c.dir, hash[:2], hash+".json")
}

// Get returns the cached vector for content, or nil if missing.
func (c *embeddingCache) Get(content string) []float32 {
	hash := c.key(content)
	p := c.path(hash)
	data, err := os.ReadFile(p)
	if err != nil {
		c.mu.Lock()
		c.mis++
		c.mu.Unlock()
		return nil
	}
	var v []float32
	if err := json.Unmarshal(data, &v); err != nil {
		return nil
	}
	c.mu.Lock()
	c.hit++
	c.mu.Unlock()
	return v
}

// Put stores a vector for content. Best-effort — failures are logged by
// the caller if at all, not fatal for ingest.
func (c *embeddingCache) Put(content string, vec []float32) error {
	hash := c.key(content)
	p := c.path(hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Stats returns (hits, misses) since process start. Used only for logging.
func (c *embeddingCache) Stats() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hit, c.mis
}
