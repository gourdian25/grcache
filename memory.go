// File: memory.go

// The memory backend (memoryCache) is grcache's default,
// zero-external-dependency backend. It stores entries in a single
// mutex-protected map with a background sweep goroutine for TTL expiry, and
// is intended for local development, testing, and single-process use — it
// does not coordinate state across processes or replicas.
package grcache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
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

// MemoryOption configures a Cache constructed by NewMemoryCache.
type MemoryOption func(*memoryCache)

// WithSweepInterval sets how often the background goroutine sweeps expired
// entries. The default is 30 seconds. Sweeping is a memory-reclamation
// optimization only — Get and Exists always check expiry lazily too, so
// correctness never depends on the sweep interval.
//
// Parameters:
//   - d: time.Duration — ignored if <= 0, leaving the default in place
//
// Example:
//
//	cache, err := grcache.NewMemoryCache(grcache.WithSweepInterval(5 * time.Second))
func WithSweepInterval(d time.Duration) MemoryOption {
	return func(c *memoryCache) {
		if d > 0 {
			c.sweepInterval = d
		}
	}
}

// WithLogger installs an optional Logger for diagnostic messages
// (sweep-cycle summaries, shutdown). Logging is always opt-in; without this
// option the cache logs nothing.
//
// Parameters:
//   - l: Logger — a nil value is equivalent to omitting this option
//
// Example:
//
//	cache, err := grcache.NewMemoryCache(grcache.WithLogger(grlog.NewDefaultLogger()))
func WithLogger(l Logger) MemoryOption {
	return func(c *memoryCache) {
		c.logger = OrNop(l)
	}
}

// memoryCache is an in-memory implementation of Cache. It has zero
// external dependencies and does not coordinate state across processes or
// replicas — running it behind multiple instances of an application means
// each instance has its own independent cache that will diverge from the
// others, which is expected, not a bug. Use NewRedisCache, NewPostgresCache,
// or NewMongoCache for state that must be shared.
type memoryCache struct {
	mu   sync.RWMutex
	data map[string]entry
	tags map[string]map[string]struct{} // tag -> set of keys

	sweepInterval time.Duration
	logger        Logger
	closed        atomic.Bool
	closeChan     chan struct{}
	wg            sync.WaitGroup

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ Cache = (*memoryCache)(nil)

// NewMemoryCache constructs a ready-to-use in-memory Cache. Construction
// never fails — the error return exists only to match the signature every
// other backend's constructor uses.
//
// Parameters:
//   - opts: ...MemoryOption — WithSweepInterval and/or WithLogger; both optional
//
// Returns:
//   - Cache: ready to use immediately
//   - error: always nil
//
// Example:
//
//	cache, err := grcache.NewMemoryCache()
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
func NewMemoryCache(opts ...MemoryOption) (Cache, error) {
	c := &memoryCache{
		data:          make(map[string]entry),
		tags:          make(map[string]map[string]struct{}),
		sweepInterval: defaultSweepInterval,
		logger:        NopLogger(),
		closeChan:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}

	c.logger.Info("grcache/memory: cache started", "sweep_interval", c.sweepInterval)

	c.wg.Add(1)
	go c.sweepLoop()

	return c, nil
}

func (c *memoryCache) sweepLoop() {
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

func (c *memoryCache) sweep() {
	now := time.Now()

	c.mu.Lock()
	swept := 0
	for key, e := range c.data {
		if e.expired(now) {
			c.removeLocked(key, e)
			c.evictions.Add(1)
			swept++
		}
	}
	c.mu.Unlock()

	if swept > 0 {
		c.logger.Debug("grcache/memory: sweep reclaimed expired entries", "count", swept)
	}
}

// removeLocked deletes key from data and from every tag set it belongs to.
// Callers must hold c.mu for writing.
func (c *memoryCache) removeLocked(key string, e entry) {
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

func (c *memoryCache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}

	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()

	if !ok || e.expired(time.Now()) {
		c.misses.Add(1)
		return nil, ErrKeyNotFound
	}

	c.hits.Add(1)
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

func (c *memoryCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if ttl < 0 {
		return ErrInvalidTTL
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

func (c *memoryCache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return ErrClosed
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.data[key]; ok {
		c.removeLocked(key, e)
	}
	return nil
}

func (c *memoryCache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, ErrClosed
	}

	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()

	if !ok || e.expired(time.Now()) {
		return false, nil
	}
	return true, nil
}

func (c *memoryCache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, ErrClosed
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

func (c *memoryCache) Stats(ctx context.Context) (Stats, error) {
	if c.closed.Load() {
		return Stats{}, ErrClosed
	}

	c.mu.RLock()
	keyCount := int64(len(c.data))
	c.mu.RUnlock()

	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  keyCount,
	}, nil
}

func (c *memoryCache) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.closeChan)
	c.wg.Wait()
	c.logger.Info("grcache/memory: cache closed")
	return nil
}
