# Architecture notes

## Divergences from sibling conventions

grcache deliberately diverges from `gourdiantoken` and `grlog` in four
places. These are considered, not accidental — documented here so a future
contributor doesn't "fix" them back into alignment.

### 1. Subpackage-per-backend layout

Both `gourdiantoken` and `grlog` use a single flat package with a
file-prefix naming convention (`gourdiantoken.<area>.go`). grcache instead
uses a root `grcache` package (the `Cache` interface, `Stats`, sentinel
errors — zero external dependencies) plus one importable subpackage per
backend: `grcache/memory`, `grcache/redis`, `grcache/memcached`,
`grcache/postgres`, `grcache/mongo`.

**Why:** a flat package would force every backend's client library
(go-redis, a memcached client, GORM + the Postgres driver, the Mongo
driver) into every consumer's dependency graph, even consumers using only
the in-memory backend. That would silently violate the requirement that
the in-memory backend has zero external dependencies.

### 2. Networked-backend constructors own their config

`gourdiantoken`'s backend constructors take an already-built client/handle
(`NewRedisTokenRepository(client *redis.Client)`,
`NewGormTokenRepository(db *gorm.DB)`,
`NewMongoTokenRepository(mongoDB *mongo.Database, transactionsEnabled bool)`).
grcache's networked backends instead each own a `<Backend>Config` struct
and build their own client internally:
`New<Backend>Cache(cfg <Backend>Config) (grcache.Cache, error)`.

**Why:** grcache is meant to be usable as a standalone cache library;
callers shouldn't need to already have a `*redis.Client`/`*gorm.DB`/
`*mongo.Database` sitting around just to use it. This also sidesteps
gourdiantoken's own inconsistency, where Mongo's constructor takes an
extra positional `transactionsEnabled bool` that doesn't fit the otherwise
uniform shape — every grcache backend constructor has the identical
`New<Backend>Cache(cfg Config) (grcache.Cache, error)` signature.

### 3. Real local services in tests, not mocks — but different connection details than gourdiantoken

Like gourdiantoken, grcache's tests run against real local Redis/
Postgres/Mongo/Memcached instances — no `miniredis`, no
`testcontainers-go`, no `//go:build integration` tags. Unlike
gourdiantoken, grcache's test suite intentionally uses different
DB indices/database names (Redis DB 14 vs. gourdiantoken's DB 15,
Postgres database `grcache_test` vs. `postgres_db`, a dedicated Mongo
database rather than gourdiantoken's) so both suites can run against the
same local service instances without colliding.

### 4. Latest dependency versions, not version-matched to gourdiantoken

The original plan pinned grcache's Redis/GORM/Mongo dependencies to the
exact versions gourdiantoken uses, for cross-repo consistency. This was
superseded: grcache now tracks the latest available version of each
dependency it actually uses (see the README's Dependencies table), rather
than matching gourdiantoken's pinned versions. `go.mongodb.org/mongo-driver`
is a partial exception — grcache stays on the v1 module (latest v1.x patch)
rather than migrating to the `/v2` module path, since that would be a
breaking API rewrite out of scope for a routine dependency bump.

## Logging

Every backend accepts an optional `grcache.Logger` (see `logger.go`) for
diagnostic messages — connection failures, sweep-cycle summaries, shutdown.
The interface is deliberately minimal (`Infof`/`Warnf`/`Errorf`) and defined
in the root package using only stdlib types, so no backend's production
code needs to import a logging library to accept one structurally. `grlog`
is used only in this module's own test suite (`logger_test.go` and each
backend's `TestWithLogger`) to prove a real `*grlog.Logger` satisfies the
interface — it is never a dependency of any backend's non-test code, so it
does not leak into consumers who only import `grcache/memory` (or any other
backend) and don't want a logging dependency at all.

## Backend-specific design notes

- **In-memory**: single `sync.RWMutex`-protected map, not sharded. There
  is no ready-made sharded-map data structure in grlog to reuse (only
  concurrency idioms — see below), so sharding is deferred until
  benchmarks (Phase 7) show it's actually needed.
- **TTL sweep shutdown idiom** (`memory`, `postgres`): mirrors grlog's
  `closed atomic.Bool` + `closeChan` + `sync.WaitGroup` idiom, with
  `Close()` guarded by `CompareAndSwap` for exactly-once shutdown.
- **Error translation**: backend-native errors (`redis.Nil`,
  `gorm.ErrRecordNotFound`, `mongo.ErrNoDocuments`,
  `memcache.ErrCacheMiss`) are always translated into a grcache sentinel
  before wrapping — never left bare for callers to match against a
  third-party type. This is the one place grcache does not copy
  gourdiantoken's `ErrX = thirdparty.ErrY` re-export pattern: leaking a
  backend-native error through `Cache` would break the backend-agnostic
  contract the interface exists to provide.
- **No Lua/`EVAL`**: gourdiantoken's doc comments claim Lua scripting for
  atomic operations, but no real usage exists in its codebase (confirmed
  by inspection) — atomicity there is actually achieved via `Pipeline`/
  `SETNX`. grcache's Redis `InvalidateTag` matches gourdiantoken's *actual*
  behavior (pipelining), not its documentation.
- **Mongo TTL index**: reuses gourdiantoken's confirmed
  `expireAfterSeconds` TTL-index convention — the one backend besides
  Redis where expiry is the database's job, not grcache's. Documents with
  no `expiresAt` field are never touched by the TTL monitor, giving "no
  expiry" for free.
- **No `IsNotFound(err error) bool` helper**: gourdiantoken has no
  precedent for one; callers use `errors.Is` directly against sentinels.
  grcache stays consistent rather than inventing a new ecosystem
  convention unilaterally.
