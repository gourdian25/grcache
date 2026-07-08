// File: memory/memory.go

// Package memory is grcache's default, zero-external-dependency backend. It
// stores entries in a single mutex-protected map with a background sweep
// goroutine for TTL expiry, and is intended for local development, testing,
// and single-process use — it does not coordinate state across processes or
// replicas.
package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gourdian25/grcache"
)

const defaultSweepInterval = 30 * time.Second

// entry holds a single cached value.
type entry struct {
	value     []byte
	expiresAt time.Time // zero value means "no expiry"
	tags      []string
}

func (e entry) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// Option configures a Cache constructed by NewMemoryCache.
type Option func(*Cache)

// WithSweepInterval sets how often the background goroutine sweeps expired
// entries. The default is 30 seconds. Sweeping is a memory-reclamation
// optimization only — Get and Exists always check expiry lazily too, so
// correctness never depends on the sweep interval.
func WithSweepInterval(d time.Duration) Option {
	return func(c *Cache) {
		if d > 0 {
			c.sweepInterval = d
		}
	}
}

// Cache is an in-memory implementation of grcache.Cache.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	tags map[string]map[string]struct{} // tag -> set of keys

	sweepInterval time.Duration
	closed        atomic.Bool
	closeChan     chan struct{}
	wg            sync.WaitGroup

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ grcache.Cache = (*Cache)(nil)

// NewMemoryCache constructs a ready-to-use in-memory Cache.
func NewMemoryCache(opts ...Option) (grcache.Cache, error) {
	c := &Cache{
		data:          make(map[string]entry),
		tags:          make(map[string]map[string]struct{}),
		sweepInterval: defaultSweepInterval,
		closeChan:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}

	c.wg.Add(1)
	go c.sweepLoop()

	return c, nil
}

func (c *Cache) sweepLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.sweep()
		case <-c.closeChan:
			return
		}
	}
}

func (c *Cache) sweep() {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	for key, e := range c.data {
		if e.expired(now) {
			c.removeLocked(key, e)
			c.evictions.Add(1)
		}
	}
}

// removeLocked deletes key from data and from every tag set it belongs to.
// Callers must hold c.mu for writing.
func (c *Cache) removeLocked(key string, e entry) {
	delete(c.data, key)
	for _, tag := range e.tags {
		if members, ok := c.tags[tag]; ok {
			delete(members, key)
			if len(members) == 0 {
				delete(c.tags, tag)
			}
		}
	}
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, grcache.ErrClosed
	}

	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()

	if !ok || e.expired(time.Now()) {
		c.misses.Add(1)
		return nil, grcache.ErrKeyNotFound
	}

	c.hits.Add(1)
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}
	if ttl < 0 {
		return grcache.ErrInvalidTTL
	}

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	valCopy := make([]byte, len(val))
	copy(valCopy, val)

	var tagsCopy []string
	if len(tags) > 0 {
		tagsCopy = make([]string, len(tags))
		copy(tagsCopy, tags)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if old, ok := c.data[key]; ok {
		c.removeLocked(key, old)
	}

	c.data[key] = entry{value: valCopy, expiresAt: expiresAt, tags: tagsCopy}
	for _, tag := range tagsCopy {
		members, ok := c.tags[tag]
		if !ok {
			members = make(map[string]struct{})
			c.tags[tag] = members
		}
		members[key] = struct{}{}
	}

	return nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.data[key]; ok {
		c.removeLocked(key, e)
	}
	return nil
}

func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, grcache.ErrClosed
	}

	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()

	if !ok || e.expired(time.Now()) {
		return false, nil
	}
	return true, nil
}

func (c *Cache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, grcache.ErrClosed
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	members, ok := c.tags[tag]
	if !ok {
		return 0, nil
	}

	n := 0
	for key := range members {
		if e, ok := c.data[key]; ok {
			delete(c.data, key)
			n++
			// Remove key from every other tag it belongs to as well.
			for _, otherTag := range e.tags {
				if otherTag == tag {
					continue
				}
				if otherMembers, ok := c.tags[otherTag]; ok {
					delete(otherMembers, key)
					if len(otherMembers) == 0 {
						delete(c.tags, otherTag)
					}
				}
			}
		}
	}
	delete(c.tags, tag)

	return n, nil
}

func (c *Cache) Stats(ctx context.Context) (grcache.Stats, error) {
	if c.closed.Load() {
		return grcache.Stats{}, grcache.ErrClosed
	}

	c.mu.RLock()
	keyCount := int64(len(c.data))
	c.mu.RUnlock()

	return grcache.Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  keyCount,
	}, nil
}

func (c *Cache) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.closeChan)
	c.wg.Wait()
	return nil
}
