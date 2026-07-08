// File: memory/memory_test.go

package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gourdian25/grcache"
	"github.com/gourdian25/grcache/conformance"
	"github.com/gourdian25/grcache/memory"
)

func newCache() (grcache.Cache, error) {
	return memory.NewMemoryCache()
}

func TestConformance(t *testing.T) {
	conformance.Run(t, newCache)
}

func TestInvalidTTL(t *testing.T) {
	ctx := context.Background()
	cache, err := memory.NewMemoryCache()
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer cache.Close()

	if err := cache.Set(ctx, "k", []byte("v"), -time.Second); !errors.Is(err, grcache.ErrInvalidTTL) {
		t.Fatalf("Set with negative ttl error = %v, want ErrInvalidTTL", err)
	}
}

func TestSweepReclaimsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	cache, err := memory.NewMemoryCache(memory.WithSweepInterval(30 * time.Millisecond))
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer cache.Close()

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

func TestWithLogger(t *testing.T) {
	logger := &conformance.RecordingLogger{}
	cache, err := memory.NewMemoryCache(memory.WithSweepInterval(20*time.Millisecond), memory.WithLogger(logger))
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}

	ctx := context.Background()
	if err := cache.Set(ctx, "logged-key", []byte("v"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && logger.Total() == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.Total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (sweep and/or close)")
	}
}

func TestConcurrentCloseIsSafe(t *testing.T) {
	cache, err := memory.NewMemoryCache()
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
