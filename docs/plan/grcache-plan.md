# grcache — Detailed Scope & Implementation Planning Document

**Repo path (to be created):** `~/Dev/gourdian25/grcache`
**Reference repos already in workspace:** `~/Dev/gourdian25/gourdiantoken`, `~/Dev/gourdian25/grlog`

---

## Instructions for the IDE agent

Before writing any code, do the following in order:

1. **Read `~/Dev/gourdian25/gourdiantoken` in full.** Pay specific attention to:
   - How the Redis backend is implemented (connection setup, pooling, timeout
     handling, context propagation, key naming/prefixing conventions).
   - How errors are defined and wrapped (sentinel errors, `errors.Is`/`errors.As`
     usage, custom error types).
   - How the storage abstraction (`TokenRepository` interface) is structured —
     package layout, where interfaces live vs. implementations, constructor
     patterns (functional options vs. config structs).
   - How configuration is passed into backends (are there `Config` structs per
     backend? Is there a shared `RedisConfig` reused by more than one backend?).
   - Testing conventions: table-driven tests, use of `miniredis` or a real Redis
     via testcontainers, benchmark style, race-detector usage in CI.

2. **Read `~/Dev/gourdian25/grlog` in full.** Pay specific attention to:
   - Package layout philosophy (Logger View → Logger Core → Formatter → Sink →
     Writer) — this layered-interface style should inform grcache's own
     layering if applicable.
   - How it achieves zero-dependency / minimal-dependency status — what it
     avoids importing, and why.
   - Any existing in-memory data structure work (e.g. buffers, ring buffers,
     sampling counters) that could be reused for grcache's in-memory backend
     instead of writing one from scratch.

3. **Only after both reads**, produce an implementation plan for grcache that:
   - Explicitly states where grcache's Redis backend will reuse code/patterns
     from gourdiantoken's Redis backend, and where it must diverge (and why).
   - Explicitly states the folder structure it intends to create, matching the
     conventions observed in the two reference repos.
   - Flags any inconsistency between gourdiantoken's and grlog's conventions
     that grcache would otherwise have to choose between (e.g. if one uses
     functional options and the other uses config structs, pick one and state
     the reasoning).

Do not begin implementation until this plan has been reviewed.

---

## 1. Vision

grcache is a generic, backend-agnostic caching abstraction for the gourdian
ecosystem — the same architectural pattern gourdiantoken uses for token
storage (`TokenRepository` interface → multiple backend implementations),
applied instead to general-purpose caching. It is the shared caching layer
that `grauth` (permission/session caching), `graudit` (read-path caching), and
`gourdianerp` (application-level caching) will all depend on.

It is **not** a new cache implementation from scratch where avoidable — for
Redis specifically, it should behave as a thin, idiomatic wrapper that mirrors
gourdiantoken's already-proven connection handling rather than reinventing
pooling/retry logic.

---

## 2. Scope

### 2.1 In scope (v1)

- Core `Cache` interface: `Get`, `Set`, `Delete`, `Exists`, `InvalidateTag`.
- Backends:
  - **In-memory** — default, zero external dependency, used for local dev and
    testing.
  - **Redis** — primary production backend. Must be built by directly
    referencing gourdiantoken's existing Redis backend code.
  - **Memcached** — secondary backend, lower priority than Redis.
  - **Dragonfly / Valkey** — not separate backends; documented as
    "Redis-protocol compatible, use the Redis backend directly" since they
    speak the same wire protocol. No new code required — this should be a
    documentation note, not an implementation task.
- TTL support on every `Set` call (per-key expiry).
- Tag-based invalidation: a key can be associated with one or more tags at
  `Set` time; `InvalidateTag` removes all keys associated with a tag.
- Basic metrics: hit count, miss count, eviction count, per-backend latency
  (exposed via a `Stats()` method, not a full Prometheus integration in v1 —
  that's an adapter, see §6).
- Context-aware everywhere (`context.Context` as first param on every method,
  matching gourdiantoken's convention).
- Sentinel errors (`ErrKeyNotFound`, `ErrCacheUnavailable`, etc.), following
  gourdiantoken's error style exactly.

### 2.2 Explicitly out of scope (v1)

- **General-purpose data store behavior.** No query language, no filtering by
  value, no secondary indexes, no range scans. If a consumer needs to "find
  all keys matching X pattern," that's a modeling mistake upstream, not a
  grcache feature request.
- **Distributed cache coherence / invalidation propagation across nodes.**
  grcache does not attempt to keep multiple in-memory caches on different
  processes in sync. If you're running the in-memory backend across multiple
  replicas, know that they will diverge — that's expected and documented, not
  a bug. Use the Redis backend for anything requiring shared state.
- **Write-through / write-behind persistence patterns.** grcache is a cache,
  not a durability layer. No consumer should treat a cache miss on grcache as
  data loss.
- **Cache warming / preloading orchestration.** Out of scope for the library;
  consumers can call `Set` in a loop at startup if they want this.
- **Prometheus/OpenTelemetry metrics export.** v1 exposes a `Stats()` snapshot
  method only; wiring that into an actual metrics backend is a
  consumer-side or adapter-package concern (see Roadmap).

---

## 3. Public API

### 3.1 Core interface

```go
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

	// Close releases any underlying connections/resources.
	Close() error
}

type Stats struct {
	Hits       uint64
	Misses     uint64
	Evictions  uint64
	KeyCount   int64         // -1 if backend cannot report this cheaply (e.g. Redis without SCAN)
	AvgLatency time.Duration
}
```

### 3.2 Sentinel errors

```go
package grcache

import "errors"

var (
	ErrKeyNotFound      = errors.New("grcache: key not found")
	ErrCacheUnavailable = errors.New("grcache: backend unavailable")
	ErrInvalidTTL       = errors.New("grcache: invalid ttl")
	ErrClosed           = errors.New("grcache: cache is closed")
)
```
*(Agent: confirm this matches gourdiantoken's actual sentinel-error naming
and wrapping convention — e.g. does it use `fmt.Errorf("...: %w", ...)`
wrapping around these, and does it define an `IsNotFound(err error) bool`
style helper? Mirror whatever pattern is found there.)*

### 3.3 Constructors (pattern TBD by agent)

Two candidate patterns — the agent should pick whichever matches
gourdiantoken's actual convention, not default to guessing:

**Option A — Config struct (if this is what gourdiantoken uses):**
```go
type RedisConfig struct {
	Addr         string
	Password     string
	DB           int
	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func NewRedisCache(cfg RedisConfig) (Cache, error)
```

**Option B — Functional options (if this is what gourdiantoken uses):**
```go
func NewRedisCache(addr string, opts ...RedisOption) (Cache, error)

func WithPassword(pw string) RedisOption
func WithPoolSize(n int) RedisOption
func WithDB(n int) RedisOption
```

**Agent decision required:** inspect gourdiantoken's actual constructor style
for its Redis token-storage backend and use the same one here. Ecosystem-wide
consistency matters more than either option being "better" in isolation.

---

## 4. Backend implementation notes

### 4.1 In-memory backend

- Backing structure: a mutex-protected (or sharded, if grlog's internal
  buffer/sink code has a sharding pattern worth reusing) `map[string]entry`
  where `entry` holds the value, expiry time, and associated tags.
- A background goroutine sweeps expired keys periodically (reuse
  gourdiantoken's "background cleanup" pattern for token storage — same
  problem shape: periodic sweep of expired entries).
  - Check: does gourdiantoken expose a reusable generic ticker/cleanup-worker
    helper internally that could be extracted into a shared internal package,
    or does grcache need its own copy? Flag this as a possible future
    extraction into a tiny internal shared utility, not a hard requirement now.
- Tag index: a second map, `map[string]map[string]struct{}` (tag → set of
  keys), updated on every `Set`/`Delete`/expiry sweep.

### 4.2 Redis backend

- **Must directly reference gourdiantoken's Redis backend implementation.**
  Specifically:
  - Reuse the same Redis client library and version already used in
    gourdiantoken's `go.mod` — do not introduce a second Redis client library
    into the ecosystem.
  - Reuse the same connection-pool configuration defaults and the same
    dial/read/write timeout conventions.
  - Reuse the same error-wrapping style when Redis returns `redis.Nil` or a
    connection error — gourdiantoken already has to translate these into its
    own sentinel errors for token lookups; grcache's translation into
    `ErrKeyNotFound` / `ErrCacheUnavailable` should follow the identical
    approach.
- Tag support in Redis: store each tag as a Redis Set (`SADD tag:<tag> <key>`)
  alongside the value key. `InvalidateTag` does `SMEMBERS tag:<tag>` then
  pipelined `DEL` on each member plus the tag set itself. Use a Redis pipeline
  or Lua script (`EVAL`) for atomicity if gourdiantoken already uses Lua
  scripts anywhere (e.g. for atomic refresh-token rotation) — if so, match
  that approach here for consistency rather than introducing pipelining as a
  new pattern.

### 4.3 Memcached backend

- Lower priority; implement after in-memory and Redis are solid.
- Memcached has no native tag support — implement tags as a secondary
  key-list stored under a `tag:<tag>` key holding a serialized list of member
  keys, accepting that this is eventually-consistent/best-effort compared to
  Redis's set-based approach. Document this limitation explicitly in the
  package doc comment so consumers don't assume parity with the Redis
  backend's invalidation guarantees.

### 4.4 Dragonfly / Valkey

- No separate backend code. Document in the package README: "Point the Redis
  backend's `Addr` at your Dragonfly/Valkey instance — it speaks the Redis
  protocol and requires no code changes." Verify this claim against Dragonfly
  and Valkey docs before publishing the note, rather than assuming full
  compatibility for every command grcache uses.

---

## 5. Package/folder structure (draft — agent to confirm against conventions observed)

```
grcache/
├── go.mod
├── README.md
├── cache.go              // Cache interface, Stats struct, sentinel errors
├── memory/
│   └── memory.go         // in-memory backend
├── redis/
│   └── redis.go          // Redis backend
├── memcached/
│   └── memcached.go      // Memcached backend
├── internal/
│   └── ...               // shared helpers not part of public API
└── grcache_test.go       // shared conformance test suite run against every backend
```

**Conformance test suite requirement:** write one shared test suite (table of
scenarios: set-then-get, expiry, tag invalidation, delete of non-existent key,
concurrent access) that runs against *every* backend via the common `Cache`
interface. This guarantees behavioral parity across backends and should be the
primary test artifact, with backend-specific tests only for things like
connection-failure handling that can't be expressed generically.

---

## 6. Roadmap / explicitly deferred items

These are real, acknowledged future needs — listed here so they're not
forgotten, but explicitly NOT part of v1:

- Prometheus/OpenTelemetry metrics adapter (`grcache-otel` or similar,
  separate module).
- Distributed invalidation pub/sub (likely built on top of `grevents` once
  that repo exists, rather than inside grcache itself).
- Cache-aside helper wrappers (e.g. a `GetOrSet(ctx, key, loader func() ([]byte, error))`
  convenience method) — genuinely useful, deferred only to keep v1's surface
  area minimal until the core interface has been proven in real usage inside
  `grauth`.

---

## 7. Testing & benchmarking strategy

- Table-driven tests per backend, plus the shared conformance suite (§5).
- In-memory backend: no external dependencies needed, runs anywhere.
- Redis backend: use `miniredis` for fast unit tests; add a `//go:build
  integration` tagged test file using a real Redis (via `testcontainers-go` or
  a docker-compose service) for CI's integration stage — mirror whichever of
  these gourdiantoken already uses rather than introducing a new test-infra
  pattern.
- Race detector (`go test -race`) mandatory for the in-memory backend given
  its concurrent map access.
- Benchmarks: `Get`/`Set` throughput per backend, and specifically the cost of
  `InvalidateTag` at varying tag-cardinality (10, 1k, 100k keys per tag) since
  this is the operation most likely to have hidden performance cliffs,
  especially on the Memcached backend's list-based tag emulation.

---

## 8. Dependencies

- `grconfig` (once it exists) — for backend connection configuration loading.
  Until `grconfig` exists, grcache's constructors accept plain Go structs/
  functional options directly (§3.3) so it isn't blocked waiting on grconfig.
- Redis client library — same one gourdiantoken already depends on (agent to
  confirm exact module and version from `go.mod`).
- Memcached client library — to be selected by agent based on
  actively-maintained options (flag this as an open decision, not pre-chosen).
- No dependency on grlog, grauth, or any higher-layer repo.

---

## 9. Evaluation questions for the agent (answer before implementing)

1. What exact Redis client library and version does gourdiantoken use? Confirm
   grcache will use the identical one.
2. Does gourdiantoken use config structs or functional options for backend
   constructors? Which one wins for grcache?
3. Does gourdiantoken have a reusable background-cleanup/ticker pattern that
   could be extracted into a tiny shared internal utility instead of being
   duplicated in grcache's in-memory backend?
4. Does gourdiantoken use Lua scripts (`EVAL`) anywhere for atomic multi-step
   Redis operations? If so, should `InvalidateTag`'s SMEMBERS+DEL sequence use
   the same technique for atomicity?
5. What error-wrapping convention does gourdiantoken use for translating
   backend-native errors (e.g. `redis.Nil`) into sentinel errors? Confirm
   grcache's `ErrKeyNotFound` translation matches it exactly.
6. Given everything read, is the folder structure in §5 consistent with how
   gourdiantoken and grlog are organized, or should it be adjusted?