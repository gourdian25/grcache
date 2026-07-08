# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
