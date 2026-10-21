// File: redis_test.go

package grcache_test

// Test connection reuses gourdiantoken's confirmed local Redis settings
// (localhost:6379, password "redis_password") for ecosystem consistency, but
// DB 14 (not gourdiantoken's DB 15) to eliminate any collision risk if both
// suites ever run against the same local Redis instance simultaneously. No
// miniredis, no testcontainers-go, no build tags — mirrors gourdiantoken's
// own real-Redis testing philosophy exactly.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/gourdian25/grcache"
)

const (
	redisTestAddr     = "localhost:6379"
	redisTestPassword = "redis_password"
	redisTestDB       = 14
)

func flushRedisTestDB(t *testing.T) {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: redisTestAddr, Password: redisTestPassword, DB: redisTestDB})
	defer func() { _ = client.Close() }()
	if err := client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("FlushDB: %v", err)
	}
}

func newRedisCache() (grcache.Cache, error) {
	return grcache.NewRedisCache(grcache.RedisConfig{
		Addr:     redisTestAddr,
		Password: redisTestPassword,
		DB:       redisTestDB,
	})
}

func newRedisCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	if _, err := newRedisCache(); err != nil {
		t.Skipf("Redis not available, skipping: %v", err)
	}
	flushRedisTestDB(t)
	cache, err := newRedisCache()
	if err != nil {
		t.Fatalf("newRedisCache: %v", err)
	}
	t.Cleanup(func() {
		_ = cache.Close()
		flushRedisTestDB(t)
	})
	return cache
}

// newInjectedRedisClient builds a go-redis client the test owns, the way a
// real caller owns one it passes as RedisConfig.Client: grcache must never
// close it, so the test closes it itself on cleanup.
func newInjectedRedisClient(t *testing.T) *goredis.Client {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: redisTestAddr, Password: redisTestPassword, DB: redisTestDB})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available, skipping: %v", err)
	}
	return client
}

func TestNewRedisCache_RequiresExactlyOneOfAddrOrClient(t *testing.T) {
	if _, err := grcache.NewRedisCache(grcache.RedisConfig{}); err == nil {
		t.Fatal("NewRedisCache with neither Addr nor Client = nil error, want error")
	}

	// NewClient doesn't dial, and the check runs before any Ping, so this
	// needs no live Redis.
	client := goredis.NewClient(&goredis.Options{Addr: redisTestAddr})
	defer func() { _ = client.Close() }()
	if _, err := grcache.NewRedisCache(grcache.RedisConfig{Addr: redisTestAddr, Client: client}); err == nil {
		t.Fatal("NewRedisCache with both Addr and Client = nil error, want error")
	}
}

func TestNewRedisCache_RejectsClusterAndRingClients(t *testing.T) {
	cluster := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{redisTestAddr}})
	defer func() { _ = cluster.Close() }()
	if _, err := grcache.NewRedisCache(grcache.RedisConfig{Client: cluster}); err == nil {
		t.Fatal("NewRedisCache with a *redis.ClusterClient = nil error, want error")
	}

	ring := goredis.NewRing(&goredis.RingOptions{Addrs: map[string]string{"shard": redisTestAddr}})
	defer func() { _ = ring.Close() }()
	if _, err := grcache.NewRedisCache(grcache.RedisConfig{Client: ring}); err == nil {
		t.Fatal("NewRedisCache with a *redis.Ring = nil error, want error")
	}
}

func TestNewRedisCache_InjectedClient(t *testing.T) {
	ctx := context.Background()
	client := newInjectedRedisClient(t)
	flushRedisTestDB(t)
	defer flushRedisTestDB(t)

	cache, err := grcache.NewRedisCache(grcache.RedisConfig{Client: client})
	if err != nil {
		t.Fatalf("NewRedisCache(Client): %v", err)
	}
	defer func() { _ = cache.Close() }()

	if err := cache.Set(ctx, "injected", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Reading the raw key back through the injected client proves the
	// cache wrote through that client (and its DB), not one of its own.
	got, err := client.Get(ctx, "grcache:val:injected").Bytes()
	if err != nil {
		t.Fatalf("raw Get through the injected client: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("raw value = %q, want %q", got, "v")
	}
}

func TestRedisCache_CloseLeavesInjectedClientOpen(t *testing.T) {
	ctx := context.Background()
	client := newInjectedRedisClient(t)

	cache, err := grcache.NewRedisCache(grcache.RedisConfig{Client: client})
	if err != nil {
		t.Fatalf("NewRedisCache(Client): %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("injected client Ping after cache Close: %v, want it still open", err)
	}
	if _, err := cache.Get(ctx, "x"); !errors.Is(err, grcache.ErrClosed) {
		t.Fatalf("Get after Close error = %v, want ErrClosed", err)
	}
}

func TestNewRedisCache_InjectedClientPingFailureLeavesClientOpen(t *testing.T) {
	client := goredis.NewClient(&goredis.Options{
		Addr:        "localhost:1", // nothing listens here
		DialTimeout: 200 * time.Millisecond,
	})
	defer func() { _ = client.Close() }()

	_, err := grcache.NewRedisCache(grcache.RedisConfig{Client: client})
	if !errors.Is(err, grcache.ErrCacheUnavailable) {
		t.Fatalf("NewRedisCache with an unreachable injected client error = %v, want ErrCacheUnavailable", err)
	}

	// Still a dial error, not "client is closed": the failed Ping must not
	// have closed a client grcache doesn't own.
	if err := client.Ping(context.Background()).Err(); err == nil || errors.Is(err, goredis.ErrClosed) {
		t.Fatalf("injected client Ping after failed NewRedisCache = %v, want a dial error, not ErrClosed", err)
	}
}

func TestNewRedisCache_BadAddr(t *testing.T) {
	_, err := grcache.NewRedisCache(grcache.RedisConfig{
		Addr:        "localhost:1", // nothing listens here
		DialTimeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("NewRedisCache with unreachable Addr = nil error, want error")
	}
}

func TestInvalidateTag_PipelinedAtScale(t *testing.T) {
	ctx := context.Background()
	cache := newRedisCacheForTest(t)

	const n = 500
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("scale-key-%d", i)
		if err := cache.Set(ctx, key, []byte("v"), time.Minute, "scale-tag"); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}

	count, err := cache.InvalidateTag(ctx, "scale-tag")
	if err != nil {
		t.Fatalf("InvalidateTag: %v", err)
	}
	if count != n {
		t.Fatalf("InvalidateTag returned %d, want %d", count, n)
	}
}

func TestRedisWithLogger(t *testing.T) {
	if _, err := newRedisCache(); err != nil {
		t.Skipf("Redis not available, skipping: %v", err)
	}
	logger := &recordingLogger{}
	flushRedisTestDB(t)
	cache, err := grcache.NewRedisCache(grcache.RedisConfig{
		Addr:     redisTestAddr,
		Password: redisTestPassword,
		DB:       redisTestDB,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("NewRedisCache: %v", err)
	}
	defer flushRedisTestDB(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

func TestKeyPrefixCollisionSafety(t *testing.T) {
	ctx := context.Background()
	cache := newRedisCacheForTest(t)

	// A key that collides textually with the tag namespace should still be
	// stored/retrieved correctly, since grcache prefixes value keys and tag
	// keys into disjoint Redis keyspaces.
	trickyKey := "tag:not-a-real-tag"
	if err := cache.Set(ctx, trickyKey, []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := cache.Get(ctx, trickyKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("Get = %q, want %q", got, "v")
	}
}
