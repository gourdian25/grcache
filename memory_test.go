// File: memory_test.go

package grcache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gourdian25/grcache"
)

func TestMemorySweepReclaimsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	cache, err := grcache.NewMemoryCache(grcache.WithSweepInterval(30 * time.Millisecond))
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer func() { _ = cache.Close() }()

	if err := cache.Set(ctx, "sweep-me", []byte("v"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := cache.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.KeyCount == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("sweep did not reclaim expired entry within deadline")
}

func TestMemoryWithLogger(t *testing.T) {
	logger := &recordingLogger{}
	cache, err := grcache.NewMemoryCache(grcache.WithSweepInterval(20*time.Millisecond), grcache.WithLogger(logger))
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}

	ctx := context.Background()
	if err := cache.Set(ctx, "logged-key", []byte("v"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && logger.total() == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (sweep and/or close)")
	}
}

func TestMemoryConcurrentCloseIsSafe(t *testing.T) {
	cache, err := grcache.NewMemoryCache()
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}

	const closers = 10
	var wg sync.WaitGroup
	wg.Add(closers)
	for i := 0; i < closers; i++ {
		go func() {
			defer wg.Done()
			if err := cache.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
}
