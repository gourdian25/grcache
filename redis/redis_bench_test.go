// File: redis/redis_bench_test.go

package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gourdian25/grcache/conformance"
)

func BenchmarkGet(b *testing.B) {
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		b.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	if err := cache.Set(ctx, "bench-get-key", []byte("benchmark-value"), time.Hour); err != nil {
		b.Fatalf("Set: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cache.Get(ctx, "bench-get-key"); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkSet(b *testing.B) {
	ctx := context.Background()
	cache, err := newCache()
	if err != nil {
		b.Fatalf("newCache: %v", err)
	}
	defer cache.Close()

	val := []byte("benchmark-value")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-set-key-%d", i)
		if err := cache.Set(ctx, key, val, time.Hour); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkInvalidateTag(b *testing.B) {
	ctx := context.Background()

	for _, n := range []int{10, 1000, 100000} {
		b.Run(fmt.Sprintf("cardinality=%d", n), func(b *testing.B) {
			cache, err := newCache()
			if err != nil {
				b.Fatalf("newCache: %v", err)
			}
			defer cache.Close()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				tag := fmt.Sprintf("bench-tag-%d-%d", n, i)
				if err := conformance.PopulateTagged(ctx, cache, tag, n); err != nil {
					b.Fatalf("PopulateTagged: %v", err)
				}
				b.StartTimer()

				if _, err := cache.InvalidateTag(ctx, tag); err != nil {
					b.Fatalf("InvalidateTag: %v", err)
				}
			}
		})
	}
}
