// File: internal/postgresdb/models.go

// versions:
//   sqlc v1.31.1

package postgresdb

import (
	"github.com/jackc/pgx/v5/pgtype"
)

type GrcacheEntry struct {
	Key       string             `db:"key" json:"key"`
	Value     []byte             `db:"value" json:"value"`
	ExpiresAt pgtype.Timestamptz `db:"expires_at" json:"expires_at"`
	CreatedAt pgtype.Timestamptz `db:"created_at" json:"created_at"`
}

type GrcacheEntryTag struct {
	Key string `db:"key" json:"key"`
	Tag string `db:"tag" json:"tag"`
}
