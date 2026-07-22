# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Ecosystem-wide Stage 2 pass: flattened to a single package, GORM removed,
a couple of real bug fixes, and coverage raised. Contains **breaking
changes** (allowed pre-1.0).

### Changed

- **Breaking:** flattened from one root package plus a subpackage per
  backend (`grcache/memory`, `grcache/redis`, `grcache/memcached`,
  `grcache/postgres`, `grcache/mongostore`) into a single flat package,
  matching every other repo in the gourdian ecosystem's convention (see
  `docs/architecture.md` §1). Every backend's `New<Backend>Cache`
  constructor and `<Backend>Config` type now live directly in
  `github.com/gourdian25/grcache` — update imports from e.g.
  `github.com/gourdian25/grcache/redis` + `redis.NewRedisCache(...)` to
  `github.com/gourdian25/grcache` + `grcache.NewRedisCache(...)`. This also
  resolves the reason `grcache/mongo` was renamed to `grcache/mongostore`
  in `v0.2.0` (avoiding a package-name collision with
  `go.mongodb.org/mongo-driver/mongo`) — as a file within one flat package
  rather than a separate importable package, `mongo.go` no longer
  collides with anything, so the file (and its internal error-message
  prefix) reverts to the shorter, clearer `mongo` name.
- **Breaking:** the PostgreSQL backend (`NewPostgresCache`) no longer uses
  GORM — rewritten on `pgx/v5` with sqlc-generated queries (see
  `internal/postgresdb`), matching gourdiantoken's and grnoti's own
  Postgres backend pattern. `PostgresConfig`'s pool-tuning fields renamed
  from `database/sql`-style (`MaxOpenConns`/`MaxIdleConns`) to pgxpool's
  own (`MaxConns`/`MinConns`), an honest reflection of the underlying pool
  library changing.
- Folded the standalone `conformance` package into the root package as
  `contract_cache_test.go` (`runCacheContract`, run via
  `TestCache_Contract`'s per-backend subtests), matching the rest of the
  gourdian ecosystem's convention.
- Every networked backend's test factory now skips gracefully (`t.Skipf`)
  rather than failing hard when its live service isn't reachable, matching
  the rest of the gourdian ecosystem's convention.
- `make coverage-check`'s threshold raised from 80% to 95%, and it now
  checks only the root package (previously iterated over one directory
  per backend subpackage, including a stale `./mongo` entry that never
  matched the actual `mongostore` directory name and so silently reported
  "no coverage output" for it on every run — moot now that there's only
  one package to measure).

### Fixed

- The memcached backend's cache-*value* keys are now namespaced
  (`grcache:val:`), matching every other backend's key-prefixing
  convention. Previously only memcached's tag-list keys carried the
  `grcache:tag:` prefix; the actual cached values themselves were stored
  under bare, unprefixed keys — no defense-in-depth against a same-named
  key from another memcached user of the same server/pool.

### Testing

- Coverage raised to 95.6% on the root package (previously ranging
  84.7%-96.7% per backend subpackage, per-package rather than aggregate),
  closing gaps in every backend's `ErrCacheUnavailable`-wrapping branches
  (via a new white-box `internal_coverage_test.go` that closes/disconnects
  each backend's underlying client or pool directly, the same technique
  used throughout the gourdian ecosystem — see gourdiantoken's own
  repository coverage tests), a shared `InvalidTTL` contract scenario
  covering all five backends at once (previously only tested against the
  in-memory backend), and a handful of per-backend constructor/schema/
  index-creation edge cases.

## [0.2.0] - 2026-07-10

Ecosystem-alignment pass ahead of `grauth`. Contains a **breaking change**
(allowed pre-1.0).

### Changed

- **Breaking:** `grcache/mongo` renamed to `grcache/mongostore`. The bare
  `mongo` package name collided with the upstream driver's own package
  (`go.mongodb.org/mongo-driver/mongo` also declares `package mongo`),
  forcing every consumer that imports both in the same file (as `grauth`
  will need to) to alias one manually. Update imports from
  `github.com/gourdian25/grcache/mongo` to
  `github.com/gourdian25/grcache/mongostore`; the package's exported API
  (`NewMongoCache`, `MongoConfig`, ...) is otherwise unchanged.
- `go.mod`'s `go` directive raised from `1.25.0` to `1.26.4`, aligning with
  the rest of the gourdian25 ecosystem.
- README: added `grpolicy` to the ecosystem section, and fixed a stale
  Roadmap line claiming distributed invalidation pub/sub is blocked on
  `grevents` "exist[ing]" — grevents has been released since v0.1.0.
- Bumped `github.com/gourdian25/grlog` to `v0.1.1`.

### Added

- `SECURITY.md` (previously missing).

## [0.1.1] - 2026-07-08

A post-release verification pass against `v0.1.0` (checked against actual
code and live Redis/Postgres/Mongo/memcached containers, not just re-reading
source) found one real correctness bug in the Redis backend, plus test and
documentation gaps. **If you adopted `v0.1.0`, read the Fixed section below.**

### Fixed

- **`grcache/redis`: `Set` and `InvalidateTag` now use go-redis's
  `TxPipeline` (real `MULTI`/`EXEC`), not the plain non-transactional
  `Pipeline`.** In `v0.1.0`, a connection failure mid-batch could leave a
  value written without all of its tag memberships applied (or, in
  `InvalidateTag`, some tag members deleted but not others) — despite the
  package doc comment already claiming these operations were atomic. That
  claim did not actually hold until this release. Benchmarked: the
  `MULTI`/`EXEC` overhead versus the previous plain `Pipeline` is within
  measurement noise (see README's Benchmarks section) — go-redis sends the
  whole batch in one round trip either way.

### Added

- Conformance suite: new `ConcurrentTagSet` scenario — concurrently `Set`s
  N distinct keys under one shared tag and asserts `InvalidateTag` removes
  exactly N for backends with a real transactional/atomic tag-index write
  path (memory, redis, postgres, mongo). A new `conformance.RunOption` +
  `WithBestEffortTagConcurrency()` lets `grcache/memcached` opt into a
  bounded-but-not-exact assertion instead, matching its documented
  best-effort tag storage.
  - Note on scope: this scenario validates concurrent tag-index
    correctness under normal operation. It does not (and, without fault
    injection such as killing the connection mid-batch, cannot easily)
    reproduce the specific "connection drops mid-pipeline" failure mode
    the `TxPipeline` fix addresses — that guarantee comes from `MULTI`/
    `EXEC`'s documented semantics, not from this test. Confirmed manually
    during the fix's verification: this test still passes if the fix is
    reverted, since it doesn't inject a connection failure.
- `grcache/memcached`: new `TestLongTTLConversion` regression test for TTLs
  beyond the 30-day relative-expiration cutoff (`expirationSeconds`'s
  absolute-Unix-timestamp conversion path) — the code was already correct,
  but nothing previously caught a regression here.

### Changed

- Documentation: reframed `grcache/postgres` and `grcache/mongo`'s
  rationale in README.md, CLAUDE.md, docs.go, and both packages' own doc
  comments. These two backends exist for test/dev/CI environments without
  Redis or memcached available — not as a production alternative to Redis.
  (Confirmed via `grauth`/`graudit` being empty directories and
  `gourdianerp` not existing on disk: no current consumer requires
  Postgres or Mongo specifically. All five backends are being kept.)
- Softened two documentation overclaims found during a broader audit
  (grepping README/CLAUDE.md/docs/architecture.md/docs.go for
  "atomic"/"guarantee"/"consistent"/"safe under concurrency" and checking
  each against the code): `docs/architecture.md`'s characterization of
  *gourdiantoken's* (not grcache's) `Pipeline`/`SETNX` usage as achieving
  "atomicity" was never independently verified for that repo; and
  `docs.go`'s/`conformance.go`'s "guaranteeing behavioral parity" phrasing
  for the conformance suite, which the `ConcurrentTagSet` gap above
  demonstrated was only ever true for what the suite actually covers.
- README's Redis benchmark numbers updated to reflect the `TxPipeline` fix.

## [0.1.0] - 2026-07-08

Initial release: a generic, backend-agnostic caching abstraction for the
gourdian ecosystem.

### Added

- Core `Cache` interface (`Get`, `Set`, `Delete`, `Exists`, `InvalidateTag`,
  `Stats`, `Close`) with sentinel errors (`ErrKeyNotFound`,
  `ErrCacheUnavailable`, `ErrInvalidTTL`, `ErrClosed`).
- Five backends, each implementing `Cache` in its own subpackage:
  - `grcache/memory` — zero-dependency, in-process, TTL sweep goroutine.
  - `grcache/redis` — Redis Sets for tag storage, pipelined invalidation.
  - `grcache/memcached` — best-effort list-based tag emulation.
  - `grcache/postgres` — GORM-backed, join-table tag storage.
  - `grcache/mongo` — native TTL index, embedded array tag storage.
- Shared `conformance` test suite run against every backend for behavioral
  parity.
- Per-backend benchmarks for `Get`/`Set`/`InvalidateTag` at 10/1k/100k tag
  cardinality.
- Optional `grcache.Logger` interface for diagnostic logging, satisfied
  structurally by `*grlog.Logger` with no adapter required.
- `make coverage-check` enforcing an 80% minimum per package, matching
  gourdiantoken's convention.
