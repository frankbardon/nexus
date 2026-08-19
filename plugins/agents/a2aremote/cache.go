package a2aremote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
)

// resultCache is a goroutine-safe, fixed-capacity LRU of remote-agent outcomes
// keyed by a content hash of (endpoint, task, context). It mirrors the delegate
// runtime's in-process cache — and nexus.agent.agui_remote's, which is the same
// cache for the same reason — so identical delegated tasks replay without
// re-hitting the remote.
//
// ONLY SUCCESSES ARE STORED. The caller skips the put on any outcome carrying
// an error, which is the whole point: a remote that was briefly down, rate
// limited, or mid-deploy must be retried on the next call rather than answering
// from a cached failure until the process restarts.
type resultCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*cacheEntry
	head     *cacheEntry
	tail     *cacheEntry
}

type cacheEntry struct {
	key  string
	val  outcome
	prev *cacheEntry
	next *cacheEntry
}

// newResultCache returns a cache with the given capacity. Capacity <= 0
// disables eviction, letting the cache grow unbounded.
func newResultCache(capacity int) *resultCache {
	return &resultCache{
		capacity: capacity,
		items:    make(map[string]*cacheEntry),
	}
}

func (c *resultCache) get(key string) (outcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return outcome{}, false
	}
	c.promote(e)
	return e.val, true
}

// put stores an outcome. A caller must not call it for a failed outcome; see
// the type comment.
func (c *resultCache) put(key string, out outcome) {
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

// cacheKey is the content-addressable key over the remote's identity, the task
// and the canonicalized context map.
//
// The endpoint identity is the base URL plus both pinned endpoints, not just
// whichever one is active: two agents entries may name the same base while
// pinning different endpoints, and collapsing them would serve one agent's
// answer for the other. The posture name is folded in for the same reason
// delegate folds in the posture version — a different budget is a different
// call, even for an identical task.
func (a *remote) cacheKey(task string, contextMap map[string]any) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(a.cfg.baseURL)
	write(a.cfg.jsonrpcEndpoint)
	write(a.cfg.restEndpoint)
	write(a.cfg.posture)
	write(task)

	if len(contextMap) > 0 {
		keys := make([]string, 0, len(contextMap))
		for k := range contextMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			write(k)
			if data, err := json.Marshal(contextMap[k]); err == nil {
				h.Write(data)
			}
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
