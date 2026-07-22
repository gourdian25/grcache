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

func TestNewRedisCache_MissingAddr(t *testing.T) {
	if _, err := grcache.NewRedisCache(grcache.RedisConfig{}); err == nil {
		t.Fatal("NewRedisCache with empty Addr = nil error, want error")
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
