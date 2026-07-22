-- File: internal/postgresdb/queries/cache.sql

-- name: UpsertEntry :exec
INSERT INTO grcache_entries (key, value, expires_at, created_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at;

-- name: DeleteEntryTagsByKey :exec
DELETE FROM grcache_entry_tags WHERE key = $1;

-- name: InsertEntryTag :exec
INSERT INTO grcache_entry_tags (key, tag) VALUES ($1, $2);

-- name: GetEntry :one
SELECT value, expires_at FROM grcache_entries WHERE key = $1;

-- name: DeleteEntry :execrows
DELETE FROM grcache_entries WHERE key = $1;

-- name: CountLiveEntry :one
SELECT COUNT(*) FROM grcache_entries WHERE key = $1 AND (expires_at IS NULL OR expires_at > now());

-- name: DeleteEntriesByTag :execrows
DELETE FROM grcache_entries WHERE key IN (SELECT key FROM grcache_entry_tags WHERE tag = $1);

-- name: DeleteEntryTagsByTag :exec
DELETE FROM grcache_entry_tags WHERE tag = $1;

-- name: CountAllEntries :one
SELECT COUNT(*) FROM grcache_entries;

-- name: ListExpiredKeys :many
SELECT key FROM grcache_entries WHERE expires_at IS NOT NULL AND expires_at <= now();

-- name: DeleteEntriesByKeys :execrows
DELETE FROM grcache_entries WHERE key = ANY($1::text[]);

-- name: DeleteEntryTagsByKeys :exec
DELETE FROM grcache_entry_tags WHERE key = ANY($1::text[]);
