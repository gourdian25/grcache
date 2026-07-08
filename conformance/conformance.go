// File: conformance/conformance.go

// Package conformance is a shared behavioral test suite run against every
// grcache backend via the common Cache interface. It enforces identical
// behavior across backends for every scenario it covers (see Run) and is
// the primary test artifact for each backend package, which supplies its
// own constructor to Run and adds backend-specific tests (e.g.
// connection-failure handling) separately. It is not an exhaustive proof
// of parity — new scenarios get added here as gaps are found.
//
// This package imports only the root grcache package, never a backend
// subpackage — each backend's own test file imports conformance sideways,
// which is what avoids an import cycle in the subpackage-per-backend layout.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gourdian25/grcache"
)

// PopulateTagged sets n keys, all tagged with tag, using a shared key prefix
// so callers can identify them. It is intended for benchmark setup (e.g.
// measuring InvalidateTag at 10/1k/100k-key cardinality) so the population
// step isn't duplicated across every backend's benchmark file.
//
// Parameters:
//   - ctx: context.Context
//   - cache: grcache.Cache — the backend under test
//   - tag: string — every populated key is tagged with this one tag
//   - n: int — how many keys to populate
//
// Returns:
//   - error: the first Set failure encountered, if any
//
// Example:
//
//	if err := conformance.PopulateTagged(ctx, cache, "bench-tag", 1000); err != nil {
//		b.Fatal(err)
//	}
//	n, err := cache.InvalidateTag(ctx, "bench-tag") // n == 1000
func PopulateTagged(ctx context.Context, cache grcache.Cache, tag string, n int) error {
	val := []byte("benchmark-value")
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s-key-%d", tag, i)
		if err := cache.Set(ctx, key, val, time.Hour, tag); err != nil {
			return fmt.Errorf("PopulateTagged: set %s: %w", key, err)
		}
	}
	return nil
}

// RunOption configures Run's behavior for scenarios where backends have
// documented, non-uniform guarantees. The zero value (no options) is the
// strictest behavior; options only ever relax specific assertions for a
// specific, documented reason — they never relax the default for every
// backend.
type RunOption func(*runConfig)

type runConfig struct {
	bestEffortTagConcurrency bool
}

// WithBestEffortTagConcurrency relaxes the ConcurrentTagSet scenario's
// assertion from "exactly N keys invalidated" to "at least one key
// invalidated, and never more than N" — for backends whose tag storage is
// documented as best-effort/eventually-consistent under concurrent writes
// (currently only grcache/memcached; see its package doc comment and
// TestTagListRaceIsDocumentedNotFixed). Backends with a real transactional
// or atomic tag-index write path (memory, redis, postgres, mongo) must not
// use this option — they are expected to meet the strict guarantee.
func WithBestEffortTagConcurrency() RunOption {
	return func(cfg *runConfig) { cfg.bestEffortTagConcurrency = true }
}

// Run executes the full conformance suite against a fresh Cache instance
// obtained from newCache for each scenario. Every backend's own test file
// calls this with its own constructor closure so the same behavioral
// assertions run identically against all five backends.
//
// Parameters:
//   - t: *testing.T
//   - newCache: func() (grcache.Cache, error) — called once per scenario to
//     get a fresh, isolated Cache instance
//   - opts: ...RunOption — normally omitted; see WithBestEffortTagConcurrency
//     for the one documented exception
//
// Example:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func() (grcache.Cache, error) {
//			return memory.NewMemoryCache()
//		})
//	}
func Run(t *testing.T, newCache func() (grcache.Cache, error), opts ...RunOption) {
	t.Helper()

	cfg := &runConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	t.Run("SetThenGet", func(t *testing.T) { testSetThenGet(t, newCache) })
	t.Run("GetMissing", func(t *testing.T) { testGetMissing(t, newCache) })
	t.Run("Expiry", func(t *testing.T) { testExpiry(t, newCache) })
	t.Run("NoExpiry", func(t *testing.T) { testNoExpiry(t, newCache) })
	t.Run("DeleteNonExistent", func(t *testing.T) { testDeleteNonExistent(t, newCache) })
	t.Run("Exists", func(t *testing.T) { testExists(t, newCache) })
	t.Run("TagInvalidation", func(t *testing.T) { testTagInvalidation(t, newCache) })
	t.Run("ConcurrentAccess", func(t *testing.T) { testConcurrentAccess(t, newCache) })
	t.Run("ConcurrentTagSet", func(t *testing.T) { testConcurrentTagSet(t, newCache, cfg.bestEffortTagConcurrency) })
	t.Run("StatsSanity", func(t *testing.T) { testStatsSanity(t, newCache) })
	t.Run("PostClose", func(t *testing.T) { testPostClose(t, newCache) })
}

func testSetThenGet(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	want := []byte("hello world")
	if err := cache.Set(ctx, "k1", want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := cache.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Get returned %q, want %q", got, want)
	}
}

func testGetMissing(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	_, err = cache.Get(ctx, "does-not-exist")
	if !errors.Is(err, grcache.ErrKeyNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrKeyNotFound", err)
	}
}

func testExpiry(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	// ttl/sleep are deliberately >= 1 second: memcached only supports
	// second-granularity expiry (see grcache/memcached's expirationSeconds),
	// so a shorter ttl here would pass on finer-grained backends but be
	// meaningless on memcached.
	if err := cache.Set(ctx, "expiring", []byte("v"), 1100*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if _, err := cache.Get(ctx, "expiring"); err != nil {
		t.Fatalf("Get (before expiry): %v", err)
	}

	time.Sleep(1400 * time.Millisecond)

	if _, err := cache.Get(ctx, "expiring"); !errors.Is(err, grcache.ErrKeyNotFound) {
		t.Fatalf("Get (after expiry) error = %v, want ErrKeyNotFound", err)
	}

	if ok, err := cache.Exists(ctx, "expiring"); err != nil || ok {
		t.Fatalf("Exists (after expiry) = (%v, %v), want (false, nil)", ok, err)
	}
}

func testNoExpiry(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	if err := cache.Set(ctx, "forever", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if _, err := cache.Get(ctx, "forever"); err != nil {
		t.Fatalf("Get (ttl=0 entry): %v", err)
	}
}

func testDeleteNonExistent(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	if err := cache.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("Delete(non-existent) = %v, want nil", err)
	}
}

func testExists(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	if ok, err := cache.Exists(ctx, "not-set"); err != nil || ok {
		t.Fatalf("Exists(not-set) = (%v, %v), want (false, nil)", ok, err)
	}

	if err := cache.Set(ctx, "is-set", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if ok, err := cache.Exists(ctx, "is-set"); err != nil || !ok {
		t.Fatalf("Exists(is-set) = (%v, %v), want (true, nil)", ok, err)
	}

	if err := cache.Delete(ctx, "is-set"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if ok, err := cache.Exists(ctx, "is-set"); err != nil || ok {
		t.Fatalf("Exists(deleted) = (%v, %v), want (false, nil)", ok, err)
	}
}

func testTagInvalidation(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()

	t.Run("OneTagOneKey", func(t *testing.T) {
		ctx := context.Background()
		cache, err := newCache()
		if err != nil {
			t.Fatalf("newCache: %v", err)
		}
		defer cache.Close()

		if err := cache.Set(ctx, "k1", []byte("v1"), time.Minute, "tagA"); err != nil {
			t.Fatalf("Set: %v", err)
		}

		n, err := cache.InvalidateTag(ctx, "tagA")
		if err != nil {
			t.Fatalf("InvalidateTag: %v", err)
		}
		if n != 1 {
			t.Fatalf("InvalidateTag returned %d, want 1", n)
		}

		if _, err := cache.Get(ctx, "k1"); !errors.Is(err, grcache.ErrKeyNotFound) {
			t.Fatalf("Get(k1) after invalidate error = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("OneTagManyKeys", func(t *testing.T) {
		ctx := context.Background()
		cache, err := newCache()
		if err != nil {
			t.Fatalf("newCache: %v", err)
		}
		defer cache.Close()

		keys := []string{"m1", "m2", "m3", "m4", "m5"}
		for _, k := range keys {
			if err := cache.Set(ctx, k, []byte("v"), time.Minute, "shared"); err != nil {
				t.Fatalf("Set(%s): %v", k, err)
			}
		}

		n, err := cache.InvalidateTag(ctx, "shared")
		if err != nil {
			t.Fatalf("InvalidateTag: %v", err)
		}
		if n != len(keys) {
			t.Fatalf("InvalidateTag returned %d, want %d", n, len(keys))
		}

		for _, k := range keys {
			if _, err := cache.Get(ctx, k); !errors.Is(err, grcache.ErrKeyNotFound) {
				t.Fatalf("Get(%s) after invalidate error = %v, want ErrKeyNotFound", k, err)
			}
		}
	})

	t.Run("MultipleTagsOnOneKey", func(t *testing.T) {
		ctx := context.Background()
		cache, err := newCache()
		if err != nil {
			t.Fatalf("newCache: %v", err)
		}
		defer cache.Close()

		if err := cache.Set(ctx, "multi", []byte("v"), time.Minute, "tagX", "tagY"); err != nil {
			t.Fatalf("Set: %v", err)
		}

		n, err := cache.InvalidateTag(ctx, "tagX")
		if err != nil {
			t.Fatalf("InvalidateTag(tagX): %v", err)
		}
		if n != 1 {
			t.Fatalf("InvalidateTag(tagX) returned %d, want 1", n)
		}

		if _, err := cache.Get(ctx, "multi"); !errors.Is(err, grcache.ErrKeyNotFound) {
			t.Fatalf("Get(multi) after invalidating tagX error = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("InvalidateThenReSet", func(t *testing.T) {
		ctx := context.Background()
		cache, err := newCache()
		if err != nil {
			t.Fatalf("newCache: %v", err)
		}
		defer cache.Close()

		if err := cache.Set(ctx, "reset-key", []byte("v1"), time.Minute, "tagZ"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := cache.InvalidateTag(ctx, "tagZ"); err != nil {
			t.Fatalf("InvalidateTag: %v", err)
		}

		if err := cache.Set(ctx, "reset-key", []byte("v2"), time.Minute, "tagZ"); err != nil {
			t.Fatalf("Set (re-set): %v", err)
		}

		got, err := cache.Get(ctx, "reset-key")
		if err != nil {
			t.Fatalf("Get (after re-set): %v", err)
		}
		if string(got) != "v2" {
			t.Fatalf("Get (after re-set) = %q, want %q", got, "v2")
		}

		n, err := cache.InvalidateTag(ctx, "tagZ")
		if err != nil {
			t.Fatalf("InvalidateTag (second time): %v", err)
		}
		if n != 1 {
			t.Fatalf("InvalidateTag (second time) returned %d, want 1", n)
		}
	})

	t.Run("InvalidateUnknownTag", func(t *testing.T) {
		ctx := context.Background()
		cache, err := newCache()
		if err != nil {
			t.Fatalf("newCache: %v", err)
		}
		defer cache.Close()

		n, err := cache.InvalidateTag(ctx, "never-used-tag")
		if err != nil {
			t.Fatalf("InvalidateTag(unknown): %v", err)
		}
		if n != 0 {
			t.Fatalf("InvalidateTag(unknown) returned %d, want 0", n)
		}
	})
}

func testConcurrentAccess(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	const workers = 20
	const opsPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				key := "concurrent-key"
				_ = cache.Set(ctx, key, []byte("v"), time.Minute, "concurrent-tag")
				_, _ = cache.Get(ctx, key)
				_, _ = cache.Exists(ctx, key)
				if i%10 == 0 {
					_, _ = cache.InvalidateTag(ctx, "concurrent-tag")
				}
				_ = cache.Delete(ctx, key)
			}
		}(w)
	}
	wg.Wait()

	if _, err := cache.Stats(ctx); err != nil {
		t.Fatalf("Stats after concurrent access: %v", err)
	}
}

// testConcurrentTagSet concurrently Sets N distinct keys all tagged with one
// shared tag, then calls InvalidateTag once and asserts on the result.
// Unlike testConcurrentAccess (which hammers a single shared key and
// discards every error), this scenario is designed to actually prove tag-index
// correctness under concurrency: backends with a real transactional or
// atomic tag-index write path must invalidate exactly N keys; only the one
// documented best-effort backend (memcached) is allowed a weaker bound.
func testConcurrentTagSet(t *testing.T, newCache func() (grcache.Cache, error), bestEffort bool) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	const n = 50
	const tag = "concurrent-tag-set-tag"

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-tag-set-key-%d", i)
			errs[i] = cache.Set(ctx, key, []byte("v"), time.Hour, tag)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Set(concurrent-tag-set-key-%d): %v", i, err)
		}
	}

	removed, err := cache.InvalidateTag(ctx, tag)
	if err != nil {
		t.Fatalf("InvalidateTag: %v", err)
	}

	if bestEffort {
		if removed <= 0 || removed > n {
			t.Fatalf("InvalidateTag (best-effort) = %d, want 0 < n <= %d", removed, n)
		}
		return
	}

	if removed != n {
		t.Fatalf("InvalidateTag = %d, want exactly %d (this backend is expected to have a strict, non-best-effort tag index)", removed, n)
	}
}

func testStatsSanity(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	before, err := cache.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats (initial): %v", err)
	}

	if err := cache.Set(ctx, "stat-key", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := cache.Get(ctx, "stat-key"); err != nil {
		t.Fatalf("Get (hit): %v", err)
	}
	if _, err := cache.Get(ctx, "stat-key-missing"); !errors.Is(err, grcache.ErrKeyNotFound) {
		t.Fatalf("Get (miss) error = %v, want ErrKeyNotFound", err)
	}

	after, err := cache.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats (after): %v", err)
	}

	if after.Hits < before.Hits {
		t.Fatalf("Stats.Hits decreased: before=%d after=%d", before.Hits, after.Hits)
	}
	if after.Misses < before.Misses {
		t.Fatalf("Stats.Misses decreased: before=%d after=%d", before.Misses, after.Misses)
	}
	if after.Hits == before.Hits {
		t.Fatalf("Stats.Hits did not increase after a hit")
	}
	if after.Misses == before.Misses {
		t.Fatalf("Stats.Misses did not increase after a miss")
	}
}

func testPostClose(t *testing.T, newCache func() (grcache.Cache, error)) {
	t.Helper()
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := cache.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil (idempotent)", err)
	}

	if _, err := cache.Get(ctx, "anything"); !errors.Is(err, grcache.ErrClosed) {
		t.Fatalf("Get after Close error = %v, want ErrClosed", err)
	}

	if err := cache.Set(ctx, "anything", []byte("v"), time.Minute); !errors.Is(err, grcache.ErrClosed) {
		t.Fatalf("Set after Close error = %v, want ErrClosed", err)
	}
}
