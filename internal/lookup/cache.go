package lookup

import (
	"sync"
	"time"
)

// cache is a bounded in-memory TTL cache of entries. A zero TTL or size turns it off.
type cache struct {
	ttl  time.Duration
	size int
	now  func() time.Time

	mu    sync.Mutex
	items map[string]cacheItem
}

type cacheItem struct {
	word    *Word
	expires time.Time
}

func newCache(ttl time.Duration, size int) *cache {
	return &cache{ttl: ttl, size: size, now: time.Now, items: map[string]cacheItem{}}
}

func (c *cache) get(key string) (*Word, bool) {
	if c.ttl <= 0 || c.size <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(it.expires) {
		delete(c.items, key)
		return nil, false
	}
	return it.word, true
}

func (c *cache) put(key string, w *Word) {
	if c.ttl <= 0 || c.size <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists && len(c.items) >= c.size {
		c.evict()
	}
	c.items[key] = cacheItem{word: w, expires: c.now().Add(c.ttl)}
}

// evict drops the expired items and, when the cache is still full, the one that expires first.
func (c *cache) evict() {
	now := c.now()
	var oldestKey string
	var oldest time.Time
	for k, it := range c.items {
		if !now.Before(it.expires) {
			delete(c.items, k)
			continue
		}
		if oldestKey == "" || it.expires.Before(oldest) {
			oldestKey, oldest = k, it.expires
		}
	}
	if len(c.items) >= c.size && oldestKey != "" {
		delete(c.items, oldestKey)
	}
}
