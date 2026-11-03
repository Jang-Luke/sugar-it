package davfs

import (
	"strings"
	"sync"
	"time"
)

const cacheMax = 1024

type cacheEntry struct {
	v   any
	exp time.Time
}

// cache is a small TTL cache, bounded in entry count, safe for concurrent
// use. Writers invalidate by key prefix; a value computed before an
// invalidation is not stored afterwards (see gen/put), so a slow listing
// cannot resurrect stale data.
type cache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]cacheEntry
	gen uint64
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, m: map[string]cacheEntry{}}
}

func (c *cache) get(k string) (any, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.exp) {
		delete(c.m, k)
		return nil, false
	}
	return e.v, true
}

// generation is read before computing a value and passed to put.
func (c *cache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *cache) put(k string, v any, gen uint64) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	if _, ok := c.m[k]; !ok && len(c.m) >= cacheMax {
		now := time.Now()
		for key, e := range c.m {
			if now.After(e.exp) {
				delete(c.m, key)
			}
		}
		for key := range c.m { // still full: drop an arbitrary entry
			if len(c.m) < cacheMax {
				break
			}
			delete(c.m, key)
		}
	}
	c.m[k] = cacheEntry{v: v, exp: time.Now().Add(c.ttl)}
}

// drop removes the exact keys and every key under the given prefixes.
func (c *cache) drop(keys []string, prefixes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for _, k := range keys {
		delete(c.m, k)
	}
	if len(prefixes) == 0 {
		return
	}
	for k := range c.m {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				delete(c.m, k)
				break
			}
		}
	}
}
