// File: cache.go

package grcache

import (
	"context"
	"time"
)

// Cache is the primary interface all backends implement. Every backend
// subpackage (grcache/memory, grcache/redis, grcache/memcached,
// grcache/postgres, grcache/mongo) provides a New<Backend>Cache constructor
// returning a Cache, so application code can depend on this interface alone
// and swap backends without changing call sites.
type Cache interface {
	// Get retrieves the raw bytes stored at key.
	//
	// Parameters:
	//   - ctx: context.Context — propagated to the backend's network/DB call
	//   - key: string — the cache key to look up
	//
	// Returns:
	//   - []byte: a copy of the stored value; safe for the caller to mutate
	//   - error: ErrKeyNotFound if key does not exist or has expired,
	//     ErrCacheUnavailable if the backend could not be reached,
	//     ErrClosed if called after Close
	//
	// Example:
	//
	//	val, err := cache.Get(ctx, "user:42")
	//	if errors.Is(err, grcache.ErrKeyNotFound) {
	//		// cache miss — expected control flow, not a failure
	//	} else if err != nil {
	//		return err
	//	}
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores val at key with the given ttl, optionally associating it
	// with one or more tags for later bulk removal via InvalidateTag.
	//
	// Parameters:
	//   - ctx: context.Context
	//   - key: string — the cache key
	//   - val: []byte — the value to store; Set copies it, so the caller's
	//     slice may be reused/mutated afterward
	//   - ttl: time.Duration — 0 means "no expiry" on every backend (Redis
	//     stores no EX flag, Mongo omits the TTL-indexed field, memory/
	//     postgres simply never sweep it); negative returns ErrInvalidTTL
	//   - tags: ...string — optional (nil/empty is valid); each tag can
	//     later be passed to InvalidateTag to remove every key under it
	//
	// Returns:
	//   - error: ErrInvalidTTL for a negative ttl, ErrCacheUnavailable on a
	//     backend failure, ErrClosed if called after Close
	//
	// Example:
	//
	//	err := cache.Set(ctx, "session:abc", data, time.Hour, "user:42", "tenant:acme")
	Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error

	// Delete removes key. Deleting a non-existent key is not an error.
	//
	// Parameters:
	//   - ctx: context.Context
	//   - key: string — the cache key to remove
	//
	// Returns:
	//   - error: ErrCacheUnavailable on a backend failure, ErrClosed if
	//     called after Close; never an error for a key that doesn't exist
	Delete(ctx context.Context, key string) error

	// Exists reports whether key is present and not expired.
	//
	// Parameters:
	//   - ctx: context.Context
	//   - key: string — the cache key to check
	//
	// Returns:
	//   - bool: true if key exists and has not expired
	//   - error: ErrCacheUnavailable on a backend failure, ErrClosed if
	//     called after Close (never ErrKeyNotFound — a missing key is
	//     reported as (false, nil))
	Exists(ctx context.Context, key string) (bool, error)

	// InvalidateTag deletes every key currently associated with tag.
	//
	// Parameters:
	//   - ctx: context.Context
	//   - tag: string — the tag whose member keys should be removed
	//
	// Returns:
	//   - int: the number of keys removed (0 if the tag has no members,
	//     which is not an error)
	//   - error: ErrCacheUnavailable on a backend failure, ErrClosed if
	//     called after Close
	//
	// Example:
	//
	//	n, err := cache.InvalidateTag(ctx, "tenant:acme")
	//	// n is how many keys under "tenant:acme" were removed
	InvalidateTag(ctx context.Context, tag string) (int, error)

	// Stats returns a snapshot of hit/miss/eviction counters and basic
	// latency info since the cache was created or last reset.
	//
	// Parameters:
	//   - ctx: context.Context
	//
	// Returns:
	//   - Stats: see the Stats type doc comment for field semantics
	//   - error: ErrCacheUnavailable on a backend failure, ErrClosed if
	//     called after Close
	Stats(ctx context.Context) (Stats, error)

	// Close releases any underlying connections/resources (stops background
	// sweep goroutines for memory/postgres, closes the client for redis/
	// mongo/memcached). After Close, every other method returns ErrClosed.
	// Close is idempotent — calling it more than once is safe and returns
	// nil on every call after the first.
	//
	// Returns:
	//   - error: any error encountered while releasing resources
	Close() error
}

// Stats is a snapshot of cache counters and latency, returned by Cache.Stats.
// Counters are cumulative since the cache was constructed — there is no
// Reset method; construct a new Cache to start counters over.
type Stats struct {
	// Hits is the number of Get calls that found a live (non-expired) entry.
	Hits uint64

	// Misses is the number of Get calls for a key that did not exist or had
	// expired.
	Misses uint64

	// Evictions is the number of entries removed by a backend's own
	// expiry/reclamation mechanism (a sweep goroutine for memory/postgres),
	// not by an explicit Delete or InvalidateTag call.
	Evictions uint64

	// KeyCount is the current number of live keys, or -1 if the backend
	// cannot report this cheaply (Redis, which would need a full SCAN to
	// count keys under its prefix).
	KeyCount int64

	// AvgLatency is reserved for future use; backends may leave it zero.
	AvgLatency time.Duration
}
