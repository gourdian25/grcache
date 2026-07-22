// File: internal/postgresdb/cache.sql.go

// versions:
//   sqlc v1.31.1
// source: cache.sql

package postgresdb

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

const countAllEntries = `-- name: CountAllEntries :one
SELECT COUNT(*) FROM grcache_entries
`

func (q *Queries) CountAllEntries(ctx context.Context) (int64, error) {
	row := q.db.QueryRow(ctx, countAllEntries)
	var count int64
	err := row.Scan(&count)
	return count, err
}

const countLiveEntry = `-- name: CountLiveEntry :one
SELECT COUNT(*) FROM grcache_entries WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())
`

func (q *Queries) CountLiveEntry(ctx context.Context, key string) (int64, error) {
	row := q.db.QueryRow(ctx, countLiveEntry, key)
	var count int64
	err := row.Scan(&count)
	return count, err
}

const deleteEntriesByKeys = `-- name: DeleteEntriesByKeys :execrows
DELETE FROM grcache_entries WHERE key = ANY($1::text[])
`

func (q *Queries) DeleteEntriesByKeys(ctx context.Context, dollar_1 []string) (int64, error) {
	result, err := q.db.Exec(ctx, deleteEntriesByKeys, dollar_1)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

const deleteEntriesByTag = `-- name: DeleteEntriesByTag :execrows
DELETE FROM grcache_entries WHERE key IN (SELECT key FROM grcache_entry_tags WHERE tag = $1)
`

func (q *Queries) DeleteEntriesByTag(ctx context.Context, tag string) (int64, error) {
	result, err := q.db.Exec(ctx, deleteEntriesByTag, tag)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

const deleteEntry = `-- name: DeleteEntry :execrows
DELETE FROM grcache_entries WHERE key = $1
`

func (q *Queries) DeleteEntry(ctx context.Context, key string) (int64, error) {
	result, err := q.db.Exec(ctx, deleteEntry, key)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

const deleteEntryTagsByKey = `-- name: DeleteEntryTagsByKey :exec
DELETE FROM grcache_entry_tags WHERE key = $1
`

func (q *Queries) DeleteEntryTagsByKey(ctx context.Context, key string) error {
	_, err := q.db.Exec(ctx, deleteEntryTagsByKey, key)
	return err
}

const deleteEntryTagsByKeys = `-- name: DeleteEntryTagsByKeys :exec
DELETE FROM grcache_entry_tags WHERE key = ANY($1::text[])
`

func (q *Queries) DeleteEntryTagsByKeys(ctx context.Context, dollar_1 []string) error {
	_, err := q.db.Exec(ctx, deleteEntryTagsByKeys, dollar_1)
	return err
}

const deleteEntryTagsByTag = `-- name: DeleteEntryTagsByTag :exec
DELETE FROM grcache_entry_tags WHERE tag = $1
`

func (q *Queries) DeleteEntryTagsByTag(ctx context.Context, tag string) error {
	_, err := q.db.Exec(ctx, deleteEntryTagsByTag, tag)
	return err
}

const getEntry = `-- name: GetEntry :one
SELECT value, expires_at FROM grcache_entries WHERE key = $1
`

type GetEntryRow struct {
	Value     []byte             `db:"value" json:"value"`
	ExpiresAt pgtype.Timestamptz `db:"expires_at" json:"expires_at"`
}

func (q *Queries) GetEntry(ctx context.Context, key string) (GetEntryRow, error) {
	row := q.db.QueryRow(ctx, getEntry, key)
	var i GetEntryRow
	err := row.Scan(&i.Value, &i.ExpiresAt)
	return i, err
}

const insertEntryTag = `-- name: InsertEntryTag :exec
INSERT INTO grcache_entry_tags (key, tag) VALUES ($1, $2)
`

type InsertEntryTagParams struct {
	Key string `db:"key" json:"key"`
	Tag string `db:"tag" json:"tag"`
}

func (q *Queries) InsertEntryTag(ctx context.Context, arg InsertEntryTagParams) error {
	_, err := q.db.Exec(ctx, insertEntryTag, arg.Key, arg.Tag)
	return err
}

const listExpiredKeys = `-- name: ListExpiredKeys :many
SELECT key FROM grcache_entries WHERE expires_at IS NOT NULL AND expires_at <= now()
`

func (q *Queries) ListExpiredKeys(ctx context.Context) ([]string, error) {
	rows, err := q.db.Query(ctx, listExpiredKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		items = append(items, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const upsertEntry = `-- name: UpsertEntry :exec

INSERT INTO grcache_entries (key, value, expires_at, created_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at
`

type UpsertEntryParams struct {
	Key       string             `db:"key" json:"key"`
	Value     []byte             `db:"value" json:"value"`
	ExpiresAt pgtype.Timestamptz `db:"expires_at" json:"expires_at"`
}

// File: internal/postgresdb/queries/cache.sql
func (q *Queries) UpsertEntry(ctx context.Context, arg UpsertEntryParams) error {
	_, err := q.db.Exec(ctx, upsertEntry, arg.Key, arg.Value, arg.ExpiresAt)
	return err
}
