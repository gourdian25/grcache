# 🗄️ grcache — Generic, Backend-Agnostic Caching for Go

grcache is a generic, backend-agnostic caching abstraction for the gourdian
ecosystem — the same architectural pattern
[gourdiantoken](https://github.com/gourdian25/gourdiantoken) uses for token
storage (`TokenRepository` interface → multiple backend implementations),
applied instead to general-purpose caching. It is the shared caching layer
that `grauth` (permission/session caching), `graudit` (read-path caching),
and `gourdianerp` (application-level caching) depend on.

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

## 🎯 Why grcache?

- **One interface, five backends.** Write your caching code once against
  `Cache`; swap in-memory for Redis (or Postgres, or Mongo, or memcached)
  by changing one constructor call.
- **Tag-based invalidation everywhere.** Every backend supports tagging a
  key at `Set` time and bulk-removing everything under a tag later — this
  isn't a Redis-only feature bolted on top.
- **No surprises across backends.** A shared conformance test suite runs
  the exact same behavioral assertions against all five backends, so
  switching backends doesn't mean re-learning edge-case semantics.
- **Zero dependency for the common case.** Import `grcache/memory` alone
  and you pull in nothing beyond the Go standard library.
- **Optional, adapter-free logging.** Plug in
  [grlog](https://github.com/gourdian25/grlog) (or any logger with the
  same three methods) with zero glue code.

## 📚 Table of Contents

- [Installation](#-installation)
- [Quick Start](#-quick-start)
- [Architecture](#-architecture)
- [Backends](#-backends)
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

Each backend is its own importable subpackage, so only add what you use:

```sh
go get github.com/gourdian25/grcache/memory      # zero extra dependencies
go get github.com/gourdian25/grcache/redis       # + github.com/redis/go-redis/v9
go get github.com/gourdian25/grcache/memcached   # + github.com/bradfitz/gomemcache
go get github.com/gourdian25/grcache/postgres    # + gorm.io/gorm, gorm.io/driver/postgres
go get github.com/gourdian25/grcache/mongostore  # + go.mongodb.org/mongo-driver
```

## 🚀 Quick Start

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/gourdian25/grcache/memory"
)

func main() {
	cache, err := memory.NewMemoryCache()
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
grcache (root)              — Cache interface, Stats, sentinel errors, Logger interface
  ├── grcache/memory         — in-process map, zero dependencies
  ├── grcache/redis          — github.com/redis/go-redis/v9
  ├── grcache/memcached      — github.com/bradfitz/gomemcache
  ├── grcache/postgres       — gorm.io/gorm + gorm.io/driver/postgres
  ├── grcache/mongostore     — go.mongodb.org/mongo-driver
  └── grcache/conformance    — shared behavioral test suite (imported by every backend's own tests)
```

Unlike gourdiantoken and grlog (both flat, single-package repos), grcache
uses one subpackage per backend. This is deliberate: a flat package would
force every backend's client library into every consumer's dependency
graph, even consumers using only `grcache/memory` — see
[`docs/architecture.md`](docs/architecture.md) for this and grcache's other
documented divergences from sibling conventions.

## 💾 Backends

| Backend    | Package             | Expiry mechanism            | Tag storage                                                              |
|------------|----------------------|------------------------------|---------------------------------------------------------------------------|
| In-memory  | `grcache/memory`     | Application sweep goroutine | In-process map                                                           |
| Redis      | `grcache/redis`      | Native (`EX`) + lazy backstop | Redis Sets, pipelined `SMEMBERS`+`DEL`                                   |
| Memcached  | `grcache/memcached`  | Native (`Expiration`)        | Serialized list — best-effort, eventually consistent (see below)         |
| PostgreSQL | `grcache/postgres`   | Application sweep goroutine | Join table (`grcache_entry_tags`), kept in sync every `Set`/`Delete`      |
| MongoDB    | `grcache/mongostore` | Native TTL index (`expireAfterSeconds: 0`) | Embedded array field on the same document          |

Redis is the recommended default for production. PostgreSQL and MongoDB
exist specifically for test/dev/CI environments where a Redis (or
memcached) instance isn't available but a Postgres or Mongo instance
already is — not as a general recommendation to run a cache on top of a
relational or document database in production instead of Redis.

### In-memory

```go
cache, err := memory.NewMemoryCache(
	memory.WithSweepInterval(30 * time.Second), // default
	memory.WithLogger(logger),                   // optional
)
```

Zero external dependencies. Does not coordinate state across processes or
replicas — running it behind multiple app instances means each instance
has its own independent cache that will diverge, which is expected.

### Redis

```go
cache, err := redis.NewRedisCache(redis.RedisConfig{
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
cache, err := memcached.NewMemcachedCache(memcached.MemcachedConfig{
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
than truncating to 0 (which would mean "never expire").

### PostgreSQL

```go
cache, err := postgres.NewPostgresCache(postgres.PostgresConfig{
	DSN:             "host=localhost user=myuser password=mypass dbname=mydb port=5432 sslmode=disable",
	MaxOpenConns:    0, // database/sql default
	MaxIdleConns:    0, // database/sql default
	ConnMaxLifetime: 0, // reuse indefinitely
	SweepInterval:   30 * time.Second, // default
	Logger:          logger,            // optional
})
```

Via GORM. Two tables: `grcache_entries` (key/value/expires_at) and
`grcache_entry_tags` (a composite-indexed join table), auto-migrated on
construct. Postgres has no native expiry at all — unlike Redis/Mongo, the
background sweep here is the *only* reclamation mechanism, not a backstop;
`Get`/`Exists`'s lazy expiry check is what keeps reads correct between
sweeps.

Intended for test/dev/CI environments with a Postgres instance already
available but no Redis/memcached — prefer Redis in production.

### MongoDB

```go
cache, err := mongo.NewMongoCache(mongo.MongoConfig{
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

Backend-native errors (`redis.Nil`, `gorm.ErrRecordNotFound`,
`mongo.ErrNoDocuments`, `memcache.ErrCacheMiss`) are always translated into
a grcache sentinel before being wrapped — a caller using `errors.Is`
against grcache's own sentinels never needs to know which backend is
underneath. There is deliberately no `IsNotFound(err error) bool` helper;
use `errors.Is` directly, consistent with how gourdiantoken's own sentinel
errors are consumed.

## 📝 Optional Logging

Every backend accepts an optional `Logger` (a `Config.Logger` field, or
`memory.WithLogger(...)` for the in-memory backend) for diagnostic messages
— connection failures, sweep-cycle summaries, shutdown. Logging is entirely
opt-in: a nil Logger (the default) means grcache logs nothing, and the
`grcache` root package itself does not depend on any logging library.

`grcache.Logger` is a tiny structural interface (`Infof`/`Warnf`/`Errorf`),
satisfied directly by [grlog](https://github.com/gourdian25/grlog)'s
`*grlog.Logger` — the ecosystem's own recommended choice — with no adapter
needed:

```go
import (
	"github.com/gourdian25/grlog"
	"github.com/gourdian25/grcache/redis"
)

logger := grlog.NewDefaultLogger()

cache, err := redis.NewRedisCache(redis.RedisConfig{
	Addr:   "localhost:6379",
	Logger: logger,
})
```

Any logger exposing the same three methods works — grlog is not required.

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
| MongoDB | `mongodb://root:mongo_password@localhost:27018/?replicaSet=rs0&authSource=admin&directConnection=true`, database `grcache_test` |
| Memcached | `localhost:11211` |

The Redis DB index and Postgres/Mongo database names are deliberately
different from gourdiantoken's own test settings (DB 15, `gourdiantoken_test`,
and its own Mongo database) so both suites can run against the same shared
instances without colliding.

Every package independently maintains at least 80% test coverage, matching
gourdiantoken's own `COVERAGE_MIN` convention:

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
make coverage-check        # every package must independently meet 80%
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

See [`CHANGELOG.md`](CHANGELOG.md) for release history.

## 📄 License

MIT — see [`LICENSE`](LICENSE).
