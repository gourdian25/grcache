// File: internal/postgresdb/querier.go

// versions:
//   sqlc v1.31.1

package postgresdb

import (
	"context"
)

type Querier interface {
	CountAllEntries(ctx context.Context) (int64, error)
	CountLiveEntry(ctx context.Context, key string) (int64, error)
	DeleteEntriesByKeys(ctx context.Context, dollar_1 []string) (int64, error)
	DeleteEntriesByTag(ctx context.Context, tag string) (int64, error)
	DeleteEntry(ctx context.Context, key string) (int64, error)
	DeleteEntryTagsByKey(ctx context.Context, key string) error
	DeleteEntryTagsByKeys(ctx context.Context, dollar_1 []string) error
	DeleteEntryTagsByTag(ctx context.Context, tag string) error
	GetEntry(ctx context.Context, key string) (GetEntryRow, error)
	InsertEntryTag(ctx context.Context, arg InsertEntryTagParams) error
	ListExpiredKeys(ctx context.Context) ([]string, error)
	// File: internal/postgresdb/queries/cache.sql
	UpsertEntry(ctx context.Context, arg UpsertEntryParams) error
}

var _ Querier = (*Queries)(nil)
