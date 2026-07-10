package aguiremote

import "sync"

// resultCache is a goroutine-safe, fixed-capacity LRU cache of remote-agent
// outcomes keyed by a content-addressable hash of (endpoint, task, context). It
// mirrors the delegate runtime's in-process cache so identical delegated tasks
// replay without re-hitting the remote endpoint. Only successful outcomes are
// cached (the caller skips errors), so a transient remote failure is retried.
type resultCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*cacheEntry
	head     *cacheEntry
	tail     *cacheEntry
}

type cacheEntry struct {
	key  string
	val  remoteOutcome
	prev *cacheEntry
	next *cacheEntry
}

// newResultCache returns a cache with the given capacity. Capacity <= 0 disables
// eviction (unbounded growth).
func newResultCache(capacity int) *resultCache {
	return &resultCache{
		capacity: capacity,
		items:    make(map[string]*cacheEntry),
	}
}

func (c *resultCache) get(key string) (remoteOutcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return remoteOutcome{}, false
	}
	c.promote(e)
	return e.val, true
}

func (c *resultCache) put(key string, out remoteOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.val = out
		c.promote(e)
		return
	}
	e := &cacheEntry{key: key, val: out}
	c.items[key] = e
	c.insertAtFront(e)
	if c.capacity > 0 && len(c.items) > c.capacity {
		c.evict()
	}
}

func (c *resultCache) insertAtFront(e *cacheEntry) {
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

func (c *resultCache) promote(e *cacheEntry) {
	if c.head == e {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if c.tail == e {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
}

func (c *resultCache) evict() {
	if c.tail == nil {
		return
	}
	old := c.tail
	c.tail = old.prev
	if c.tail != nil {
		c.tail.next = nil
	} else {
		c.head = nil
	}
	delete(c.items, old.key)
}
