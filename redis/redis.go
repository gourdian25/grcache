// File: redis/redis.go

// Package redis is grcache's primary production backend, built directly on
// gourdiantoken's proven Redis handling conventions: the same Ping-on-construct
// validation, the same error-wrapping style, and pipelining (not Lua/EVAL,
// which gourdiantoken's own docs claim but its code never actually uses) for
// atomic multi-key operations.
//
// Unlike gourdiantoken's Redis backend, which takes an already-built
// *redis.Client, this package owns a RedisConfig and builds its own client —
// grcache is meant to be usable standalone, without requiring callers to
// already have a *redis.Client on hand.
package redis

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/gourdian25/grcache"
)

const (
	valuePrefix = "grcache:val:"
	tagPrefix   = "grcache:tag:"

	defaultPoolSize     = 100 // matches gourdiantoken's own doc-comment example
	defaultDialTimeout  = 5 * time.Second
	defaultReadTimeout  = 3 * time.Second
	defaultWriteTimeout = 3 * time.Second
	pingTimeout         = 5 * time.Second // matches gourdiantoken's Ping-on-construct timeout
)

// RedisConfig configures a Cache constructed by NewRedisCache. Zero-valued
// fields fall back to sane defaults.
type RedisConfig struct {
	Addr         string // required
	Password     string
	DB           int
	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func (cfg RedisConfig) withDefaults() RedisConfig {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = defaultPoolSize
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	return cfg
}

// Cache is a Redis-backed implementation of grcache.Cache.
type Cache struct {
	client *goredis.Client

	closed    atomic.Bool
	closeOnce sync.Once

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ grcache.Cache = (*Cache)(nil)

// NewRedisCache builds a *redis.Client from cfg and validates connectivity
// with a Ping before returning, mirroring gourdiantoken's constructor-time
// validation step.
func NewRedisCache(cfg RedisConfig) (grcache.Cache, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("grcache/redis: RedisConfig.Addr is required")
	}
	cfg = cfg.withDefaults()

	client := goredis.NewClient(&goredis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if _, err := client.Ping(ctx).Result(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("grcache/redis: connect %s: %w", cfg.Addr, grcache.ErrCacheUnavailable)
	}

	return &Cache{client: client}, nil
}

func valueKey(key string) string { return valuePrefix + key }
func tagKey(tag string) string   { return tagPrefix + tag }

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, grcache.ErrClosed
	}

	val, err := c.client.Get(ctx, valueKey(key)).Bytes()
	if err != nil {
		if err == goredis.Nil {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/redis: get %q: %w", key, grcache.ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/redis: get %q: %w", key, grcache.ErrCacheUnavailable)
	}
	c.hits.Add(1)
	return val, nil
}

func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}
	if ttl < 0 {
		return grcache.ErrInvalidTTL
	}

	pipe := c.client.Pipeline()
	pipe.Set(ctx, valueKey(key), val, ttl)
	for _, tag := range tags {
		pipe.SAdd(ctx, tagKey(tag), key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("grcache/redis: set %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}

	if err := c.client.Del(ctx, valueKey(key)).Err(); err != nil {
		return fmt.Errorf("grcache/redis: delete %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, grcache.ErrClosed
	}

	n, err := c.client.Exists(ctx, valueKey(key)).Result()
	if err != nil {
		return false, fmt.Errorf("grcache/redis: exists %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return n > 0, nil
}

func (c *Cache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, grcache.ErrClosed
	}

	members, err := c.client.SMembers(ctx, tagKey(tag)).Result()
	if err != nil {
		return 0, fmt.Errorf("grcache/redis: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
	}
	if len(members) == 0 {
		return 0, nil
	}

	pipe := c.client.Pipeline()
	for _, member := range members {
		pipe.Del(ctx, valueKey(member))
	}
	pipe.Del(ctx, tagKey(tag))
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("grcache/redis: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
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
		KeyCount:  -1, // Redis has no cheap way to count keys under a prefix without SCAN.
	}, nil
}

// Close closes the underlying *redis.Client, guarded by sync.Once since
// go-redis errors on a double Close — the same reason gourdiantoken's own
// RedisTokenRepository.Close guards itself the same way.
func (c *Cache) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.client.Close()
	})
	return err
}
