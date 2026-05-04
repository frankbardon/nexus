package classifier

import (
	"container/list"
	"sync"
)

// cache is a small LRU keyed by prompt-prefix hash. The values are model
// ids (strings) the classifier picked for that prefix. Cap is bounded to
// keep memory predictable on long sessions; classifier-router cost is
// dominated by misses (one extra LLM call apiece) so the cap is the ROI
// knob — bigger cache means more hits, but also bigger working set.
type cache struct {
	mu    sync.Mutex
	cap   int
	items map[string]*list.Element
	order *list.List // front = MRU
}

type cacheEntry struct {
	key   string
	value string
}

func newCache(cap int) *cache {
	if cap <= 0 {
		cap = 1
	}
	return &cache{
		cap:   cap,
		items: make(map[string]*list.Element, cap),
		order: list.New(),
	}
}

func (c *cache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*cacheEntry).value, true
	}
	return "", false
}

func (c *cache) put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value.(*cacheEntry).value = value
		c.order.MoveToFront(e)
		return
	}
	e := c.order.PushFront(&cacheEntry{key: key, value: value})
	c.items[key] = e
	for c.order.Len() > c.cap {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}

func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
