// File: memcached/memcached.go

// Package memcached is grcache's secondary, lower-priority backend (behind
// Redis). It uses github.com/bradfitz/gomemcache/memcache, the de facto
// minimal Go memcached client.
//
// Tag invalidation is emulated on top of memcached's flat key-value model:
// each tag is itself a memcached key holding a newline-delimited list of
// member keys, updated via a read-modify-write on Set. This is explicitly
// best-effort and eventually consistent, unlike Redis's Set-based tag
// storage — memcached has no atomic list/set type and no scripting analog to
// Lua/pipelining, so concurrent Set calls tagging the same tag can race and
// drop a list member. A dropped member means that key simply won't be swept
// up by a later InvalidateTag call for that tag; it is not a correctness bug
// for Get/Set/Delete/Exists on the key itself, only for the invalidation
// guarantee.
package memcached

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/gourdian25/grcache"
)

const (
	tagKeyPrefix = "grcache:tag:"

	// relativeExpirationLimit is memcached's own cutoff: expirations at or
	// below this many seconds are relative to now; above it, they're treated
	// as an absolute Unix timestamp.
	relativeExpirationLimit = 30 * 24 * time.Hour
)

// MemcachedConfig configures a Cache constructed by NewMemcachedCache.
type MemcachedConfig struct {
	Servers      []string // required, e.g. []string{"localhost:11211"}
	Timeout      time.Duration
	MaxIdleConns int

	// Logger receives optional diagnostic messages (connection failures,
	// shutdown). A nil Logger disables logging entirely.
	Logger grcache.Logger
}

// Cache is a memcached-backed implementation of grcache.Cache.
type Cache struct {
	client *memcache.Client
	logger grcache.Logger

	closed    atomic.Bool
	closeOnce sync.Once

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ grcache.Cache = (*Cache)(nil)

// NewMemcachedCache builds a *memcache.Client from cfg and validates
// connectivity with Ping before returning.
func NewMemcachedCache(cfg MemcachedConfig) (grcache.Cache, error) {
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("grcache/memcached: MemcachedConfig.Servers is required")
	}
	logger := grcache.OrNop(cfg.Logger)

	client := memcache.New(cfg.Servers...)
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	if cfg.MaxIdleConns > 0 {
		client.MaxIdleConns = cfg.MaxIdleConns
	}

	if err := client.Ping(); err != nil {
		logger.Errorf("grcache/memcached: connect %v failed: %v", cfg.Servers, err)
		return nil, fmt.Errorf("grcache/memcached: connect %v: %w", cfg.Servers, grcache.ErrCacheUnavailable)
	}

	logger.Infof("grcache/memcached: connected to %v", cfg.Servers)
	return &Cache{client: client, logger: logger}, nil
}

func tagListKey(tag string) string { return tagKeyPrefix + tag }

// expirationSeconds converts a ttl into memcached's Expiration convention:
// 0 means no expiry, values up to 30 days are relative, longer ttls are
// converted to an absolute Unix timestamp.
//
// memcached only supports second-granularity expiry, and an Expiration of 0
// means "never expire" — so a positive sub-second ttl must round up to 1
// second, not truncate down to 0, or it would silently live forever instead
// of expiring almost immediately.
func expirationSeconds(ttl time.Duration) int32 {
	if ttl <= 0 {
		return 0
	}
	if ttl <= relativeExpirationLimit {
		if ttl < time.Second {
			return 1
		}
		return int32(ttl.Seconds())
	}
	return int32(time.Now().Add(ttl).Unix())
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, grcache.ErrClosed
	}

	item, err := c.client.Get(key)
	if err != nil {
		if err == memcache.ErrCacheMiss {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/memcached: get %q: %w", key, grcache.ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/memcached: get %q: %w", key, grcache.ErrCacheUnavailable)
	}
	c.hits.Add(1)
	return item.Value, nil
}

func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}
	if ttl < 0 {
		return grcache.ErrInvalidTTL
	}

	item := &memcache.Item{Key: key, Value: val, Expiration: expirationSeconds(ttl)}
	if err := c.client.Set(item); err != nil {
		return fmt.Errorf("grcache/memcached: set %q: %w", key, grcache.ErrCacheUnavailable)
	}

	for _, tag := range tags {
		if err := c.addToTagList(tag, key); err != nil {
			return fmt.Errorf("grcache/memcached: set %q tag %q: %w", key, tag, grcache.ErrCacheUnavailable)
		}
	}

	return nil
}

// addToTagList performs a best-effort read-modify-write of the tag's member
// list. Concurrent calls for the same tag can race; see the package doc.
func (c *Cache) addToTagList(tag, key string) error {
	members, err := c.readTagList(tag)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m == key {
			return nil // already a member
		}
	}
	members = append(members, key)
	return c.writeTagList(tag, members)
}

func (c *Cache) readTagList(tag string) ([]string, error) {
	item, err := c.client.Get(tagListKey(tag))
	if err != nil {
		if err == memcache.ErrCacheMiss {
			return nil, nil
		}
		return nil, err
	}
	if len(item.Value) == 0 {
		return nil, nil
	}
	return strings.Split(string(item.Value), "\n"), nil
}

func (c *Cache) writeTagList(tag string, members []string) error {
	if len(members) == 0 {
		return c.client.Delete(tagListKey(tag))
	}
	item := &memcache.Item{Key: tagListKey(tag), Value: []byte(strings.Join(members, "\n"))}
	return c.client.Set(item)
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}

	err := c.client.Delete(key)
	if err != nil && err != memcache.ErrCacheMiss {
		return fmt.Errorf("grcache/memcached: delete %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, grcache.ErrClosed
	}

	_, err := c.client.Get(key)
	if err != nil {
		if err == memcache.ErrCacheMiss {
			return false, nil
		}
		return false, fmt.Errorf("grcache/memcached: exists %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return true, nil
}

func (c *Cache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, grcache.ErrClosed
	}

	members, err := c.readTagList(tag)
	if err != nil {
		return 0, fmt.Errorf("grcache/memcached: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
	}
	if len(members) == 0 {
		return 0, nil
	}

	for _, member := range members {
		if err := c.client.Delete(member); err != nil && err != memcache.ErrCacheMiss {
			return 0, fmt.Errorf("grcache/memcached: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
		}
	}
	if err := c.client.Delete(tagListKey(tag)); err != nil && err != memcache.ErrCacheMiss {
		return 0, fmt.Errorf("grcache/memcached: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
	}

	return len(members), nil
}

func (c *Cache) Stats(ctx context.Context) (grcache.Stats, error) {
	if c.closed.Load() {
		return grcache.Stats{}, grcache.ErrClosed
	}

	return grcache.Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  -1, // memcached has no cheap way to count keys grcache owns.
	}, nil
}

func (c *Cache) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.client.Close()
		c.logger.Infof("grcache/memcached: cache closed")
	})
	return err
}
