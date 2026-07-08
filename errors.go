// File: errors.go

package grcache

import "errors"

// Sentinel errors for use with errors.Is. Backend implementations translate
// their own native errors (redis.Nil, gorm.ErrRecordNotFound,
// mongo.ErrNoDocuments, memcache.ErrCacheMiss, ...) into these sentinels
// before wrapping with fmt.Errorf("...: %w", ...) — a backend-native error
// must never leak through the Cache interface unwrapped, since doing so
// would break the backend-agnostic contract the interface exists to provide.
//
// There is deliberately no IsNotFound(err error) bool helper: callers use
// errors.Is(err, grcache.ErrKeyNotFound) directly, consistent with how
// gourdiantoken's own sentinel errors are consumed.
var (
	// ErrKeyNotFound indicates the key does not exist or has expired.
	ErrKeyNotFound = errors.New("grcache: key not found")

	// ErrCacheUnavailable indicates the backend could not be reached
	// (connection failure, timeout, etc.).
	ErrCacheUnavailable = errors.New("grcache: backend unavailable")

	// ErrInvalidTTL indicates a negative ttl was passed to Set.
	ErrInvalidTTL = errors.New("grcache: invalid ttl")

	// ErrClosed indicates a method was called after Close.
	ErrClosed = errors.New("grcache: cache is closed")
)
