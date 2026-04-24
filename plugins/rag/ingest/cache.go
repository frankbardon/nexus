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

// embeddingCache maps content-hash (keyed per embedding model) to the vector
// returned for that content. Avoids re-embedding unchanged chunks on reingest.
// Persistence layout: one JSON file per entry at
//
//	<dir>/<model>/<hash2>/<hash>.json
//
// Two-byte directory shard keeps listdir fast even with many entries.
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

// key hashes the content. Same content + same model ⇒ same hash ⇒ same
// cached vector. Different model ⇒ different subdir.
func (c *embeddingCache) key(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (c *embeddingCache) path(model, hash string) string {
	if model == "" {
		model = "default"
	}
	return filepath.Join(c.dir, model, hash[:2], hash+".json")
}

// Get returns the cached vector for (model, content), or nil if missing.
func (c *embeddingCache) Get(model, content string) []float32 {
	hash := c.key(content)
	p := c.path(model, hash)
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

// Put stores a vector for (model, content). Best-effort — failures are
// logged by the caller if at all, not fatal for ingest.
func (c *embeddingCache) Put(model, content string, vec []float32) error {
	hash := c.key(content)
	p := c.path(model, hash)
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
