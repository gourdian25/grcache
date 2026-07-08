# grcache

grcache is a generic, backend-agnostic caching abstraction for the gourdian
ecosystem — the same architectural pattern [gourdiantoken](https://github.com/gourdian25/gourdiantoken)
uses for token storage (`TokenRepository` interface → multiple backend
implementations), applied instead to general-purpose caching. It is the
shared caching layer that `grauth` (permission/session caching), `graudit`
(read-path caching), and `gourdianerp` (application-level caching) depend on.

It is not a new cache implementation from scratch where avoidable — for
Redis specifically, it behaves as a thin, idiomatic wrapper that mirrors
gourdiantoken's already-proven connection handling rather than reinventing
pooling/retry logic.

## Backends

| Backend    | Package             | Expiry mechanism         | Tag storage             |
|------------|----------------------|---------------------------|--------------------------|
| In-memory  | `grcache/memory`     | Application sweep goroutine | In-process map           |
| Redis      | `grcache/redis`      | Native (`EX`) + backstop  | Redis Sets               |
| Memcached  | `grcache/memcached`  | Native (`Expiration`)     | Serialized list (best-effort, eventually consistent — see package docs) |
| PostgreSQL | `grcache/postgres`   | Application sweep goroutine | Join table               |
| MongoDB    | `grcache/mongo`      | Native TTL index          | Embedded array field     |

Dragonfly and Valkey are Redis-protocol compatible — point the Redis
backend's `RedisConfig.Addr` at your Dragonfly/Valkey instance; no separate
backend is needed. Verify compatibility against the specific commands
grcache issues (`GET`, `SET EX`, `DEL`, `EXISTS`, `SADD`, `SMEMBERS`,
pipelining) rather than assuming blanket compatibility.

## Quick start

```go
import "github.com/gourdian25/grcache/memory"

cache, err := memory.NewMemoryCache()
if err != nil {
    log.Fatal(err)
}
defer cache.Close()

cache.Set(ctx, "user:42", []byte("..."), time.Minute, "user:42", "tenant:acme")
val, err := cache.Get(ctx, "user:42")
cache.InvalidateTag(ctx, "tenant:acme")
```

Each backend has its own constructor and `Config` struct — see the package
doc comment in `memory/`, `redis/`, `memcached/`, `postgres/`, `mongo/`.

## Optional logging

Every backend accepts an optional `Logger` (a `Config.Logger` field, or
`memory.WithLogger(...)` for the in-memory backend) for diagnostic messages
— connection failures, sweep-cycle summaries, shutdown. Logging is entirely
opt-in: a nil Logger (the default) means grcache logs nothing, and grcache
itself does not depend on any logging library.

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

## Testing

grcache's tests run against real local services (no mocks, no
`miniredis`/`testcontainers-go`), mirroring gourdiantoken's testing
philosophy. Start the services below before running `make test` / `make race`:

- Redis: `localhost:6379`, password `redis_password`, DB `14`.
- PostgreSQL: `host=localhost user=postgres_user password=postgres_password dbname=grcache_test port=5432 sslmode=disable`.
- MongoDB: `mongodb://root:mongo_password@localhost:27018/?directConnection=true`, database `grcache_test`.
- Memcached: `localhost:11211`.

The Redis DB index, Postgres database name, and Mongo database name are
deliberately different from gourdiantoken's own test settings (DB 15,
`postgres_db`, and its own Mongo database) to avoid collisions if both
suites ever run against the same local instances simultaneously.

Every package independently maintains at least 80% test coverage, matching
gourdiantoken's own `COVERAGE_MIN` convention — run `make coverage-check` to
verify (requires all four services above running locally).

## Dependencies

Unlike gourdiantoken and grlog, which pin dependency versions to match each
other for ecosystem consistency, grcache tracks the latest available
version of each dependency it actually uses:

| Dependency | Used by |
|---|---|
| `github.com/redis/go-redis/v9` | `grcache/redis` |
| `github.com/bradfitz/gomemcache` | `grcache/memcached` |
| `gorm.io/gorm`, `gorm.io/driver/postgres` | `grcache/postgres` |
| `go.mongodb.org/mongo-driver` | `grcache/mongo` |
| `github.com/gourdian25/grlog` | test-only, proving `*grlog.Logger` satisfies `grcache.Logger` |

The root `grcache` package and `grcache/memory` remain stdlib-only —
`grlog` is a dependency of this module's own test suite, not of any
backend's production code, so it never leaks into a consumer that only
imports `grcache/memory`.

## Roadmap

Explicitly deferred, not forgotten:

- Prometheus/OpenTelemetry metrics adapter (separate module).
- Distributed invalidation pub/sub, likely built on top of `grevents` once
  that repo exists.
- Cache-aside helper wrappers (e.g. `GetOrSet(ctx, key, loader)`), deferred
  to keep v1's surface area minimal until the core interface has been
  proven in real usage inside `grauth`.

## Out of scope (v1)

- General-purpose data store behavior (no query language, no filtering by
  value, no secondary indexes, no range scans).
- Distributed cache coherence / invalidation propagation across nodes.
- Write-through / write-behind persistence patterns.
- Cache warming / preloading orchestration.
- Prometheus/OpenTelemetry metrics export (v1 exposes `Stats()` only).

See `docs/architecture.md` for the deliberate divergences from
gourdiantoken's and grlog's conventions, and `docs/plan/grcache-plan.md`
for the full scope/spec document.
