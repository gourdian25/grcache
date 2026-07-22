// File: redis.go

// The Redis backend (redisCache) is grcache's primary production backend,
// built directly on gourdiantoken's proven Redis handling conventions: the
// same Ping-on-construct validation, the same error-wrapping style, and a
// transactional pipeline (TxPipeline, i.e. MULTI/EXEC — not Lua/EVAL, which
// gourdiantoken's own docs claim but its code never actually uses) for
// atomic multi-key operations. TxPipeline, not the plain non-transactional
// Pipeline, is required here: a connection failure mid-batch on a plain
// Pipeline can leave a value written without all its tag memberships
// applied (or vice versa in InvalidateTag) — MULTI/EXEC is what actually
// makes "atomic multi-key operations" true rather than aspirational.
//
// Unlike gourdiantoken's Redis backend, which takes an already-built
// *redis.Client, this backend owns a RedisConfig and builds its own client —
// grcache is meant to be usable standalone, without requiring callers to
// already have a *redis.Client on hand.
package grcache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
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
// fields fall back to sane defaults (PoolSize 100, Dial/Read/WriteTimeout as
// documented on each field below) — only Addr is required.
//
// Example:
//
//	cfg := grcache.RedisConfig{Addr: "localhost:6379", Password: "secret", DB: 0}
type RedisConfig struct {
	// Addr is the Redis server address, e.g. "localhost:6379". Required.
	Addr string

	// Password authenticates with the server. Empty means no auth.
	Password string

	// DB selects the Redis logical database (0-15 by default server config).
	DB int

	// PoolSize is the maximum number of connections in the pool. Defaults
	// to 100 — the one number with a direct gourdiantoken precedent (its
	// own doc-comment example).
	PoolSize int

	// DialTimeout bounds how long connecting to Redis may take. Defaults to
	// 5s, matching gourdiantoken's own Ping-timeout precedent.
	DialTimeout time.Duration

	// ReadTimeout bounds how long a read may take. Defaults to 3s (go-redis's
	// own upstream default; gourdiantoken has no equivalent field to match).
	ReadTimeout time.Duration

	// WriteTimeout bounds how long a write may take. Defaults to 3s, same
	// rationale as ReadTimeout.
	WriteTimeout time.Duration

	// Logger receives optional diagnostic messages (connection failures,
	// shutdown). A nil Logger disables logging entirely.
	Logger Logger
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

// redisCache is a Redis-backed implementation of Cache.
type redisCache struct {
	client *goredis.Client
	logger Logger

	closed    atomic.Bool
	closeOnce sync.Once

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ Cache = (*redisCache)(nil)

// NewRedisCache builds a *redis.Client from cfg and validates connectivity
// with a Ping before returning, mirroring gourdiantoken's constructor-time
// validation step.
//
// Parameters:
//   - cfg: RedisConfig — Addr is required; all other fields default to
//     sane values (see RedisConfig's field docs)
//
// Returns:
//   - Cache: ready to use
//   - error: non-nil if Addr is empty or the connection/Ping fails, wrapping
//     ErrCacheUnavailable in the latter case
//
// Example:
//
//	cache, err := grcache.NewRedisCache(grcache.RedisConfig{Addr: "localhost:6379"})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
func NewRedisCache(cfg RedisConfig) (Cache, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("grcache/redis: RedisConfig.Addr is required")
	}
	cfg = cfg.withDefaults()
	logger := OrNop(cfg.Logger)

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
		logger.Errorf("grcache/redis: connect %s failed: %v", cfg.Addr, err)
		return nil, fmt.Errorf("grcache/redis: connect %s: %w", cfg.Addr, ErrCacheUnavailable)
	}

	logger.Infof("grcache/redis: connected to %s (db %d)", cfg.Addr, cfg.DB)
	return &redisCache{client: client, logger: logger}, nil
}

func valueKey(key string) string { return valuePrefix + key }
func tagKey(tag string) string   { return tagPrefix + tag }

func (c *redisCache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}

	val, err := c.client.Get(ctx, valueKey(key)).Bytes()
	if err != nil {
		if err == goredis.Nil {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/redis: get %q: %w", key, ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/redis: get %q: %w", key, ErrCacheUnavailable)
	}
	c.hits.Add(1)
	return val, nil
}

func (c *redisCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if ttl < 0 {
		return ErrInvalidTTL
	}

	pipe := c.client.TxPipeline()
	pipe.Set(ctx, valueKey(key), val, ttl)
	for _, tag := range tags {
		pipe.SAdd(ctx, tagKey(tag), key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("grcache/redis: set %q: %w", key, ErrCacheUnavailable)
	}
	return nil
}

func (c *redisCache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return ErrClosed
	}

	if err := c.client.Del(ctx, valueKey(key)).Err(); err != nil {
		return fmt.Errorf("grcache/redis: delete %q: %w", key, ErrCacheUnavailable)
	}
	return nil
}

func (c *redisCache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, ErrClosed
	}

	n, err := c.client.Exists(ctx, valueKey(key)).Result()
	if err != nil {
		return false, fmt.Errorf("grcache/redis: exists %q: %w", key, ErrCacheUnavailable)
	}
	return n > 0, nil
}

func (c *redisCache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, ErrClosed
	}

	members, err := c.client.SMembers(ctx, tagKey(tag)).Result()
	if err != nil {
		return 0, fmt.Errorf("grcache/redis: invalidate tag %q: %w", tag, ErrCacheUnavailable)
	}
	if len(members) == 0 {
		return 0, nil
	}

	pipe := c.client.TxPipeline()
	for _, member := range members {
		pipe.Del(ctx, valueKey(member))
	}
	pipe.Del(ctx, tagKey(tag))
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("grcache/redis: invalidate tag %q: %w", tag, ErrCacheUnavailable)
	}

	return len(members), nil
}

func (c *redisCache) Stats(ctx context.Context) (Stats, error) {
	if c.closed.Load() {
		return Stats{}, ErrClosed
	}

	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  -1, // Redis has no cheap way to count keys under a prefix without SCAN.
	}, nil
}

// Close closes the underlying *redis.Client, guarded by sync.Once since
// go-redis errors on a double Close — the same reason gourdiantoken's own
// RedisTokenRepository.Close guards itself the same way.
func (c *redisCache) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.client.Close()
		c.logger.Infof("grcache/redis: cache closed")
	})
	return err
}
