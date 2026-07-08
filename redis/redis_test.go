// File: redis/redis_test.go

package redis_test

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
	"github.com/gourdian25/grcache/conformance"
	"github.com/gourdian25/grcache/redis"
)

const (
	testAddr     = "localhost:6379"
	testPassword = "redis_password"
	testDB       = 14
)

func flushTestDB(t *testing.T) {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: testAddr, Password: testPassword, DB: testDB})
	defer client.Close()
	if err := client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("FlushDB: %v", err)
	}
}

func newCache() (grcache.Cache, error) {
	return redis.NewRedisCache(redis.RedisConfig{
		Addr:     testAddr,
		Password: testPassword,
		DB:       testDB,
	})
}

func newCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	flushTestDB(t)
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	t.Cleanup(func() {
		cache.Close()
		flushTestDB(t)
	})
	return cache
}

func TestConformance(t *testing.T) {
	flushTestDB(t)
	conformance.Run(t, newCache)
	flushTestDB(t)
}

func TestNewRedisCache_MissingAddr(t *testing.T) {
	if _, err := redis.NewRedisCache(redis.RedisConfig{}); err == nil {
		t.Fatal("NewRedisCache with empty Addr = nil error, want error")
	}
}

func TestNewRedisCache_BadAddr(t *testing.T) {
	_, err := redis.NewRedisCache(redis.RedisConfig{
		Addr:        "localhost:1", // nothing listens here
		DialTimeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("NewRedisCache with unreachable Addr = nil error, want error")
	}
}

func TestInvalidateTag_PipelinedAtScale(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

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

func TestKeyPrefixCollisionSafety(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

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
