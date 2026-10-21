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
// By default this backend owns its client: NewRedisCache dials one from
// RedisConfig's Addr/Password/DB/pool fields and closes it on Close —
// grcache is meant to be usable standalone, without requiring callers to
// already have a *redis.Client on hand. Since v0.6.0 a caller that already
// holds a go-redis client (one pool shared with the rest of its backend,
// the way grnoti's PostgresConfig.Pool shares a pgxpool.Pool) can pass it
// as RedisConfig.Client instead: grcache uses it as given and never closes
// it. A *redis.ClusterClient or *redis.Ring is refused: the transactional
// pipeline above spans a value key and its tag keys, which a cluster
// refuses across hash slots (go-redis ErrCrossSlot) and a ring splits
// across shards, losing the atomicity this backend exists to provide.
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

// RedisConfig configures a Cache constructed by NewRedisCache. Exactly one
// of Addr or Client must be set: with Addr, grcache dials (and owns) its
// own client, and zero-valued fields fall back to sane defaults (PoolSize
// 100, Dial/Read/WriteTimeout as documented on each field below); with
// Client, it uses the caller's client as given and every connection field
// is ignored — only Logger still applies.
//
// Example:
//
//	cfg := grcache.RedisConfig{Addr: "localhost:6379", Password: "secret", DB: 0}
//
//	// or, sharing a client the rest of the application already uses:
//	cfg := grcache.RedisConfig{Client: rdb}
type RedisConfig struct {
	// Addr is the Redis server address, e.g. "localhost:6379". Exactly one
	// of Addr or Client must be set.
	Addr string

	// Client, if set, is used as given instead of dialing a new client
	// from Addr, so grcache can share one connection pool with the rest of
	// your application. NewRedisCache only Pings it, to validate it.
	// grcache never closes a Client it did not create: Close leaves it
	// open, and so does a failed Ping — closing it is the caller's job.
	// It must be a single-node or Sentinel (failover) client: a
	// *redis.ClusterClient or *redis.Ring is refused (see this file's doc
	// comment). Exactly one of Addr or Client must be set.
	Client goredis.UniversalClient

	// Password authenticates with the server. Empty means no auth. Ignored
	// when Client is set.
	Password string

	// DB selects the Redis logical database (0-15 by default server config).
	// Ignored when Client is set.
	DB int

	// PoolSize is the maximum number of connections in the pool. Defaults
	// to 100 — the one number with a direct gourdiantoken precedent (its
	// own doc-comment example). Ignored when Client is set — size the
	// client's pool yourself before passing it in.
	PoolSize int

	// DialTimeout bounds how long connecting to Redis may take. Defaults to
	// 5s, matching gourdiantoken's own Ping-timeout precedent. Ignored when
	// Client is set.
	DialTimeout time.Duration

	// ReadTimeout bounds how long a read may take. Defaults to 3s (go-redis's
	// own upstream default; gourdiantoken has no equivalent field to match).
	// Ignored when Client is set.
	ReadTimeout time.Duration

	// WriteTimeout bounds how long a write may take. Defaults to 3s, same
	// rationale as ReadTimeout. Ignored when Client is set.
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
	client goredis.UniversalClient
	// ownsClient is true only when NewRedisCache dialed client itself
	// from Addr; Close closes the client only then. A client injected
	// through RedisConfig.Client belongs to the caller.
	ownsClient bool
	logger     Logger

	closed    atomic.Bool
	closeOnce sync.Once

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ Cache = (*redisCache)(nil)

// NewRedisCache returns a Redis-backed Cache, validating connectivity with
// a Ping before returning (mirroring gourdiantoken's constructor-time
// validation step). With cfg.Addr it builds its own *redis.Client, which
// Close later closes; with cfg.Client it uses that client as given and
// never closes it.
//
// Parameters:
//   - cfg: RedisConfig — exactly one of Addr or Client is required; with
//     Addr, all other fields default to sane values (see RedisConfig's
//     field docs)
//
// Returns:
//   - Cache: ready to use
//   - error: non-nil if Addr and Client are both set or both empty, or
//     Client is a *redis.ClusterClient or *redis.Ring; wrapping
//     ErrCacheUnavailable if the connection/Ping fails
//
// Example:
//
//	cache, err := grcache.NewRedisCache(grcache.RedisConfig{Addr: "localhost:6379"})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
//
//	// Sharing an existing client (grcache will not close it):
//	cache, err := grcache.NewRedisCache(grcache.RedisConfig{Client: rdb})
func NewRedisCache(cfg RedisConfig) (Cache, error) {
	if (cfg.Addr == "") == (cfg.Client == nil) {
		return nil, fmt.Errorf("grcache/redis: exactly one of RedisConfig.Addr or RedisConfig.Client is required")
	}
	if cfg.Client != nil {
		return newRedisCacheFromClient(cfg.Client, OrNop(cfg.Logger))
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
		logger.Error("grcache/redis: connect failed", "addr", cfg.Addr, "error", err)
		return nil, fmt.Errorf("grcache/redis: connect %s: %w", cfg.Addr, ErrCacheUnavailable)
	}

	logger.Info("grcache/redis: connected", "addr", cfg.Addr, "db", cfg.DB)
	return &redisCache{client: client, ownsClient: true, logger: logger}, nil
}

// newRedisCacheFromClient is NewRedisCache's RedisConfig.Client path. The
// client is the caller's, so it is checked and Pinged but never closed
// here, not even when the Ping fails.
func newRedisCacheFromClient(client goredis.UniversalClient, logger Logger) (Cache, error) {
	switch client.(type) {
	case *goredis.ClusterClient, *goredis.Ring:
		// Set and InvalidateTag run one MULTI/EXEC over a value key and
		// its tag keys: a cluster refuses that across hash slots, a ring
		// splits it per shard. Refuse at startup, not on the first tagged Set.
		return nil, fmt.Errorf("grcache/redis: RedisConfig.Client must be a single-node or failover client, not %T", client)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if _, err := client.Ping(ctx).Result(); err != nil {
		logger.Error("grcache/redis: injected client ping failed", "error", err)
		return nil, fmt.Errorf("grcache/redis: ping injected client: %w", ErrCacheUnavailable)
	}

	logger.Info("grcache/redis: using injected client")
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

// Close closes the underlying client only if NewRedisCache created it
// (from RedisConfig.Addr); a client injected through RedisConfig.Client is
// left open for its owner to close. Guarded by sync.Once since go-redis
// errors on a double Close — the same reason gourdiantoken's own
// RedisTokenRepository.Close guards itself the same way.
func (c *redisCache) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.ownsClient {
			err = c.client.Close()
		}
		c.logger.Info("grcache/redis: cache closed")
	})
	return err
}
