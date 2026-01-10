package dnscache

import (
	"sync"
	"time"
)

// Cache is the interface for a DNS cache.
type Cache interface {
	// Get returns the IP addresses for the given key and its expiration time.
	// If the key is not found, it returns nil and a zero time.
	Get(key string) ([]string, error, time.Time)

	// Set sets the IP addresses for the given key with a TTL.
	Set(key string, ips []string, err error, ttl time.Duration)

	// Prune removes entries that haven't been used since the last Prune call.
	// It returns the number of removed entries.
	Prune() int
}

type cacheItem struct {
	ips      []string
	err      error
	expireAt time.Time
	used     bool
}

type memoryCache struct {
	mu    sync.RWMutex
	store map[string]*cacheItem
}

func newMemoryCache() *memoryCache {
	return &memoryCache{
		store: make(map[string]*cacheItem),
	}
}

func (c *memoryCache) Get(key string) ([]string, error, time.Time) {
	c.mu.RLock()
	item, ok := c.store[key]
	if !ok {
		c.mu.RUnlock()
		return nil, nil, time.Time{}
	}
	ips := item.ips
	err := item.err
	expireAt := item.expireAt
	used := item.used
	c.mu.RUnlock()

	if !used {
		c.mu.Lock()
		if item, ok := c.store[key]; ok {
			item.used = true
		}
		c.mu.Unlock()
	}

	return ips, err, expireAt
}

func (c *memoryCache) Set(key string, ips []string, err error, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = &cacheItem{
		ips:      ips,
		err:      err,
		expireAt: time.Now().Add(ttl),
		used:     true,
	}
}

func (c *memoryCache) Prune() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	deleted := 0
	for key, item := range c.store {
		if !item.used {
			delete(c.store, key)
			deleted++
		} else {
			// Reset used flag for the next cycle
			item.used = false
		}
	}
	return deleted
}
