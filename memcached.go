// File: memcached.go

// The memcached backend (memcachedCache) is grcache's secondary,
// lower-priority backend (behind Redis). It uses
// github.com/bradfitz/gomemcache/memcache, the de facto minimal Go
// memcached client.
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
package grcache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
)

const (
	// memcachedValuePrefix namespaces cache-value keys, mirroring the
	// Redis backend's own valuePrefix convention — previously this backend
	// stored cache values under the bare, unprefixed key while only its
	// tag-list keys were namespaced, a real gap (a cache value and a
	// same-named tag list could never collide in practice since Set always
	// writes both under different derived keys, but the value key itself
	// offered no defense-in-depth against an application key that happened
	// to collide with some other memcached user of the same server/pool).
	memcachedValuePrefix = "grcache:val:"

	tagKeyPrefix = "grcache:tag:"

	// relativeExpirationLimit is memcached's own cutoff: expirations at or
	// below this many seconds are relative to now; above it, they're treated
	// as an absolute Unix timestamp.
	relativeExpirationLimit = 30 * 24 * time.Hour
)

// MemcachedConfig configures a Cache constructed by NewMemcachedCache.
//
// Example:
//
//	cfg := grcache.MemcachedConfig{Servers: []string{"localhost:11211"}}
type MemcachedConfig struct {
	// Servers is the list of memcached server addresses. Required, e.g.
	// []string{"localhost:11211"}. Multiple servers are load-balanced by
	// the underlying client's consistent-hashing key distribution.
	Servers []string

	// Timeout bounds how long a single operation may take. Defaults to the
	// underlying client's own default (500ms) if zero.
	Timeout time.Duration

	// MaxIdleConns caps idle connections held per server. Defaults to the
	// underlying client's own default if zero.
	MaxIdleConns int

	// Logger receives optional diagnostic messages (connection failures,
	// shutdown). A nil Logger disables logging entirely.
	Logger Logger
}

// Cache is a memcached-backed implementation of Cache.
type memcachedCache struct {
	client *memcache.Client
	logger Logger

	closed    atomic.Bool
	closeOnce sync.Once

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ Cache = (*memcachedCache)(nil)

// NewMemcachedCache builds a *memcache.Client from cfg and validates
// connectivity with Ping before returning.
//
// Parameters:
//   - cfg: MemcachedConfig — Servers is required
//
// Returns:
//   - Cache: ready to use
//   - error: non-nil if Servers is empty or Ping fails, wrapping
//     ErrCacheUnavailable in the latter case
//
// Example:
//
//	cache, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{
//		Servers: []string{"localhost:11211"},
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
func NewMemcachedCache(cfg MemcachedConfig) (Cache, error) {
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("grcache/memcached: MemcachedConfig.Servers is required")
	}
	logger := OrNop(cfg.Logger)

	client := memcache.New(cfg.Servers...)
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	if cfg.MaxIdleConns > 0 {
		client.MaxIdleConns = cfg.MaxIdleConns
	}

	if err := client.Ping(); err != nil {
		logger.Error("grcache/memcached: connect failed", "servers", cfg.Servers, "error", err)
		return nil, fmt.Errorf("grcache/memcached: connect %v: %w", cfg.Servers, ErrCacheUnavailable)
	}

	logger.Info("grcache/memcached: connected", "servers", cfg.Servers)
	return &memcachedCache{client: client, logger: logger}, nil
}

func memcachedValueKey(key string) string { return memcachedValuePrefix + key }
func tagListKey(tag string) string        { return tagKeyPrefix + tag }

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

func (c *memcachedCache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}

	item, err := c.client.Get(memcachedValueKey(key))
	if err != nil {
		if err == memcache.ErrCacheMiss {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/memcached: get %q: %w", key, ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/memcached: get %q: %w", key, ErrCacheUnavailable)
	}
	c.hits.Add(1)
	return item.Value, nil
}

func (c *memcachedCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if ttl < 0 {
		return ErrInvalidTTL
	}

	item := &memcache.Item{Key: memcachedValueKey(key), Value: val, Expiration: expirationSeconds(ttl)}
	if err := c.client.Set(item); err != nil {
		return fmt.Errorf("grcache/memcached: set %q: %w", key, ErrCacheUnavailable)
	}

	for _, tag := range tags {
		if err := c.addToTagList(tag, key); err != nil {
			return fmt.Errorf("grcache/memcached: set %q tag %q: %w", key, tag, ErrCacheUnavailable)
		}
	}

	return nil
}

// addToTagList performs a best-effort read-modify-write of the tag's member
// list. Concurrent calls for the same tag can race; see the package doc.
func (c *memcachedCache) addToTagList(tag, key string) error {
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

func (c *memcachedCache) readTagList(tag string) ([]string, error) {
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

func (c *memcachedCache) writeTagList(tag string, members []string) error {
	if len(members) == 0 {
		return c.client.Delete(tagListKey(tag))
	}
	item := &memcache.Item{Key: tagListKey(tag), Value: []byte(strings.Join(members, "\n"))}
	return c.client.Set(item)
}

func (c *memcachedCache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return ErrClosed
	}

	err := c.client.Delete(memcachedValueKey(key))
	if err != nil && err != memcache.ErrCacheMiss {
		return fmt.Errorf("grcache/memcached: delete %q: %w", key, ErrCacheUnavailable)
	}
	return nil
}

func (c *memcachedCache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, ErrClosed
	}

	_, err := c.client.Get(memcachedValueKey(key))
	if err != nil {
		if err == memcache.ErrCacheMiss {
			return false, nil
		}
		return false, fmt.Errorf("grcache/memcached: exists %q: %w", key, ErrCacheUnavailable)
	}
	return true, nil
}

func (c *memcachedCache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, ErrClosed
	}

	members, err := c.readTagList(tag)
	if err != nil {
		return 0, fmt.Errorf("grcache/memcached: invalidate tag %q: %w", tag, ErrCacheUnavailable)
	}
	if len(members) == 0 {
		return 0, nil
	}

	for _, member := range members {
		if err := c.client.Delete(memcachedValueKey(member)); err != nil && err != memcache.ErrCacheMiss {
			return 0, fmt.Errorf("grcache/memcached: invalidate tag %q: %w", tag, ErrCacheUnavailable)
		}
	}
	if err := c.client.Delete(tagListKey(tag)); err != nil && err != memcache.ErrCacheMiss {
		return 0, fmt.Errorf("grcache/memcached: invalidate tag %q: %w", tag, ErrCacheUnavailable)
	}

	return len(members), nil
}

func (c *memcachedCache) Stats(ctx context.Context) (Stats, error) {
	if c.closed.Load() {
		return Stats{}, ErrClosed
	}

	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  -1, // memcached has no cheap way to count keys grcache owns.
	}, nil
}

func (c *memcachedCache) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.client.Close()
		c.logger.Info("grcache/memcached: cache closed")
	})
	return err
}
