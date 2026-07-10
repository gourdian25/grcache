// File: example/example.go

// Command example is a standalone runnable demonstration of every grcache
// backend. It is package main, in the same module as grcache itself, and
// is not part of the library's test surface — mirrors gourdiantoken's own
// example/example.go convention.
//
// The memory backend always runs (no external service required). The four
// networked backends (Redis, memcached, PostgreSQL, MongoDB) each attempt
// to connect using this repo's documented local dev settings (see
// CLAUDE.md) and print "skipping <backend>: <err>" instead of failing if
// the corresponding service isn't running, so this program always completes
// standalone.
//
// Run with: go run ./example
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gourdian25/grlog"

	"github.com/gourdian25/grcache"
	"github.com/gourdian25/grcache/memcached"
	"github.com/gourdian25/grcache/memory"
	"github.com/gourdian25/grcache/mongostore"
	"github.com/gourdian25/grcache/postgres"
	"github.com/gourdian25/grcache/redis"
)

func main() {
	ctx := context.Background()

	fmt.Println("=== grcache example ===")

	demoMemory(ctx)
	demoMemoryWithLogging(ctx)
	demoRedis(ctx)
	demoMemcached(ctx)
	demoPostgres(ctx)
	demoMongo(ctx)

	fmt.Println("\n=== done ===")
}

// walkthrough exercises the full Cache interface against cache, printing
// each step's result. Every backend demo below calls this once it has a
// working Cache, so the same behavior is demonstrated identically
// regardless of which backend produced it.
func walkthrough(ctx context.Context, name string, cache grcache.Cache) {
	defer cache.Close()

	key := "user:42"
	val := []byte("alice")

	if err := cache.Set(ctx, key, val, time.Minute, "tenant:acme"); err != nil {
		fmt.Printf("[%s] Set failed: %v\n", name, err)
		return
	}
	fmt.Printf("[%s] Set %q = %q (tag: tenant:acme)\n", name, key, val)

	got, err := cache.Get(ctx, key)
	if err != nil {
		fmt.Printf("[%s] Get failed: %v\n", name, err)
		return
	}
	fmt.Printf("[%s] Get %q -> %q\n", name, key, got)

	exists, err := cache.Exists(ctx, key)
	if err != nil {
		fmt.Printf("[%s] Exists failed: %v\n", name, err)
		return
	}
	fmt.Printf("[%s] Exists %q -> %v\n", name, key, exists)

	n, err := cache.InvalidateTag(ctx, "tenant:acme")
	if err != nil {
		fmt.Printf("[%s] InvalidateTag failed: %v\n", name, err)
		return
	}
	fmt.Printf("[%s] InvalidateTag(tenant:acme) removed %d key(s)\n", name, n)

	stats, err := cache.Stats(ctx)
	if err != nil {
		fmt.Printf("[%s] Stats failed: %v\n", name, err)
		return
	}
	fmt.Printf("[%s] Stats: hits=%d misses=%d evictions=%d keyCount=%d\n",
		name, stats.Hits, stats.Misses, stats.Evictions, stats.KeyCount)
}

func demoMemory(ctx context.Context) {
	fmt.Println("\n--- memory (always available, zero dependencies) ---")
	cache, err := memory.NewMemoryCache()
	if err != nil {
		fmt.Printf("skipping memory: %v\n", err)
		return
	}
	walkthrough(ctx, "memory", cache)
}

// demoMemoryWithLogging shows grlog interoperability: *grlog.Logger
// satisfies grcache.Logger with no adapter needed.
func demoMemoryWithLogging(ctx context.Context) {
	fmt.Println("\n--- memory with grlog logging ---")
	logger := grlog.NewDefaultLogger()
	cache, err := memory.NewMemoryCache(
		memory.WithLogger(logger),
		memory.WithSweepInterval(5*time.Second),
	)
	if err != nil {
		fmt.Printf("skipping memory-with-logging: %v\n", err)
		return
	}
	walkthrough(ctx, "memory+grlog", cache)
}

func demoRedis(ctx context.Context) {
	fmt.Println("\n--- redis ---")
	cache, err := redis.NewRedisCache(redis.RedisConfig{
		Addr:     "localhost:6379",
		Password: "redis_password",
		DB:       14,
	})
	if err != nil {
		fmt.Printf("skipping redis: %v\n", err)
		return
	}
	walkthrough(ctx, "redis", cache)
}

func demoMemcached(ctx context.Context) {
	fmt.Println("\n--- memcached ---")
	cache, err := memcached.NewMemcachedCache(memcached.MemcachedConfig{
		Servers: []string{"localhost:11211"},
	})
	if err != nil {
		fmt.Printf("skipping memcached: %v\n", err)
		return
	}
	walkthrough(ctx, "memcached", cache)
}

func demoPostgres(ctx context.Context) {
	fmt.Println("\n--- postgres ---")
	cache, err := postgres.NewPostgresCache(postgres.PostgresConfig{
		DSN: "host=localhost user=postgres_user password=postgres_password dbname=grcache_test port=5432 sslmode=disable",
	})
	if err != nil {
		fmt.Printf("skipping postgres: %v\n", err)
		return
	}
	walkthrough(ctx, "postgres", cache)
}

func demoMongo(ctx context.Context) {
	fmt.Println("\n--- mongo ---")
	cache, err := mongostore.NewMongoCache(mongostore.MongoConfig{
		URI:      "mongodb://root:mongo_password@localhost:27018/?directConnection=true",
		Database: "grcache_test",
	})
	if err != nil {
		fmt.Printf("skipping mongo: %v\n", err)
		return
	}
	walkthrough(ctx, "mongo", cache)
}
