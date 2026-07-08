// File: cache.go

// Package grcache is a generic, backend-agnostic caching abstraction for the
// gourdian ecosystem. It defines the Cache interface that every backend
// (grcache/memory, grcache/redis, grcache/memcached, grcache/postgres,
// grcache/mongo) implements, plus the sentinel errors and Stats type shared
// across all of them.
//
// grcache is explicitly not a general-purpose data store: there is no query
// language, no filtering by value, no secondary indexes, and no range scans.
// It does not attempt distributed cache coherence — multiple in-memory
// backend instances across processes are expected to diverge; use a shared
// backend (Redis, Postgres, Mongo) for anything requiring shared state. It is
// not a durability layer: a cache miss must never be treated as data loss by
// callers, and there is no write-through/write-behind persistence. Cache
// warming/preloading orchestration is a consumer-side concern. v1 exposes a
// Stats snapshot only; Prometheus/OpenTelemetry export is a separate,
// consumer-side or adapter-package concern.
package grcache

import (
	"context"
	"time"
)

// Cache is the primary interface all backends implement.
type Cache interface {
	// Get retrieves the raw bytes stored at key.
	// Returns ErrKeyNotFound if the key does not exist or has expired.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores val at key with the given ttl. A ttl of 0 means "no expiry"
	// (backend-dependent: in-memory never expires it, Redis uses no EX flag).
	// tags is optional (nil or empty is valid) and associates key with one or
	// more tags for later bulk invalidation via InvalidateTag.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error

	// Delete removes key. Deleting a non-existent key is not an error.
	Delete(ctx context.Context, key string) error

	// Exists reports whether key is present and not expired.
	Exists(ctx context.Context, key string) (bool, error)

	// InvalidateTag deletes every key currently associated with tag.
	// Returns the number of keys removed.
	InvalidateTag(ctx context.Context, tag string) (int, error)

	// Stats returns a snapshot of hit/miss/eviction counters and basic
	// latency info since the cache was created or last reset.
	Stats(ctx context.Context) (Stats, error)

	// Close releases any underlying connections/resources. After Close,
	// every other method returns ErrClosed.
	Close() error
}

// Stats is a snapshot of cache counters and latency, returned by Cache.Stats.
type Stats struct {
	Hits       uint64
	Misses     uint64
	Evictions  uint64
	KeyCount   int64         // -1 if backend cannot report this cheaply (e.g. Redis without SCAN)
	AvgLatency time.Duration
}
