package web

import "sync"

// urlCache is a session-scoped memoizer for fetch results keyed on
// extract-mode + final URL. Entries are cleared on io.session.end so
// recall-from-history in a new engine boot starts fresh.
type urlCache struct {
	mu    sync.RWMutex
	store map[string]string
}

func newURLCache() *urlCache {
	return &urlCache{store: make(map[string]string)}
}

func (c *urlCache) get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.store[key]
	return v, ok
}

func (c *urlCache) set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = value
}

func (c *urlCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = make(map[string]string)
}
