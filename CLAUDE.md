# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

grcache (`github.com/gourdian25/grcache`) is a generic, backend-agnostic caching abstraction for the gourdian ecosystem — the same `interface → multiple backend implementations` pattern gourdiantoken uses for token storage, applied to general-purpose caching. Unlike gourdiantoken and grlog (both flat, single-package repos), grcache uses **one subpackage per backend** (`grcache/memory`, `grcache/redis`, `grcache/memcached`, `grcache/postgres`, `grcache/mongo`) so that only the root package and `grcache/memory` are dependency-free — importing `grcache/redis` alone doesn't pull GORM or the Mongo driver into a consumer's build. See `docs/architecture.md` for the full reasoning behind this and grcache's other deliberate divergences from sibling conventions.

## Commands

```sh
make test             # go test -cover ./...
make race             # go test -race ./...  (mandatory before any commit touching backend code)
make coverage-check   # verify every package independently meets 80% coverage
make bench            # go test -bench=. -benchmem -benchtime=10s ./...
make lint             # golangci-lint run ./...
make fmt              # gofmt
make vet              # go vet ./...
make release VERSION=vX.Y.Z   # tag, push, goreleaser release --clean
make goreleaser-check         # dry run: goreleaser check + --snapshot --clean
```

Run a single test: `go test -run TestSetThenGet ./memory/...` (or any backend's directory). Run one conformance scenario across a backend: `go test -run TestConformance/SetThenGet ./redis/...`.

### Backend tests require live local services

Every backend except `memory` needs a real running service — no mocks, no `miniredis`, no `testcontainers-go`, mirroring gourdiantoken's own testing philosophy. Connection settings are deliberately different from gourdiantoken's own test suite (different Redis DB index, different Postgres/Mongo database names) so both repos' tests can run against the same local instances without colliding — this is now formalized workspace-wide: grnoti, graudit, grcache, and gourdiantoken all share one running Postgres/Redis/Mongo/Memcached instance apiece, each repo using its own database/keyspace/DB-index:

```sh
docker run -d --name gourdian-redis      -p 6379:6379  redis:7 --requirepass redis_password
docker run -d --name gourdian-postgres   -p 5432:5432  -e POSTGRES_USER=postgres_user -e POSTGRES_PASSWORD=postgres_password postgres:16
docker run -d --name gourdian-memcached  -p 11211:11211 memcached:1.6

# Mongo requires a --keyFile once --replSet is combined with auth, even
# single-node — see graudit's CLAUDE.md for the full keyfile-generation
# steps this repo's Mongo container now shares with graudit/gourdiantoken.
docker volume create gourdian-mongo-keyfile
docker run --rm -v gourdian-mongo-keyfile:/keyfile-dir mongo:7 bash -c \
  "openssl rand -base64 756 > /keyfile-dir/mongo-keyfile && chmod 400 /keyfile-dir/mongo-keyfile && chown 999:999 /keyfile-dir/mongo-keyfile"
docker run -d --name gourdian-mongo-auth -p 27018:27017 \
  -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD=mongo_password \
  -v gourdian-mongo-keyfile:/etc/mongo-keyfile-dir \
  mongo:7 --replSet rs0 --keyFile /etc/mongo-keyfile-dir/mongo-keyfile
docker exec gourdian-mongo-auth mongosh -u root -p mongo_password \
  --authenticationDatabase admin --eval 'rs.initiate()'
```

Then, once, create the Postgres test database: `createdb -U postgres_user -h localhost grcache_test` (or `psql ... -c "CREATE DATABASE grcache_test;"`). Redis and Mongo need no equivalent setup step — Redis DB 14 and the Mongo `grcache_test` database are created implicitly on first write. grcache's `mongostore` backend doesn't use transactions or replica-set-specific behavior, so connecting to the now-replica-set-enabled shared Mongo container needs no code change — a plain client connection to one member of a replica set works the same as connecting to a standalone instance for ordinary CRUD.

## Architecture

- **Root package (`grcache`)** — `cache.go` defines the `Cache` interface (`Get`, `Set`, `Delete`, `Exists`, `InvalidateTag`, `Stats`, `Close`) and the `Stats` struct; `errors.go` defines the sentinel errors (`ErrKeyNotFound`, `ErrCacheUnavailable`, `ErrInvalidTTL`, `ErrClosed`); `logger.go` defines the optional `Logger` interface (`Infof`/`Warnf`/`Errorf`) plus `NopLogger`/`OrNop`; `docs.go` is the package-level doc comment (no code). All three files are stdlib-only — `go list -deps .` should never show a non-stdlib import.
- **One subpackage per backend**, each implementing `Cache` on its own concrete type and taking a `<Backend>Config` struct in its constructor (`New<Backend>Cache(cfg Config) (grcache.Cache, error)` — the same shape for every backend, unlike gourdiantoken's constructors, which vary):
  - `memory/` — `sync.RWMutex`-protected map + tag index (`map[string]map[string]struct{}`), both behind the same mutex to avoid a race window between value and tag state. A background sweep goroutine (mirrors grlog's `closed atomic.Bool` + `closeChan` + `sync.WaitGroup` shutdown idiom) reclaims expired entries; `Get`/`Exists` also check expiry lazily as the actual correctness mechanism — the sweep is a memory-reclamation optimization only.
  - `redis/` — tags stored as Redis Sets (`SADD tag:<tag> <key>`); `Set`/`InvalidateTag` use `TxPipeline` (real `MULTI`/`EXEC`) for actual atomicity — a v0.1.0 bug used the plain non-transactional `Pipeline` here despite the package doc already claiming atomicity; fixed in v0.1.1, see CHANGELOG.md. No Lua/`EVAL` — gourdiantoken's docs claim Lua scripting but its actual code only uses `Pipeline`/`SETNX` (batched calls, not verified as transactional in that repo).
  - `memcached/` — tags emulated via a newline-delimited member list stored under its own key, updated by read-modify-write on every tagged `Set`. This is explicitly best-effort/eventually-consistent (concurrent `Set` calls on the same tag can race and drop a member) — see the package doc comment, `TestTagListRaceIsDocumentedNotFixed`, and `conformance.WithBestEffortTagConcurrency`. Also note `expirationSeconds`: memcached only supports second-granularity TTLs, so any positive sub-second `ttl` rounds up to 1 second rather than truncating to 0 (which would mean "never expire"); ttls beyond the 30-day relative-expiration cutoff convert to an absolute Unix timestamp (see `TestLongTTLConversion`).
  - `postgres/` — via GORM, two tables: `grcache_entries` (key/value/expires_at) and `grcache_entry_tags` (a join table, composite-indexed on tag+key). Postgres has no native expiry at all, so a background sweep goroutine is the *only* reclamation mechanism (not belt-and-suspenders like Redis/Mongo) — `Get`/`Exists`'s lazy check is what keeps this correct between sweeps. Intended for test/dev/CI environments without Redis/memcached available, not as a production alternative to Redis.
  - `mongo/` — tags live directly as an array field on the same document (no join table needed, unlike postgres). A TTL index (`expireAfterSeconds: 0`) gives native, database-managed expiry; documents with no `expiresAt` field (ttl=0) are simply never touched by the TTL monitor. Same intended use case as postgres above: test/dev/CI without Redis/memcached.
- **`conformance/`** — a shared behavioral test suite (`conformance.Run(t, newCache, opts...)`) that every backend's own `_test.go` file calls with its own constructor closure. It imports only the root `grcache` package, never a backend subpackage, which is what avoids an import cycle in the subpackage-per-backend layout — each backend's test file imports `conformance` sideways instead of a shared suite importing every backend. `ConcurrentTagSet` asserts `InvalidateTag` removes exactly N keys after N concurrent tagged `Set` calls on distinct keys — strict by default; only `memcached`'s test file passes `conformance.WithBestEffortTagConcurrency()` to relax that to a bounded (not exact) count, matching its documented best-effort tag storage. Also provides `PopulateTagged` (benchmark setup helper) and `RecordingLogger` (a `Logger` that records every call, used by each backend's `TestWithLogger`).
- **Logging** — every backend's `Config` (or, for `memory`, `WithLogger(...)`) accepts an optional `grcache.Logger`. The interface is deliberately stdlib-only so no backend's production code needs to import a logging library structurally; `grlog` is used only in this module's own test files (`logger_test.go`, each backend's `TestWithLogger`) to prove `*grlog.Logger` satisfies the interface, and is never a dependency of any backend's non-test code.

## Testing conventions

Real local services, no mocks, `-race` mandatory (`memory`'s concurrent map access and every backend's `ConcurrentAccess` conformance scenario depend on it). Each backend's `_test.go` file supplies a `newCache` closure to `conformance.Run` and adds backend-specific tests only for things that can't be expressed generically (connection failures, tag-list races, TTL-index timing windows). Coverage is checked per-package, not just in aggregate — `make coverage-check` fails if any single package drops below 80%.

## Repo conventions

- Every `.go` file (and the `Makefile`) starts with a `// File: <relative-path>` header line, maintained by the `bark` tool (`.bark.toml`).
- Deliberate divergences from gourdiantoken's/grlog's conventions (subpackage layout, config-struct constructors instead of client injection, dependency versions tracked to latest rather than pinned to gourdiantoken's) are recorded in `docs/architecture.md` — check there before "fixing" something that looks inconsistent with a sibling repo.
- `docs/plan/grcache-plan.md` is the original scope/spec document this repo was built from; treat it as historical context, not a live source of truth for current behavior (the actual code and this file are authoritative).
