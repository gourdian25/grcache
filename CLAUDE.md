# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

grcache (`github.com/gourdian25/grcache`) is a generic, backend-agnostic caching abstraction for the gourdian ecosystem — the same `interface → multiple backend implementations` pattern gourdiantoken uses for token storage, applied to general-purpose caching. It is a **flat, single-package library** (`package grcache` at the repo root) — no subpackages — matching every other repo in the gourdian ecosystem's convention (grcache originally split each backend into its own importable subpackage to keep unused client libraries out of a consumer's build; that layout was reversed for ecosystem-wide consistency, see `docs/architecture.md` §1). There is nothing to build or run, only lint and test.

## Commands

```sh
make test             # go test -cover ./...
make race             # go test -race ./...  (mandatory before any commit touching backend code)
make coverage-check   # verify the root package meets 95% coverage
make bench            # go test -bench=. -benchmem -benchtime=10s ./...
make lint             # golangci-lint run ./...
make fmt              # gofmt
make vet              # go vet ./...
make docker-up        # start the shared Postgres/Redis/Mongo/Memcached test containers (idempotent)
make docker-down      # stop those containers (state preserved for a fast restart)
make release VERSION=vX.Y.Z   # tag, push, goreleaser release --clean
make goreleaser-check         # dry run: goreleaser check + --snapshot --clean
```

Run a single test: `go test -run TestSetThenGet .`. Run one contract scenario across every backend: `go test -run TestCache_Contract/Redis/SetThenGet .` (or swap `Redis` for `Memory`/`Memcached`/`Postgres`/`Mongo`).

### Backend tests require live local services

Every backend except the in-memory one needs a real running service — no mocks, no `miniredis`, no `testcontainers-go`, mirroring gourdiantoken's own testing philosophy. Connection settings are deliberately different from gourdiantoken's own test suite (different Redis DB index, different Postgres/Mongo database names) so both repos' tests can run against the same local instances without colliding — this is formalized workspace-wide: grnoti, graudit, grcache, and gourdiantoken all share one running Postgres/Redis/Mongo/Memcached instance apiece, each repo using its own database/keyspace/DB-index. Run `make docker-up` to start (or reuse already-running) shared containers — see the Makefile for the exact idempotent commands, or start them by hand per the commands there.

Once, create the Postgres test database if `make docker-up` hasn't already: `createdb -U postgres_user -h localhost grcache_test` (or `psql ... -c "CREATE DATABASE grcache_test;"`). Redis and Mongo need no equivalent setup step — Redis DB 14 and the Mongo `grcache_test` database are created implicitly on first write; the Postgres schema (`grcache_entries`/`grcache_entry_tags`) is applied automatically by `NewPostgresCache` (`CREATE TABLE/INDEX IF NOT EXISTS`, serialized by a Postgres advisory lock — see `postgres.go`), no manual migration needed.

Every networked backend's test factory (`newRedisCache`, `newMemcachedCache`, `newPostgresCache`, `newMongoCache` in their respective `*_test.go` files) skips gracefully (`t.Skipf`) rather than failing hard when its service isn't reachable, matching the rest of the gourdian ecosystem's convention — to iterate on logic that doesn't touch storage, scope test runs to the in-memory backend (e.g. `go test -run TestCache_Contract/Memory .`) to avoid needing any service up at all.

## Architecture

Everything is in package `grcache`, one file per backend plus the shared interface/error/logger files:

- **`cache.go`** — the `Cache` interface (`Get`, `Set`, `Delete`, `Exists`, `InvalidateTag`, `Stats`, `Close`) and the `Stats` struct.
- **`errors.go`** — sentinel errors (`ErrKeyNotFound`, `ErrCacheUnavailable`, `ErrInvalidTTL`, `ErrClosed`), usable with `errors.Is`. No `IsX(err) bool` helpers, by convention.
- **`logger.go`** — the optional `Logger` interface (`Infof`/`Warnf`/`Errorf`) plus `NopLogger`/`OrNop`. grcache itself never imports grlog — it's a test-only dependency of this module (see `logger_test.go` and each backend's own `TestWithLogger`).
- **`docs.go`** — package-level doc comment only, no code.
- **One file per backend**, each defining its own unexported concrete type behind the shared `Cache` interface and a `New<Backend>Cache(cfg <Backend>Config) (Cache, error)` constructor — the same shape for every backend:
  - `memory.go` (`memoryCache`) — `sync.RWMutex`-protected map + tag index (`map[string]map[string]struct{}`), both behind the same mutex to avoid a race window between value and tag state. A background sweep goroutine (mirrors grlog's `closed atomic.Bool` + `closeChan` + `sync.WaitGroup` shutdown idiom) reclaims expired entries; `Get`/`Exists` also check expiry lazily as the actual correctness mechanism — the sweep is a memory-reclamation optimization only.
  - `redis.go` (`redisCache`) — tags stored as Redis Sets (`SADD tag:<tag> <key>`); `Set`/`InvalidateTag` use `TxPipeline` (real `MULTI`/`EXEC`) for actual atomicity. No Lua/`EVAL`.
  - `memcached.go` (`memcachedCache`) — tags emulated via a newline-delimited member list stored under its own key, updated by read-modify-write on every tagged `Set`. This is explicitly best-effort/eventually-consistent (concurrent `Set` calls on the same tag can race and drop a member) — see the file's package doc comment, `TestTagListRaceIsDocumentedNotFixed`, and `withBestEffortTagConcurrency`. Cache-value keys and tag-list keys are both prefixed (`grcache:val:`/`grcache:tag:`) — a real gap fixed in this pass (previously only tag-list keys were prefixed). Also note `expirationSeconds`: memcached only supports second-granularity TTLs, so any positive sub-second `ttl` rounds up to 1 second rather than truncating to 0 (which would mean "never expire"); ttls beyond the 30-day relative-expiration cutoff convert to an absolute Unix timestamp.
  - `postgres.go` (`postgresCache`) — pgx/v5 + sqlc-generated queries (see `internal/postgresdb`, generated from `internal/postgresdb/schema.sql`/`queries/cache.sql` via `sqlc generate` — never hand-edit generated files), replacing an earlier GORM implementation. Two tables: `grcache_entries` (key/value/nullable expires_at) and `grcache_entry_tags` (a join table). Schema is applied on connect via an embedded `//go:embed` schema string, serialized by a Postgres advisory lock (`grcacheSchemaLockKey`). Postgres has no native expiry at all, so a background sweep goroutine is the *only* reclamation mechanism (not belt-and-suspenders like Redis/Mongo) — `Get`/`Exists`'s lazy check is what keeps this correct between sweeps. Intended for test/dev/CI environments without Redis/memcached available, not as a production alternative to Redis.
  - `mongo.go` (`mongoCache`) — tags live directly as an array field on the same document (no join table needed, unlike postgres). A TTL index (`expireAfterSeconds: 0`) gives native, database-managed expiry; documents with no `expiresAt` field (ttl=0) are simply never touched by the TTL monitor. Same intended use case as postgres above: test/dev/CI without Redis/memcached.
- **`contract_cache_test.go`** — the shared behavioral test suite (`runCacheContract`, run via `TestCache_Contract`'s per-backend subtests: `Memory`/`Redis`/`Memcached`/`Postgres`/`Mongo`, each skipping gracefully if its live service isn't reachable). This was originally a separate, publicly-importable `conformance` package — folded into the root package's own tests for ecosystem consistency; see `docs/architecture.md`'s closing section. `ConcurrentTagSet` asserts `InvalidateTag` removes exactly N keys after N concurrent tagged `Set` calls on distinct keys — strict by default; only the memcached subtest passes `withBestEffortTagConcurrency()` to relax that to a bounded (not exact) count, matching its documented best-effort tag storage. `populateTagged` (benchmark setup helper, used by `cache_bench_test.go`) and `recordingLogger` (a `Logger` that records every call, used by each backend's `TestWithLogger`) also live here / in `recording_logger_test.go`.
- **Per-backend test files** (`memory_test.go`, `redis_test.go`, `memcached_test.go`, `postgres_test.go`, `mongo_test.go`) — each supplies its own `new<Backend>Cache`/`new<Backend>CacheForTest` factory to the contract suite and adds backend-specific tests for anything that can't be expressed generically (connection failures, tag-list races, TTL-index timing windows, schema/index idempotency).
- **`internal_coverage_test.go`** (white-box, still `package grcache`) — reaches into each networked backend's unexported concrete type (`redisCache.client`, `postgresCache.pool`, `mongoCache.client`, etc.) to close/disconnect it directly while the `Cache` object itself still believes it's open, deterministically covering every method's `ErrCacheUnavailable`-wrapping branch — the same technique used throughout the gourdian ecosystem (see gourdiantoken's own repository coverage tests). Not expressible from `contract_cache_test.go`/the per-backend `*_test.go` files (`package grcache_test`, external) since those can't see unexported fields.

## Testing conventions

Real local services, no mocks, `-race` mandatory (the in-memory backend's concurrent map access and every backend's `ConcurrentAccess`/`ConcurrentTagSet` contract scenarios depend on it). Each backend's own `*_test.go` file supplies a `new<Backend>Cache` closure to `TestCache_Contract`'s corresponding subtest and adds backend-specific tests only for things that can't be expressed generically. Coverage is checked on the root package only (`.`, not `./...` — `example/` is a runnable demo, not library code under test) — `make coverage-check` fails if it drops below 95%.

## Repo conventions

- Every `.go` file (and the `Makefile`) starts with a `// File: <relative-path>` header line, maintained by the `bark` tool (`.bark.toml`).
- Deliberate divergences from gourdiantoken's/grlog's conventions (config-struct constructors instead of client injection for most backends, dependency versions tracked to latest rather than pinned to gourdiantoken's) are recorded in `docs/architecture.md` — check there before "fixing" something that looks inconsistent with a sibling repo.
- `docs/plan/grcache-plan.md` is the original scope/spec document this repo was built from; treat it as historical context, not a live source of truth for current behavior (the actual code and this file are authoritative).
