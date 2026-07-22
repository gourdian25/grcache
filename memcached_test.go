// File: memcached_test.go

package grcache_test

// Test connection uses the standard memcached default port (localhost:11211)
// — unlike Redis's DB-index or Postgres/Mongo's database-name concerns,
// memcached has no namespace-within-instance concept to collide on, so no
// special isolation convention is needed beyond flushing before each test.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	gomemcache "github.com/bradfitz/gomemcache/memcache"

	"github.com/gourdian25/grcache"
)

const memcachedTestAddr = "localhost:11211"

func flushMemcachedTestServer(t *testing.T) {
	t.Helper()
	client := gomemcache.New(memcachedTestAddr)
	defer func() { _ = client.Close() }()
	if err := client.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
}

func newMemcachedCache() (grcache.Cache, error) {
	return grcache.NewMemcachedCache(grcache.MemcachedConfig{Servers: []string{memcachedTestAddr}})
}

func newMemcachedCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	if _, err := newMemcachedCache(); err != nil {
		t.Skipf("memcached not available, skipping: %v", err)
	}
	flushMemcachedTestServer(t)
	cache, err := newMemcachedCache()
	if err != nil {
		t.Fatalf("newMemcachedCache: %v", err)
	}
	t.Cleanup(func() {
		_ = cache.Close()
		flushMemcachedTestServer(t)
	})
	return cache
}

func TestNewMemcachedCache_MissingServers(t *testing.T) {
	if _, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{}); err == nil {
		t.Fatal("NewMemcachedCache with no Servers = nil error, want error")
	}
}

func TestNewMemcachedCache_MaxIdleConns(t *testing.T) {
	if _, err := newMemcachedCache(); err != nil {
		t.Skipf("memcached not available, skipping: %v", err)
	}
	cache, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{
		Servers:      []string{memcachedTestAddr},
		MaxIdleConns: 5,
	})
	if err != nil {
		t.Fatalf("NewMemcachedCache with MaxIdleConns: %v", err)
	}
	defer func() { _ = cache.Close() }()
}

func TestNewMemcachedCache_Unreachable(t *testing.T) {
	_, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{
		Servers: []string{"localhost:1"}, // nothing listens here
		Timeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("NewMemcachedCache with unreachable server = nil error, want error")
	}
}

// TestLongTTLConversion exercises a ttl beyond memcached's 30-day
// relative-expiration cutoff (relativeExpirationLimit in memcached.go),
// which must be converted to an absolute Unix timestamp rather than passed
// through as a relative second count. This proves the conversion round-trips
// correctly through a real memcached server, not just that the Go duration
// math in expirationSeconds is correct in isolation.
func TestLongTTLConversion(t *testing.T) {
	ctx := context.Background()
	cache := newMemcachedCacheForTest(t)

	const longTTL = 45 * 24 * time.Hour // > the 30-day relative cutoff

	if err := cache.Set(ctx, "long-ttl-key", []byte("still-here"), longTTL); err != nil {
		t.Fatalf("Set with 45-day ttl: %v", err)
	}

	val, err := cache.Get(ctx, "long-ttl-key")
	if err != nil {
		t.Fatalf("Get after 45-day-ttl Set: %v (indicates the absolute-timestamp conversion is broken)", err)
	}
	if string(val) != "still-here" {
		t.Fatalf("Get = %q, want %q", val, "still-here")
	}

	if exists, err := cache.Exists(ctx, "long-ttl-key"); err != nil || !exists {
		t.Fatalf("Exists = (%v, %v), want (true, nil)", exists, err)
	}
}

func TestMemcachedWithLogger(t *testing.T) {
	if _, err := newMemcachedCache(); err != nil {
		t.Skipf("memcached not available, skipping: %v", err)
	}
	logger := &recordingLogger{}
	flushMemcachedTestServer(t)
	cache, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{
		Servers: []string{memcachedTestAddr},
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewMemcachedCache: %v", err)
	}
	defer flushMemcachedTestServer(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

// TestTagListRaceIsDocumentedNotFixed exercises concurrent Set calls tagging
// the same tag and documents (rather than "fixes") that the list-based
// emulation can lose a member under a race — see memcached.go's package
// doc. This test asserts the emulation doesn't corrupt/crash, not that every
// concurrent member survives.
func TestTagListRaceIsDocumentedNotFixed(t *testing.T) {
	ctx := context.Background()
	cache := newMemcachedCacheForTest(t)

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("race-key-%d", i)
			_ = cache.Set(ctx, key, []byte("v"), time.Minute, "race-tag")
		}(i)
	}
	wg.Wait()

	// InvalidateTag must not error even if some members were dropped by the
	// race — a best-effort count is acceptable, a crash or error is not.
	if _, err := cache.InvalidateTag(ctx, "race-tag"); err != nil {
		t.Fatalf("InvalidateTag after concurrent Set: %v", err)
	}
}
