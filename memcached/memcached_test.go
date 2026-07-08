// File: memcached/memcached_test.go

package memcached_test

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
	"github.com/gourdian25/grcache/conformance"
	"github.com/gourdian25/grcache/memcached"
)

const testAddr = "localhost:11211"

func flushTestServer(t *testing.T) {
	t.Helper()
	client := gomemcache.New(testAddr)
	defer client.Close()
	if err := client.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
}

func newCache() (grcache.Cache, error) {
	return memcached.NewMemcachedCache(memcached.MemcachedConfig{Servers: []string{testAddr}})
}

func newCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	flushTestServer(t)
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	t.Cleanup(func() {
		cache.Close()
		flushTestServer(t)
	})
	return cache
}

func TestConformance(t *testing.T) {
	flushTestServer(t)
	conformance.Run(t, newCache)
	flushTestServer(t)
}

func TestNewMemcachedCache_MissingServers(t *testing.T) {
	if _, err := memcached.NewMemcachedCache(memcached.MemcachedConfig{}); err == nil {
		t.Fatal("NewMemcachedCache with no Servers = nil error, want error")
	}
}

func TestNewMemcachedCache_Unreachable(t *testing.T) {
	_, err := memcached.NewMemcachedCache(memcached.MemcachedConfig{
		Servers: []string{"localhost:1"}, // nothing listens here
		Timeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("NewMemcachedCache with unreachable server = nil error, want error")
	}
}

// TestTagListRaceIsDocumentedNotFixed exercises concurrent Set calls tagging
// the same tag and documents (rather than "fixes") that the list-based
// emulation can lose a member under a race — see the package doc comment.
// This test asserts the emulation doesn't corrupt/crash, not that every
// concurrent member survives.
func TestWithLogger(t *testing.T) {
	logger := &conformance.RecordingLogger{}
	flushTestServer(t)
	cache, err := memcached.NewMemcachedCache(memcached.MemcachedConfig{
		Servers: []string{testAddr},
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewMemcachedCache: %v", err)
	}
	defer flushTestServer(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.Total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

func TestPostCloseAllMethods(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := cache.Delete(ctx, "anything"); err != grcache.ErrClosed {
		t.Fatalf("Delete after Close error = %v, want ErrClosed", err)
	}
	if _, err := cache.Exists(ctx, "anything"); err != grcache.ErrClosed {
		t.Fatalf("Exists after Close error = %v, want ErrClosed", err)
	}
	if _, err := cache.InvalidateTag(ctx, "anything"); err != grcache.ErrClosed {
		t.Fatalf("InvalidateTag after Close error = %v, want ErrClosed", err)
	}
	if _, err := cache.Stats(ctx); err != grcache.ErrClosed {
		t.Fatalf("Stats after Close error = %v, want ErrClosed", err)
	}
}

func TestTagListRaceIsDocumentedNotFixed(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

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
