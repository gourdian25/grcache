# 🗄️ grcache — Generic, Backend-Agnostic Caching for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/gourdian25/grcache.svg)](https://pkg.go.dev/github.com/gourdian25/grcache)
[![Go Version](https://img.shields.io/badge/go-1.26.4+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

grcache is a generic, backend-agnostic caching abstraction for the gourdian
ecosystem — the same architectural pattern
[gourdiantoken](https://github.com/gourdian25/gourdiantoken) uses for token
storage (`TokenRepository` interface → multiple backend implementations),
applied instead to general-purpose caching. It is the shared caching layer
that `grauth` (permission/session caching) and `graudit` (read-path caching)
depend on, and that any service needing backend-agnostic caching can build on
directly — `grnoti`, for example, wraps a `Cache` directly for idempotent
event tracking, preference caching, and A/B experiment assignment (see below).

## 🌐 Part of the gourdian25 ecosystem

grcache is one of several small, independent Go libraries meant to be used
together:

- [gourdiantoken](https://github.com/gourdian25/gourdiantoken) — JWT
  access/refresh token issuance, verification, revocation, and rotation.
- [grlog](https://github.com/gourdian25/grlog) — zero-dependency structured
  logging; grcache's optional `Logger` interface is satisfied by it directly.
- [grevents](https://github.com/gourdian25/grevents) — an in-process event
  bus for decoupling producers of state changes from consumers that react
  to them.
- [graudit](https://github.com/gourdian25/graudit) — an append-only audit
  log with pluggable storage backends, mirroring grcache's own layout.
- [grpolicy](https://github.com/gourdian25/grpolicy) — attribute-based
  policy evaluation (RBAC/ABAC), independent of any notion of "user" or
  "role".
- [grnoti](https://github.com/gourdian25/grnoti) — a push-notification
  service (FCM dispatch, idempotent event processing, device-token
  management, DLQ retry, circuit breaking, distributed rate limiting,
  deterministic A/B experiment assignment, localization, topic-based
  routing). A direct grcache consumer: `NewCacheIdempotencyStore`,
  `NewCachedPreferencesStore`, and `NewCacheBackedExperimentEngine` each
  wrap a `grcache.Cache` for idempotent event tracking, read-through
  preference caching, and cache-backed experiment assignment respectively.

## 🎯 Why grcache?

- **One interface, five backends.** Write your caching code once against
  `Cache`; swap in-memory for Redis (or Postgres, or Mongo, or memcached)
  by changing one constructor call.
- **Tag-based invalidation everywhere.** Every backend supports tagging a
  key at `Set` time and bulk-removing everything under a tag later — this
  isn't a Redis-only feature bolted on top.
- **No surprises across backends.** A shared contract test suite runs
  the exact same behavioral assertions against all five backends, so
  switching backends doesn't mean re-learning edge-case semantics.
- **One flat package.** `go get github.com/gourdian25/grcache` and every
  backend's constructor is right there — no subpackage-per-backend
  navigation, matching every other repo in the gourdian ecosystem.
- **Optional, slog-shaped logging.** Plug in
  [grlog](https://github.com/gourdian25/grlog) via its `log/slog` adapter
  (or any logger with the same four methods, including `*slog.Logger`
  directly) with zero glue code.

## 📚 Table of Contents

- [Installation](#-installation)
- [Quick Start](#-quick-start)
- [Architecture](#-architecture)
- [Backends](#-backends)
- [Thread Safety](#-thread-safety)
- [Tag-Based Invalidation](#-tag-based-invalidation)
- [TTL Semantics](#-ttl-semantics)
- [Stats & Observability](#-stats--observability)
- [Error Handling](#-error-handling)
- [Optional Logging](#-optional-logging)
- [Backend Compatibility](#-backend-compatibility)
- [Testing](#-testing)
- [Benchmarks](#-benchmarks)
- [Roadmap](#-roadmap)
- [Out of Scope](#-out-of-scope)
- [Contributing](#-contributing)
- [Releasing](#-releasing)
- [License](#-license)

## 📦 Installation

```sh
go get github.com/gourdian25/grcache
```

grcache is a single flat package — this one `go get` pulls in every
backend's dependency (go-redis, gomemcache, pgx/v5, mongo-driver) whether
you use that backend or not. See
[`docs/architecture.md`](docs/architecture.md) §1 for why this trades away
per-backend dependency isolation for consistency with the rest of the
gourdian ecosystem's flat-package convention.

## 🚀 Quick Start

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/gourdian25/grcache"
)

func main() {
	cache, err := grcache.NewMemoryCache()
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

	ctx := context.Background()

	if err := cache.Set(ctx, "user:42", []byte("alice"), time.Minute, "tenant:acme"); err != nil {
		log.Fatal(err)
	}

	val, err := cache.Get(ctx, "user:42")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(string(val)) // "alice"

	n, err := cache.InvalidateTag(ctx, "tenant:acme")
	log.Printf("invalidated %d keys", n) // 1
}
```

See [`example/example.go`](example/example.go) for a runnable demo covering
all five backends (`go run ./example`) — the networked backends print a
"skipping" message and continue if their local service isn't running.

## 🏗️ Architecture

```
grcache (one flat package)
  ├── cache.go, errors.go, logger.go, docs.go   — Cache interface, Stats, sentinel errors, Logger interface
  ├── memory.go       — in-process map, zero dependencies
  ├── redis.go         — github.com/redis/go-redis/v9
  ├── memcached.go      — github.com/bradfitz/gomemcache
  ├── postgres.go        — github.com/jackc/pgx/v5 + sqlc-generated queries (internal/postgresdb)
  ├── mongo.go            — go.mongodb.org/mongo-driver
  └── contract_cache_test.go — shared behavioral test suite (TestCache_Contract, one subtest per backend)
```

grcache was originally one subpackage per backend, to keep each backend's
client library out of a consumer's dependency graph unless they imported
that specific backend. That layout was reversed for consistency with the
rest of the gourdian ecosystem's flat-package convention — see
[`docs/architecture.md`](docs/architecture.md) for the full rationale and
grcache's other documented divergences from sibling conventions.

## 💾 Backends

| Backend    | Constructor          | Expiry mechanism            | Tag storage                                                              |
|------------|----------------------|------------------------------|---------------------------------------------------------------------------|
| In-memory  | `NewMemoryCache`      | Application sweep goroutine | In-process map                                                           |
| Redis      | `NewRedisCache`       | Native (`EX`) + lazy backstop | Redis Sets, pipelined `SMEMBERS`+`DEL`                                   |
| Memcached  | `NewMemcachedCache`   | Native (`Expiration`)        | Serialized list — best-effort, eventually consistent (see below)         |
| PostgreSQL | `NewPostgresCache`    | Application sweep goroutine | Join table (`grcache_entry_tags`), kept in sync every `Set`/`Delete`      |
| MongoDB    | `NewMongoCache`       | Native TTL index (`expireAfterSeconds: 0`) | Embedded array field on the same document          |

Redis is the recommended default for production. PostgreSQL and MongoDB
exist specifically for test/dev/CI environments where a Redis (or
memcached) instance isn't available but a Postgres or Mongo instance
already is — not as a general recommendation to run a cache on top of a
relational or document database in production instead of Redis.

### In-memory

```go
cache, err := grcache.NewMemoryCache(
	grcache.WithSweepInterval(30 * time.Second), // default
	grcache.WithLogger(logger),                   // optional
)
```

Zero external dependencies. Does not coordinate state across processes or
replicas — running it behind multiple app instances means each instance
has its own independent cache that will diverge, which is expected.

### Redis

```go
cache, err := grcache.NewRedisCache(grcache.RedisConfig{
	Addr:         "localhost:6379", // required
	Password:     "",
	DB:           0,
	PoolSize:     100,             // default
	DialTimeout:  5 * time.Second,  // default
	ReadTimeout:  3 * time.Second,  // default
	WriteTimeout: 3 * time.Second,  // default
	Logger:       logger,           // optional
})
```

Built directly on gourdiantoken's proven Redis conventions: the same
`Ping`-on-construct validation and error-wrapping style. Tags are Redis
Sets; `InvalidateTag` pipelines `SMEMBERS` + `DEL` in one round trip. No
Lua/`EVAL` — gourdiantoken's own docs claim Lua scripting but its actual
code only ever uses `Pipeline`/`SETNX`, so grcache matches gourdiantoken's
*real* behavior, not its documentation.

### Memcached

```go
cache, err := grcache.NewMemcachedCache(grcache.MemcachedConfig{
	Servers:      []string{"localhost:11211"}, // required
	Timeout:      500 * time.Millisecond,        // default
	MaxIdleConns: 2,                              // default
	Logger:       logger,                         // optional
})
```

**Documented limitation:** memcached has no native set/list type, so tags
are emulated with a newline-delimited member list under its own key,
updated by read-modify-write on every tagged `Set`. This is explicitly
best-effort/eventually-consistent — concurrent `Set` calls tagging the same
tag can race and drop a member, meaning that key simply won't be swept up
by a later `InvalidateTag` (the key itself is unaffected; only the tag
index entry can be lost). Also note: memcached only supports
second-granularity TTLs, so a sub-second `ttl` rounds up to 1 second rather
than truncating to 0 (which would mean "never expire"). Both cache-value
keys and tag-list keys are namespaced (`grcache:val:`/`grcache:tag:`) —
previously only tag-list keys were prefixed, a gap fixed alongside this
backend's GORM-era-adjacent flattening pass.

### PostgreSQL

```go
cache, err := grcache.NewPostgresCache(grcache.PostgresConfig{
	DSN:             "host=localhost user=myuser password=mypass dbname=mydb port=5432 sslmode=disable",
	MaxConns:        0, // pgxpool default
	MinConns:        0, // pgxpool default
	MaxConnLifetime: 0, // reuse indefinitely
	SweepInterval:   30 * time.Second, // default
	Logger:          logger,            // optional
})
```

Via `pgx/v5` with sqlc-generated queries (no ORM) — replacing an earlier
GORM implementation. Two tables: `grcache_entries` (key/value/nullable
expires_at) and `grcache_entry_tags` (a composite-indexed join table),
schema applied on connect (`CREATE TABLE/INDEX IF NOT EXISTS`, serialized
by a Postgres advisory lock so concurrent callers building a cache against
the same fresh database don't race on the DDL). Postgres has no native
expiry at all — unlike Redis/Mongo, the background sweep here is the
*only* reclamation mechanism, not a backstop; `Get`/`Exists`'s lazy expiry
check is what keeps reads correct between sweeps.

Intended for test/dev/CI environments with a Postgres instance already
available but no Redis/memcached — prefer Redis in production.

### MongoDB

```go
cache, err := grcache.NewMongoCache(grcache.MongoConfig{
	URI:        "mongodb://localhost:27017", // required
	Database:   "myapp",                       // required
	Collection: "grcache_entries",              // default
	Logger:     logger,                         // optional
})
```

Tags live directly as an array field on the same document — no join table
needed, unlike Postgres. A TTL index (`expireAfterSeconds: 0`) gives
native, database-managed expiry, the same as Redis's `EX`; documents with
no `expiresAt` field (ttl=0) are simply never touched by the TTL monitor.

Intended for test/dev/CI environments with a Mongo instance already
available but no Redis/memcached — prefer Redis in production.

## 🔒 Thread Safety

Every `Cache` implementation returned by this package is safe for
concurrent use by multiple goroutines — `Get`, `Set`, `Delete`, `Exists`,
`InvalidateTag`, `Stats`, and `Close` can all be called concurrently from
any number of goroutines with no external locking required. `Close` is
idempotent on every backend (`sync.Once`, or an atomic compare-and-swap for
the in-memory backend) — calling it more than once, including
concurrently, is safe and returns `nil` on every call after the first.

Per-backend notes on top of that baseline guarantee:

- **In-memory** — a single `sync.RWMutex` guards both the value map and the
  tag index together, so there is no race window where a reader could see
  an updated value paired with a stale tag index, or vice versa.
- **Redis** — `Set` and `InvalidateTag` use a transactional pipeline
  (`TxPipeline`, real `MULTI`/`EXEC`) so a value write and its tag-set
  memberships apply atomically as a unit, even under concurrent callers.
- **Memcached** — per-key `Get`/`Set`/`Delete`/`Exists` are safe under
  concurrency, but tag *membership* is best-effort/eventually consistent:
  concurrent `Set` calls tagging the same tag can race and drop a member
  from that tag's list (the key itself is never affected — only whether a
  later `InvalidateTag` call for that tag catches it). See
  [Memcached](#memcached) above for the full tradeoff.
- **PostgreSQL** — schema creation (`CREATE TABLE/INDEX IF NOT EXISTS`) is
  serialized by a Postgres advisory lock, so multiple processes
  constructing a cache against the same fresh database concurrently don't
  race on DDL; per-key reads/writes use standard transactional statements.
- **MongoDB** — value, tags, and expiry are written together in one
  `ReplaceOne` per `Set`, so a concurrent reader never observes a
  partially-applied update.

None of the above extends across processes: the in-memory backend's state
is local to a single process by design (see [Out of Scope](#-out-of-scope)),
so multiple instances behind different app replicas are expected to
diverge. The other backends' concurrency guarantees are about safe
concurrent *access* to shared backend state, not about giving the in-memory
backend that same cross-process reach.

## 🏷️ Tag-Based Invalidation

```go
cache.Set(ctx, "session:abc", data, time.Hour, "user:42", "tenant:acme")
cache.Set(ctx, "session:def", data, time.Hour, "user:42")

n, err := cache.InvalidateTag(ctx, "user:42") // removes both sessions, n == 2
```

Use tags to group related keys (all sessions for a user, all cached rows
for a tenant) instead of tracking key lists yourself.

## ⏱️ TTL Semantics

A `ttl` of `0` passed to `Set` means "no expiry" on every backend — Redis
stores no `EX` flag, Mongo omits the `expiresAt` field entirely, and the
memory/postgres backends simply never sweep the entry. A negative `ttl`
returns `ErrInvalidTTL`. Every backend also checks expiry lazily on
`Get`/`Exists` in addition to whatever background mechanism it uses, so a
not-yet-reaped expired entry is never visible to a caller.

## 📊 Stats & Observability

```go
stats, err := cache.Stats(ctx)
// stats.Hits, stats.Misses, stats.Evictions, stats.KeyCount, stats.AvgLatency
```

`KeyCount` is `-1` for backends that can't report it cheaply (Redis, which
would need a full `SCAN` to count keys under its prefix). `Stats()` is a
snapshot only — wiring these numbers into Prometheus/OpenTelemetry is a
consumer-side or adapter-package concern, not something this package does.

## ⚠️ Error Handling

```go
val, err := cache.Get(ctx, key)
if errors.Is(err, grcache.ErrKeyNotFound) {
	// cache miss — expected control flow, not a failure
} else if err != nil {
	// ErrCacheUnavailable, ErrClosed, or ErrInvalidTTL
}
```

Backend-native errors (`redis.Nil`, `pgx.ErrNoRows`,
`mongo.ErrNoDocuments`, `memcache.ErrCacheMiss`) are always translated into
a grcache sentinel before being wrapped — a caller using `errors.Is`
against grcache's own sentinels never needs to know which backend is
underneath. There is deliberately no `IsNotFound(err error) bool` helper;
use `errors.Is` directly, consistent with how gourdiantoken's own sentinel
errors are consumed.

## 📝 Optional Logging

Every backend accepts an optional `Logger` (a `Config.Logger` field, or
`grcache.WithLogger(...)` for the in-memory backend) for diagnostic messages
— connection failures, sweep-cycle summaries, shutdown. Logging is entirely
opt-in: a nil Logger (the default) means grcache logs nothing, and the
`grcache` root package itself does not depend on any logging library.

`grcache.Logger` is a tiny structural interface shaped exactly like
`*slog.Logger` (`Debug`/`Info`/`Warn`/`Error(msg string, args ...any)`), so
`*slog.Logger` satisfies it directly. [grlog](https://github.com/gourdian25/grlog)
plugs in via its `log/slog` adapter — the ecosystem's own recommended
bridge:

```go
import (
	"log/slog"

	"github.com/gourdian25/grlog"
	"github.com/gourdian25/grcache"
)

logger := slog.New(grlog.NewSlogHandler(grlog.NewDefaultLogger()))

cache, err := grcache.NewRedisCache(grcache.RedisConfig{
	Addr:   "localhost:6379",
	Logger: logger,
})
```

Any logger exposing the same four methods works — grlog is not required.

## 🧩 Backend Compatibility

Dragonfly and Valkey are Redis-protocol compatible — point the Redis
backend's `RedisConfig.Addr` at your Dragonfly/Valkey instance; no separate
backend is needed. Verify compatibility against the specific commands
grcache issues (`GET`, `SET EX`, `DEL`, `EXISTS`, `SADD`, `SMEMBERS`,
pipelining) rather than assuming blanket compatibility.

## 🧪 Testing

grcache's tests run against real local services (no mocks, no
`miniredis`/`testcontainers-go`), mirroring gourdiantoken's testing
philosophy. These are the same shared containers grnoti, graudit, and
gourdiantoken test against (each repo gets its own database/keyspace/DB-index)
— start them with:

```sh
make docker-up   # starts the shared Postgres/Redis/Mongo/Memcached test containers
make docker-down # stops them when you're done
```

| Backend | Connection |
|---|---|
| Redis | `localhost:6379`, password `redis_password`, DB `14` |
| PostgreSQL | `host=localhost user=postgres_user password=postgres_password dbname=grcache_test port=5432 sslmode=disable` |
| MongoDB | `mongodb://root:mongo_password@localhost:27018/?directConnection=true`, database `grcache_test` |
| Memcached | `localhost:11211` |

The Redis DB index and Postgres/Mongo database names are deliberately
different from gourdiantoken's own test settings (DB 15, `gourdiantoken_test`,
and its own Mongo database) so both suites can run against the same shared
instances without colliding.

The root package maintains 95.6% test coverage, enforced by a 95% gate:

```sh
make coverage-check
```

## 📈 Benchmarks

```sh
make bench
```

`InvalidateTag` is benchmarked at 10/1k/100k-key tag cardinality per
backend, since it's the operation most likely to have a hidden performance
cliff. Indicative numbers from this repo's own dev machine (Apple M4; run
`make bench` for current numbers on your hardware):

| Backend | 10 keys | 1,000 keys | 100,000 keys |
|---|---|---|---|
| memory | ~9µs | ~133µs | ~9.2ms |
| Redis | ~3.2ms | ~3.1ms | ~68ms |
| PostgreSQL | ~26ms | ~26ms | ~93ms |
| MongoDB | ~7.6ms | ~11ms | ~404ms |
| memcached | ~4.1ms (10) | ~92ms (1,000, capped) | *not run — see below* |

Redis's numbers above are post-`v0.1.1`'s `TxPipeline` fix (see CHANGELOG.md)
— `MULTI`/`EXEC` overhead versus the previous non-transactional `Pipeline`
turned out to be within measurement noise (a few percent, not a step
change), since go-redis still sends the whole batch in one round trip
either way.

memcached's benchmark deliberately caps at 1,000 keys, not 100,000: its
list-based tag emulation does a read-modify-write of the *entire* member
list on every tagged `Set`, so populating a single tag with *n* keys costs
O(n²) total data movement. The 10→100→1,000 progression above (4ms → 12.6ms
→ 92ms) already demonstrates the scaling cliff clearly — a full 100,000-key
run would be impractically slow to include in a routine `make bench`, which
is itself the direct, expected consequence of the documented
eventual-consistency tradeoff, not a gap in the measurement.

## 🗺️ Roadmap

Explicitly deferred, not forgotten:

- Prometheus/OpenTelemetry metrics adapter (separate module).
- Distributed invalidation pub/sub, likely built on top of
  [`grevents`](https://github.com/gourdian25/grevents) (already released,
  not yet wired up here).
- Cache-aside helper wrappers (e.g. `GetOrSet(ctx, key, loader)`), deferred
  to keep v1's surface area minimal until the core interface has been
  proven in real usage inside `grauth`.

## 🚫 Out of Scope

- General-purpose data store behavior (no query language, no filtering by
  value, no secondary indexes, no range scans).
- Distributed cache coherence / invalidation propagation across nodes.
- Write-through / write-behind persistence patterns.
- Cache warming / preloading orchestration.
- Prometheus/OpenTelemetry metrics export (`Stats()` is a snapshot only).

## 🤝 Contributing

```sh
make fmt              # gofmt
make vet               # go vet
make lint              # golangci-lint (if installed)
make test               # go test -cover ./...
make race                # go test -race ./...  — required before any PR touching backend code
make coverage-check        # the root package must meet 95%
```

See [`CLAUDE.md`](CLAUDE.md) for the full architecture rundown and
[`docs/architecture.md`](docs/architecture.md) for the reasoning behind
grcache's deliberate divergences from gourdiantoken's and grlog's
conventions.

## 🚀 Releasing

Releases are built with [goreleaser](https://goreleaser.com):

```sh
make goreleaser-check          # dry run — validates .goreleaser.yaml, builds a local snapshot, no tag/push
make release VERSION=vX.Y.Z    # tags, pushes, and runs goreleaser release --clean
```

See [`CHANGELOG.md`](CHANGELOG.md) for release history and
[`SECURITY.md`](SECURITY.md) to report a vulnerability privately instead of
opening a public issue.

## 📄 License

MIT — see [`LICENSE`](LICENSE).
