package main

import (
	"errors"
	"sync"
	"time"
)

// Cache is a TTL cache in front of Reddit.
//
//   - Concurrent misses for the same key share a single upstream fetch.
//   - "Not found" / "forbidden" answers are cached too (for negativeTTL), so
//     unknown subreddit names cannot be used to hammer Reddit.
//   - If a refresh fails, the last good value is served (for up to staleTTL)
//     and the refresh is retried after retryAfter. With no good value to
//     fall back on, the error itself is cached for retryAfter.
type Cache[V any] struct {
	ttl         time.Duration
	negativeTTL time.Duration
	staleTTL    time.Duration
	retryAfter  time.Duration

	mu       sync.Mutex
	entries  map[string]*cacheEntry[V]
	inflight map[string]*cacheCall[V]
}

type cacheEntry[V any] struct {
	value   V
	err     error // non-nil for a cached negative answer
	fetched time.Time
	expires time.Time
}

type cacheCall[V any] struct {
	done  chan struct{}
	entry *cacheEntry[V]
}

// NewCache creates a cache and starts a janitor that evicts old entries.
func NewCache[V any](ttl, negativeTTL, staleTTL time.Duration) *Cache[V] {
	c := &Cache[V]{
		ttl:         ttl,
		negativeTTL: negativeTTL,
		staleTTL:    staleTTL,
		retryAfter:  time.Minute,
		entries:     make(map[string]*cacheEntry[V]),
		inflight:    make(map[string]*cacheCall[V]),
	}
	go c.janitor()
	return c
}

// Get returns the cached value for key, calling fetch if it is missing or
// expired. It also returns when the value was fetched from Reddit and when it
// will be refreshed.
func (c *Cache[V]) Get(key string, fetch func() (V, error)) (V, time.Time, time.Time, error) {
	c.mu.Lock()
	now := time.Now()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.value, e.fetched, e.expires, e.err
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.entry.value, call.entry.fetched, call.entry.expires, call.entry.err
	}
	call := &cacheCall[V]{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	value, err := fetch()

	c.mu.Lock()
	now = time.Now()
	switch {
	case err == nil:
		call.entry = &cacheEntry[V]{value: value, fetched: now, expires: now.Add(c.ttl)}
		c.entries[key] = call.entry
	case errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden):
		call.entry = &cacheEntry[V]{err: err, fetched: now, expires: now.Add(c.negativeTTL)}
		c.entries[key] = call.entry
	default:
		if old, ok := c.entries[key]; ok && old.err == nil && now.Sub(old.fetched) < c.staleTTL {
			// Serve the stale copy and back off before trying Reddit again.
			old.expires = now.Add(c.retryAfter)
			call.entry = old
		} else {
			// Nothing to fall back on: remember the failure briefly so an
			// outage does not turn every request into an upstream call.
			call.entry = &cacheEntry[V]{err: err, fetched: now, expires: now.Add(c.retryAfter)}
			c.entries[key] = call.entry
		}
	}
	delete(c.inflight, key)
	c.mu.Unlock()
	close(call.done)

	return call.entry.value, call.entry.fetched, call.entry.expires, call.entry.err
}

func (c *Cache[V]) janitor() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.evict(time.Now())
	}
}

// evict drops expired entries, keeping good ones while they can still be
// served stale.
func (c *Cache[V]) evict(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if now.After(e.expires) && (e.err != nil || now.Sub(e.fetched) > c.staleTTL) {
			delete(c.entries, key)
		}
	}
}
