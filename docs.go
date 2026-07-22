// File: docs.go

// Package grcache provides a generic, backend-agnostic caching abstraction
// for the gourdian ecosystem.
//
// Overview:
// grcache is the same architectural pattern gourdiantoken uses for token
// storage — a small interface (Cache) implemented by multiple interchangeable
// backends — applied to general-purpose caching instead. It is the shared
// caching layer grauth (permission/session caching), graudit (read-path
// caching), and gourdianerp (application-level caching) depend on.
//
// Key Features:
//   - A single Cache interface: Get, Set, Delete, Exists, InvalidateTag,
//     Stats, Close — implemented identically by all five backends
//   - Per-key TTL on every Set call, with ttl=0 meaning "no expiry"
//   - Tag-based bulk invalidation: associate a key with one or more tags at
//     Set time, then remove every key under a tag with one InvalidateTag call
//   - A Stats() snapshot (hits, misses, evictions, key count, avg latency)
//   - Sentinel errors (ErrKeyNotFound, ErrCacheUnavailable, ErrInvalidTTL,
//     ErrClosed) usable with errors.Is
//   - Optional diagnostic logging via any logger satisfying the small
//     Logger interface — grlog.Logger works with no adapter
//
// Getting Started:
//
//	import (
//		"context"
//		"log"
//		"time"
//
//		"github.com/gourdian25/grcache"
//	)
//
//	func main() {
//		cache, err := grcache.NewMemoryCache()
//		if err != nil {
//			log.Fatal(err)
//		}
//		defer cache.Close()
//
//		ctx := context.Background()
//		if err := cache.Set(ctx, "user:42", []byte("alice"), time.Minute, "tenant:acme"); err != nil {
//			log.Fatal(err)
//		}
//
//		val, err := cache.Get(ctx, "user:42")
//		if err != nil {
//			log.Fatal(err)
//		}
//		log.Println(string(val)) // "alice"
//	}
//
// Backends:
//
// grcache is a flat, single package — every backend's constructor and
// Config type live directly in the grcache package (no subpackages to
// import selectively). This trades away per-backend dependency isolation
// for consistency with the rest of the gourdian ecosystem's flat-package
// convention; see docs/architecture.md for the full rationale.
//
//  1. In-memory (NewMemoryCache) — default, zero external dependency, for
//     local development and testing. A background goroutine sweeps expired
//     entries; Get/Exists also check expiry lazily as a correctness backstop.
//
//     cache, err := grcache.NewMemoryCache(grcache.WithSweepInterval(30 * time.Second))
//
//  2. Redis (NewRedisCache) — primary production backend. Tags are stored as
//     Redis Sets; InvalidateTag pipelines SMEMBERS + DEL.
//
//     cache, err := grcache.NewRedisCache(grcache.RedisConfig{Addr: "localhost:6379"})
//
//  3. Memcached (NewMemcachedCache) — secondary backend. Tags are emulated
//     via a serialized member list, which is best-effort/eventually
//     consistent under concurrent writes (see memcached.go's package
//     doc for the exact tradeoff).
//
//     cache, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{Servers: []string{"localhost:11211"}})
//
//  4. PostgreSQL (NewPostgresCache) — via pgx/v5 + sqlc-generated queries
//     (no ORM). Tags live in a separate join table kept in sync with the
//     entries table on every Set/Delete. Intended for test/dev/CI
//     environments with Postgres already available but no Redis/memcached;
//     prefer Redis in production.
//
//     cache, err := grcache.NewPostgresCache(grcache.PostgresConfig{DSN: dsn})
//
//  5. MongoDB (NewMongoCache) — tags live directly on the document as an
//     array field. A TTL index (expireAfterSeconds: 0) gives native,
//     database-managed expiry, the same as Redis's EX. Same intended use
//     case as PostgreSQL above: test/dev/CI without Redis/memcached.
//
//     cache, err := grcache.NewMongoCache(grcache.MongoConfig{URI: uri, Database: "myapp"})
//
// Dragonfly and Valkey are Redis-protocol compatible — point RedisConfig.Addr
// at either instead of building a separate backend.
//
// Tag-Based Invalidation:
//
//	cache.Set(ctx, "session:abc", data, time.Hour, "user:42", "tenant:acme")
//	cache.Set(ctx, "session:def", data, time.Hour, "user:42")
//	n, err := cache.InvalidateTag(ctx, "user:42") // removes both sessions, n == 2
//
// TTL Semantics:
//
// A ttl of 0 passed to Set means "no expiry" on every backend. Redis stores
// no EX flag; Mongo omits the expiresAt field entirely so its TTL index never
// touches the document; the memory and postgres backends simply never sweep
// it. A negative ttl returns ErrInvalidTTL. Expiry is always checked lazily
// on Get/Exists in addition to whatever background mechanism a backend uses
// (sweep goroutine, native TTL), so a not-yet-reaped expired entry is never
// visible to a caller.
//
// Error Handling:
//
//	val, err := cache.Get(ctx, key)
//	if errors.Is(err, grcache.ErrKeyNotFound) {
//		// cache miss — expected control flow, not a failure
//	} else if err != nil {
//		// backend unavailable or another real failure
//	}
//
// Backend-native errors (redis.Nil, pgx.ErrNoRows,
// mongo.ErrNoDocuments, memcache.ErrCacheMiss) are always translated into a
// grcache sentinel before being wrapped — a caller using only errors.Is
// against grcache's own sentinels never needs to know which backend is
// underneath.
//
// Optional Logging:
//
// Every backend accepts an optional Logger for diagnostic messages
// (connection failures, sweep-cycle summaries, shutdown). A nil Logger (the
// default) means grcache logs nothing.
//
//	import "github.com/gourdian25/grlog"
//
//	logger := grlog.NewDefaultLogger()
//	cache, err := redis.NewRedisCache(redis.RedisConfig{
//		Addr:   "localhost:6379",
//		Logger: logger,
//	})
//
// Architecture:
//
// grcache is one flat package: cache.go/errors.go/logger.go hold the Cache
// interface, Stats, sentinel errors, and the Logger interface (stdlib
// only), and each backend's Config type, constructor, and concrete
// (unexported) implementation live in their own file (memory.go, redis.go,
// memcached.go, postgres.go, mongo.go). A shared contract test suite
// (contract_cache_test.go, run via TestCache_Contract's per-backend
// subtests) runs one behavioral test against all five backends through the
// Cache interface, enforcing identical behavior for every scenario it
// covers. It is not an exhaustive proof of parity — see that file for the
// current scenario list and each backend's own *_test.go file for anything
// scenario-specific it adds on top.
//
// Testing:
//
// grcache's own tests run against real local Redis/Postgres/Mongo/memcached
// instances — no mocks, no miniredis, no testcontainers-go — mirroring
// gourdiantoken's testing philosophy. See CLAUDE.md and README.md for the
// exact connection settings and docker commands to stand up each service.
//
// Performance:
//
// Run `make bench` for current numbers. InvalidateTag is benchmarked at
// 10/1k/100k-key tag cardinality per backend; memcached's list-based tag
// emulation is the one backend with a documented O(n²) scaling cliff at
// high cardinality, a direct consequence of its best-effort tag model, not
// an implementation oversight.
//
// Best Practices:
//   - Always `defer cache.Close()` — it stops background goroutines
//     (memory, postgres) and closes underlying connections (redis, mongo,
//     memcached)
//   - Treat ErrKeyNotFound as expected control flow, not an error to log
//   - Use tags to group related keys (e.g. all data for one user or tenant)
//     rather than tracking key lists yourself
//   - Don't rely on the in-memory backend for state shared across processes
//     or replicas — use Redis, Postgres, or Mongo for that
//
// Out of Scope:
//
// grcache is explicitly not a general-purpose data store: there is no query
// language, no filtering by value, no secondary indexes, and no range scans.
// It does not attempt distributed cache coherence — multiple in-memory
// backend instances across processes are expected to diverge; use a shared
// backend (Redis, Postgres, Mongo) for anything requiring shared state. It
// is not a durability layer: a cache miss must never be treated as data
// loss by callers, and there is no write-through/write-behind persistence.
// Cache warming/preloading orchestration is a consumer-side concern.
// Stats() is a snapshot only; Prometheus/OpenTelemetry export is a separate
// adapter-package concern, not part of this package.
//
// License: MIT
// Repository: https://github.com/gourdian25/grcache
package grcache
