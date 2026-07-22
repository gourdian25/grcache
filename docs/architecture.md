# Architecture notes

## Divergences from sibling conventions

grcache deliberately diverges from `gourdiantoken` and `grlog` in a few
places. These are considered, not accidental — documented here so a future
contributor doesn't "fix" them back into misalignment.

### 1. Flat package (no longer a divergence)

grcache originally used a root `grcache` package plus one importable
subpackage per backend (`grcache/memory`, `grcache/redis`,
`grcache/memcached`, `grcache/postgres`, `grcache/mongostore`), to keep each
backend's client library (go-redis, a memcached client, GORM + the
Postgres driver, the Mongo driver) out of a consumer's dependency graph
unless they actually imported that backend. This was reversed: grcache is
now a single flat package (`memory.go`, `redis.go`, `memcached.go`,
`postgres.go`, `mongo.go` at the root, each defining its own unexported
concrete type — `memoryCache`, `redisCache`, etc. — behind the shared
`Cache` interface), matching every other repo in the gourdian ecosystem's
convention. The dependency-isolation benefit is real but was judged not
worth the ecosystem-wide inconsistency once every sibling repo (including
graudit and grpolicy, which had their own subpackage/conformance-package
splits) converged on one flat-package shape — see the ecosystem-wide
migration plan for the full rationale. A consumer that only wants the
in-memory backend and truly cannot tolerate the other backends' transitive
dependencies should still use `grcache/memory`... except that subpackage no
longer exists either: this is an accepted tradeoff of the flattening, not
an oversight.

### 2. Networked-backend constructors own their config

`gourdiantoken`'s backend constructors take an already-built client/handle
(`NewRedisTokenRepository(client *redis.Client)`,
`NewPostgresTokenRepository(ctx, pool *pgxpool.Pool)`,
`NewMongoTokenRepository(mongoDB *mongo.Database, transactionsEnabled bool)`).
grcache's networked backends instead each own a `<Backend>Config` struct
and build their own client internally:
`New<Backend>Cache(cfg <Backend>Config) (grcache.Cache, error)`.

**Why:** grcache is meant to be usable as a standalone cache library;
callers shouldn't need to already have a `*redis.Client`/`*pgxpool.Pool`/
`*mongo.Database` sitting around just to use it. This also sidesteps
gourdiantoken's own inconsistency, where Mongo's constructor takes an
extra positional `transactionsEnabled bool` that doesn't fit the otherwise
uniform shape — every grcache backend constructor has the identical
`New<Backend>Cache(cfg Config) (grcache.Cache, error)` signature. Unlike
grnoti's/gourdiantoken's Postgres backend (which take an already-built
`*pgxpool.Pool`, since those repos are meant to share a pool with the rest
of a caller's application), grcache's `NewPostgresCache` always dials its
own pool from a DSN and closes that same pool on `Close()` — consistent
with every other grcache backend's own config-owns-everything shape, not
with gourdiantoken's injection pattern.

### 3. Real local services in tests, not mocks — but different connection details than gourdiantoken

Like gourdiantoken, grcache's tests run against real local Redis/
Postgres/Mongo/Memcached instances — no `miniredis`, no
`testcontainers-go`, no `//go:build integration` tags. Unlike
gourdiantoken, grcache's test suite intentionally uses different
DB indices/database names (Redis DB 14 vs. gourdiantoken's DB 15,
Postgres database `grcache_test` vs. `postgres_db`, a dedicated Mongo
database rather than gourdiantoken's) so both suites can run against the
same local service instances without colliding. Every networked backend's
test factory skips gracefully (`t.Skipf`) rather than failing hard when its
service isn't reachable, matching the rest of the gourdian ecosystem's
convention.

### 4. Latest dependency versions, not version-matched to gourdiantoken

The original plan pinned grcache's Redis/Postgres/Mongo dependencies to the
exact versions gourdiantoken uses, for cross-repo consistency. This was
superseded: grcache now tracks the latest available version of each
dependency it actually uses (see the README's Dependencies table), rather
than matching gourdiantoken's pinned versions. `go.mongodb.org/mongo-driver`
is a partial exception — grcache stays on the v1 module (latest v1.x patch)
rather than migrating to the `/v2` module path, since that would be a
breaking API rewrite out of scope for a routine dependency bump.

## GORM removed: postgres.go now uses pgx/v5 + sqlc

The Postgres backend originally used GORM (two auto-migrated models,
`cacheEntry`/`cacheEntryTag`). It was rewritten to use `pgx/v5` directly
with sqlc-generated queries (`internal/postgresdb`, generated from
`internal/postgresdb/schema.sql` and `internal/postgresdb/queries/cache.sql`
via `sqlc generate` — never hand-edit the generated files), matching the
pattern gourdiantoken and grnoti already established for their own Postgres
backends: an embedded schema (`//go:embed`) applied via
`CREATE TABLE/INDEX IF NOT EXISTS`, serialized by a Postgres advisory lock
(`grcacheSchemaLockKey`, distinct from gourdiantoken's and grnoti's own lock
keys) so concurrent callers building a cache against the same fresh
database don't race on the DDL. `expires_at` changed from GORM's
zero-`time.Time`-means-no-expiry convention to a genuinely nullable
`TIMESTAMPTZ` column (`NULL` means no expiry) — a cleaner mapping now that
GORM's automatic zero-value defaulting is gone, and purely an internal
schema detail with no effect on the public `Cache` interface (still just a
`ttl time.Duration`, 0 meaning no expiry, as always).
`PostgresConfig`'s pool-tuning fields changed from `database/sql`-style
names (`MaxOpenConns`/`MaxIdleConns`) to pgxpool's own (`MaxConns`/
`MinConns`) — an honest reflection of the underlying pool library changing,
not a gratuitous rename.

## Naming: mongostore/ folded into mongo.go

The `mongostore` subpackage was named that way (not simply `mongo`) only to
avoid a Go import-path collision with `go.mongodb.org/mongo-driver/mongo`'s
own default package identifier while it was a *separate importable
package*. Once flattened into the root package, that collision concern no
longer applies (the file is simply `mongo.go`, the concrete type is
`mongoCache`, and the driver import keeps its own `mongo.` qualifier as
before) — this was one of the incidental benefits of the flattening.

## Memcached value-key prefix

Every other backend's key-prefixing was already fully namespaced
(`grcache:val:`/`grcache:tag:` for Redis, `grcache_entries`/
`grcache_entry_tags` for Postgres, etc.), but memcached's actual cache
*value* keys were stored bare/unprefixed — only its tag-list keys carried
the `grcache:tag:` prefix. Fixed by adding `memcachedValuePrefix =
"grcache:val:"`, applied to every value-key read/write
(`Get`/`Set`/`Delete`/`Exists`/`InvalidateTag`'s per-member deletes) while
tag *list membership values* (the actual key strings stored inside a tag's
member list) stay unprefixed raw keys, exactly mirroring Redis's own
convention (its tag Sets also store raw member keys, prefixed only at the
point of dereferencing them back into a value key).

## Logging

Every backend accepts an optional `grcache.Logger` (see `logger.go`) for
diagnostic messages — connection failures, sweep-cycle summaries, shutdown.
The interface is deliberately minimal (`Infof`/`Warnf`/`Errorf`) and defined
in the root package using only stdlib types, so no backend's production
code needs to import a logging library to accept one structurally. `grlog`
is used only in this module's own test suite (`logger_test.go` and each
backend's `TestWithLogger`/`Test<Backend>WithLogger`) to prove a real
`*grlog.Logger` satisfies the interface — it is never a dependency of any
backend's non-test code.

## Backend-specific design notes

- **In-memory**: single `sync.RWMutex`-protected map, not sharded. There
  is no ready-made sharded-map data structure in grlog to reuse (only
  concurrency idioms — see below), so sharding is deferred until
  benchmarks show it's actually needed.
- **TTL sweep shutdown idiom** (`memory`, `postgres`): mirrors grlog's
  `closed atomic.Bool` + `closeChan` + `sync.WaitGroup` idiom, with
  `Close()` guarded by `CompareAndSwap` (memory) or `sync.Once` (postgres,
  matching the other three networked backends) for exactly-once shutdown.
- **Error translation**: backend-native errors (`redis.Nil`,
  `pgx.ErrNoRows`, `mongo.ErrNoDocuments`, `memcache.ErrCacheMiss`) are
  always translated into a grcache sentinel before wrapping — never left
  bare for callers to match against a third-party type. This is the one
  place grcache does not copy gourdiantoken's `ErrX = thirdparty.ErrY`
  re-export pattern: leaking a backend-native error through `Cache` would
  break the backend-agnostic contract the interface exists to provide.
- **Redis: TxPipeline, not Lua/`EVAL`**: gourdiantoken's doc comments claim
  Lua scripting for atomic operations, but no real usage exists in its
  codebase (confirmed by inspection via grep) — its actual mechanism for
  these operations is `Pipeline`/`SETNX` (batched calls), not Lua
  scripting. grcache's Redis `Set`/`InvalidateTag` use go-redis's
  `TxPipeline` (real `MULTI`/`EXEC`), not the plain non-transactional
  `Pipeline` — a v0.1.0 bug used plain `Pipeline` here despite the package
  doc already claiming atomicity; see CHANGELOG.md's `[0.1.1]` entry.
- **Mongo TTL index**: reuses gourdiantoken's confirmed
  `expireAfterSeconds` TTL-index convention — the one backend besides
  Redis where expiry is the database's job, not grcache's. Documents with
  no `expiresAt` field are never touched by the TTL monitor, giving "no
  expiry" for free.
- **No `IsNotFound(err error) bool` helper**: gourdiantoken has no
  precedent for one; callers use `errors.Is` directly against sentinels.
  grcache stays consistent rather than inventing a new ecosystem
  convention unilaterally.

## No separate `conformance/` package

Like grevents'/grpolicy's own former `conformance/` packages, grcache's was
folded into the root package as `contract_cache_test.go`
(`runCacheContract`, run via `TestCache_Contract`'s per-backend subtests).
Unlike those two (which had no real backend-parity concern to prove),
grcache's suite genuinely proves identical behavior across five backend
implementations, so `TestCache_Contract` drives one subtest per backend
(`Memory`/`Redis`/`Memcached`/`Postgres`/`Mongo`) rather than collapsing
into a single flat test — each subtest skips gracefully if that backend's
live service isn't reachable. `RunOption`/`WithBestEffortTagConcurrency`
were kept (renamed to unexported `cacheContractOption`/
`withBestEffortTagConcurrency`) since, unlike grevents'/grpolicy's own
vestigial, zero-defined-options extension points, memcached's relaxed
`ConcurrentTagSet` assertion is a real, currently-used option.
