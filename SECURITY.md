# Security Policy

## Supported Versions

Security fixes are applied to the latest released minor version.

| Version | Supported |
|---------|-----------|
| 0.1.x   | ✅        |
| < 0.1   | ❌        |

## Reporting a Vulnerability

Please report suspected vulnerabilities privately via
[GitHub Security Advisories](https://github.com/gourdian25/grcache/security/advisories/new)
rather than opening a public issue.

Include:

- A description of the issue and its impact
- Steps or a proof-of-concept to reproduce
- Affected version(s)

You can expect an acknowledgment within a week. Once a fix is available, the
advisory will be published together with a patched release.

## Scope Notes

grcache is a caching abstraction over Redis, Postgres, Mongo, and memcached;
it stores whatever bytes the caller gives it and does not interpret them.
The most relevant security considerations for users are:

- **No encryption at rest or in transit is provided by grcache itself.**
  TLS to a backend (Redis/Postgres/Mongo all support it) and encryption of
  values before calling `Set` are the caller's responsibility if the cached
  data is sensitive.
- **Cache poisoning is the caller's responsibility to prevent.** grcache
  does not validate or sanitize keys/values; an attacker who can influence
  what gets cached under a given key (e.g. via an unvalidated upstream
  response) can poison reads for every subsequent `Get` of that key until
  it expires or is invalidated.
- **Tag invalidation is trusted, not authenticated.** Any caller with a
  `Cache` handle can invalidate any tag; grcache does not implement
  per-tag access control. Scope tags carefully (e.g. `tenant:<id>`) and
  don't expose raw tag names to untrusted input if cross-tenant
  invalidation would be a problem.
- **`memcached`'s tag storage is explicitly best-effort** (see README/
  docs.go) — concurrent tagged `Set` calls on the same tag can race and
  drop a member, which is a correctness caveat, not a security one, but is
  worth knowing if tag-based invalidation is load-bearing for access
  control in your application (it should not be).
