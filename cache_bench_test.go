// File: cache_bench_test.go

package grcache_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gourdian25/grcache"
)

// benchCaches enumerates every backend a benchmark subtest should run
// against, in the same shape as the ecosystem's own
// getTestRepositoryFactories() convention (see gourdiantoken) — a fresh
// Cache per subtest, skipping gracefully if the backend's live service
// isn't reachable.
func benchCaches(b *testing.B) map[string]func() (grcache.Cache, error) {
	b.Helper()
	return map[string]func() (grcache.Cache, error){
		"Memory":    func() (grcache.Cache, error) { return grcache.NewMemoryCache() },
		"Redis":     newRedisCache,
		"Memcached": newMemcachedCache,
		"Postgres":  newPostgresCache,
		"Mongo":     newMongoCache,
	}
}

func newBenchCache(b *testing.B, name string, factory func() (grcache.Cache, error)) grcache.Cache {
	b.Helper()
	cache, err := factory()
	if err != nil {
		b.Skipf("%s not available, skipping: %v", name, err)
	}
	return cache
}

func BenchmarkGet(b *testing.B) {
	ctx := context.Background()
	for name, factory := range benchCaches(b) {
		b.Run(name, func(b *testing.B) {
			cache := newBenchCache(b, name, factory)
			defer func() { _ = cache.Close() }()

			if err := cache.Set(ctx, "bench-get-key", []byte("benchmark-value"), time.Hour); err != nil {
				b.Fatalf("Set: %v", err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := cache.Get(ctx, "bench-get-key"); err != nil {
					b.Fatalf("Get: %v", err)
				}
			}
		})
	}
}

func BenchmarkSet(b *testing.B) {
	ctx := context.Background()
	for name, factory := range benchCaches(b) {
		b.Run(name, func(b *testing.B) {
			cache := newBenchCache(b, name, factory)
			defer func() { _ = cache.Close() }()

			val := []byte("benchmark-value")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("bench-set-key-%d", i)
				if err := cache.Set(ctx, key, val, time.Hour); err != nil {
					b.Fatalf("Set: %v", err)
				}
			}
		})
	}
}

// BenchmarkInvalidateTag caps memcached's cardinality tiers at 1000, not the
// 100000 used by every other backend. memcached's tag emulation (see
// memcached.go's package doc) does a read-modify-write of the entire member
// list on every tagged Set, so populating a single tag with n keys costs
// O(n^2) total data movement (each Set rewrites an ever-growing list). At
// n=100000 that makes a single benchmark population step impractically slow
// to include in a routine `make bench` run. This is not a gap in the
// measurement — it's the direct, expected consequence of the documented
// eventual-consistency tradeoff, and the 1000-key tier already demonstrates
// the scaling cliff clearly relative to Redis/Mongo/Postgres/memory's
// near-flat curves at the same cardinality.
func BenchmarkInvalidateTag(b *testing.B) {
	ctx := context.Background()
	cardinalities := map[string][]int{
		"Memory":    {10, 1000, 100000},
		"Redis":     {10, 1000, 100000},
		"Memcached": {10, 100, 1000},
		"Postgres":  {10, 1000, 100000},
		"Mongo":     {10, 1000, 100000},
	}

	for name, factory := range benchCaches(b) {
		b.Run(name, func(b *testing.B) {
			for _, n := range cardinalities[name] {
				b.Run(fmt.Sprintf("cardinality=%d", n), func(b *testing.B) {
					cache := newBenchCache(b, name, factory)
					defer func() { _ = cache.Close() }()

					for i := 0; i < b.N; i++ {
						b.StopTimer()
						tag := fmt.Sprintf("bench-tag-%d-%d", n, i)
						if err := populateTagged(ctx, cache, tag, n); err != nil {
							b.Fatalf("populateTagged: %v", err)
						}
						b.StartTimer()

						if _, err := cache.InvalidateTag(ctx, tag); err != nil {
							b.Fatalf("InvalidateTag: %v", err)
						}
					}
				})
			}
		})
	}
}
