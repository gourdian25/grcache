# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
