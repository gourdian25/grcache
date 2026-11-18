// File: redis.go

// The Redis backend (redisCache) is grcache's primary production backend,
// built on gourdiantoken's Redis handling conventions: the same
// Ping-on-construct validation and the same error-wrapping style.
//
// Set and InvalidateTag each run as one Lua script (redisSetScript,
// redisInvalidateTagScript), which Redis executes atomically: no other
// command runs between a script's first command and its last. v0.1.1 moved
// them from a plain Pipeline to MULTI/EXEC so a dropped connection couldn't
// leave a value without its tags; that made each batch atomic, but
// InvalidateTag still read the tag's members (SMEMBERS) in one round trip
// and deleted them in another, and a Set landing between the two kept its
// value while its tag membership was deleted, so no later InvalidateTag
// reached it (v0.7.0 fixes it). Inside one script the read and the deletes
// can't be split.
//
// A tag set lives at least as long as the longest-lived value in it: Set
// extends a tag's expiry to its value's TTL when that is later, and makes
// the tag persistent for a value with no TTL. Before v0.7.0 tag sets never
// expired, so every tag ever used stayed in Redis. A member whose value has
// expired or been deleted stays in its tag set until the tag expires or is
// invalidated; InvalidateTag counts only the values it actually deleted.
//
// By default this backend owns its client: NewRedisCache dials one from
// RedisConfig's Addr/Password/DB/pool fields and closes it on Close —
// grcache is meant to be usable standalone, without requiring callers to
// already have a *redis.Client on hand. Since v0.6.0 a caller that already
// holds a go-redis client (one pool shared with the rest of its backend,
// the way grnoti's PostgresConfig.Pool shares a pgxpool.Pool) can pass it
// as RedisConfig.Client instead: grcache uses it as given and never closes
// it. A *redis.ClusterClient or *redis.Ring is refused: both scripts touch a
// value key and its tag keys together, which a cluster refuses across hash
// slots and a ring splits across shards, losing the atomicity this backend
// exists to provide.
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

// redisSetScript writes a value and adds it to each of its tags, atomically.
// KEYS[1] is the value key and KEYS[2..] its tag keys; ARGV[1] is the value,
// ARGV[2] its TTL in milliseconds (0: none) and ARGV[3] the caller's key,
// the member stored in each tag set. A tag's expiry only ever grows: a new
// tag takes the value's TTL, a value with no TTL makes its tag persistent,
// and a persistent tag stays persistent.
var redisSetScript = goredis.NewScript(`
local ttl = tonumber(ARGV[2])
if ttl > 0 then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ttl)
else
  redis.call('SET', KEYS[1], ARGV[1])
end
for i = 2, #KEYS do
  local current = redis.call('PTTL', KEYS[i])
  redis.call('SADD', KEYS[i], ARGV[3])
  if ttl == 0 then
    redis.call('PERSIST', KEYS[i])
  elseif current == -2 or (current >= 0 and current < ttl) then
    redis.call('PEXPIRE', KEYS[i], ttl)
  end
end
return 1
`)

// redisInvalidateTagScript deletes every value in a tag and the tag itself,
// atomically, and returns how many values it deleted (a member whose value
// already expired or was deleted isn't counted). KEYS[1] is the tag key;
// ARGV[1] is the value key prefix. The value keys are built inside the
// script, so a tag's size isn't limited by how many keys one call can carry.
var redisInvalidateTagScript = goredis.NewScript(`
local members = redis.call('SMEMBERS', KEYS[1])
local deleted = 0
for _, member in ipairs(members) do
  deleted = deleted + redis.call('DEL', ARGV[1] .. member)
end
redis.call('DEL', KEYS[1])
return deleted
`)

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

	keys := make([]string, 0, len(tags)+1)
	keys = append(keys, valueKey(key))
	for _, tag := range tags {
		keys = append(keys, tagKey(tag))
	}
	if err := redisSetScript.Run(ctx, c.client, keys, val, ttlMilliseconds(ttl), key).Err(); err != nil {
		return fmt.Errorf("grcache/redis: set %q: %w", key, ErrCacheUnavailable)
	}
	return nil
}

// ttlMilliseconds is ttl in whole milliseconds, rounded up so a positive TTL
// never becomes 0 (no expiry); 0 stays 0.
func ttlMilliseconds(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return int64((ttl + time.Millisecond - 1) / time.Millisecond)
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

	n, err := redisInvalidateTagScript.Run(ctx, c.client, []string{tagKey(tag)}, valuePrefix).Int()
	if err != nil {
		return 0, fmt.Errorf("grcache/redis: invalidate tag %q: %w", tag, ErrCacheUnavailable)
	}
	return n, nil
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
