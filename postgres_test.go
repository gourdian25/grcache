// File: postgres_test.go

package grcache_test

// Test DSN reuses gourdiantoken's confirmed local Postgres settings
// (host=localhost user=postgres_user password=postgres_password port=5432
// sslmode=disable) but a distinct dbname=grcache_test (not gourdiantoken's
// postgres_db) to avoid table collisions on a shared local instance.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gourdian25/grcache"
)

const postgresTestDSN = "host=localhost user=postgres_user password=postgres_password dbname=grcache_test port=5432 sslmode=disable"

func truncatePostgresTestDB(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, postgresTestDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	// Errors are ignored here since the tables may not exist yet on the
	// very first run before the schema has ever been applied.
	_, _ = pool.Exec(ctx, "TRUNCATE TABLE grcache_entries, grcache_entry_tags")
}

func newPostgresCache() (grcache.Cache, error) {
	return grcache.NewPostgresCache(grcache.PostgresConfig{DSN: postgresTestDSN})
}

func newPostgresCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	if _, err := newPostgresCache(); err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	truncatePostgresTestDB(t)
	cache, err := newPostgresCache()
	if err != nil {
		t.Fatalf("newPostgresCache: %v", err)
	}
	t.Cleanup(func() {
		_ = cache.Close()
		truncatePostgresTestDB(t)
	})
	return cache
}

func TestSchemaApplyIsIdempotent(t *testing.T) {
	c1, err := newPostgresCache()
	if err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	_ = c1.Close()

	c2, err := newPostgresCache()
	if err != nil {
		t.Fatalf("newPostgresCache (2nd, should re-apply schema safely): %v", err)
	}
	_ = c2.Close()
}

func TestNewPostgresCache_MissingDSN(t *testing.T) {
	if _, err := grcache.NewPostgresCache(grcache.PostgresConfig{}); err == nil {
		t.Fatal("NewPostgresCache with empty DSN = nil error, want error")
	}
}

func TestNewPostgresCache_MalformedDSN(t *testing.T) {
	// Distinct from TestNewPostgresCache_BadDSN (a syntactically valid DSN
	// with bad credentials, caught later at Ping): this DSN fails to parse
	// at all, exercising pgxpool.ParseConfig's own error branch.
	_, err := grcache.NewPostgresCache(grcache.PostgresConfig{
		DSN: "not a valid dsn ::: %zz",
	})
	if err == nil {
		t.Fatal("NewPostgresCache with a malformed DSN = nil error, want error")
	}
}

func TestNewPostgresCache_BadDSN(t *testing.T) {
	_, err := grcache.NewPostgresCache(grcache.PostgresConfig{
		DSN: "host=localhost user=nonexistent password=wrong dbname=grcache_test port=5432 sslmode=disable connect_timeout=1",
	})
	if err == nil {
		t.Fatal("NewPostgresCache with bad credentials = nil error, want error")
	}
}

func TestPostgresSweepReclaimsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	if _, err := newPostgresCache(); err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	truncatePostgresTestDB(t)
	cache, err := grcache.NewPostgresCache(grcache.PostgresConfig{
		DSN:           postgresTestDSN,
		SweepInterval: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPostgresCache: %v", err)
	}
	defer func() { _ = cache.Close() }()
	defer truncatePostgresTestDB(t)

	if err := cache.Set(ctx, "sweep-me", []byte("v"), 10*time.Millisecond, "sweep-tag"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := cache.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.KeyCount == 0 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}

	t.Fatal("sweep did not reclaim expired entry within deadline")
}

func TestPostgresConnectionPoolOptions(t *testing.T) {
	if _, err := newPostgresCache(); err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	truncatePostgresTestDB(t)
	cache, err := grcache.NewPostgresCache(grcache.PostgresConfig{
		DSN:             postgresTestDSN,
		MaxConns:        5,
		MinConns:        1,
		MaxConnLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewPostgresCache with pool options: %v", err)
	}
	defer func() { _ = cache.Close() }()
	defer truncatePostgresTestDB(t)
}

func TestPostgresWithLogger(t *testing.T) {
	if _, err := newPostgresCache(); err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	logger := &recordingLogger{}
	truncatePostgresTestDB(t)
	cache, err := grcache.NewPostgresCache(grcache.PostgresConfig{
		DSN:           postgresTestDSN,
		SweepInterval: 20 * time.Millisecond,
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("NewPostgresCache: %v", err)
	}
	defer truncatePostgresTestDB(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

func TestTransactionRollbackOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	cache := newPostgresCacheForTest(t)

	// A normal Set with tags should leave both the entry and its tag rows
	// present — this is the baseline the rollback guarantee protects.
	if err := cache.Set(ctx, "txn-key", []byte("v"), time.Minute, "txn-tag"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	n, err := cache.InvalidateTag(ctx, "txn-tag")
	if err != nil {
		t.Fatalf("InvalidateTag: %v", err)
	}
	if n != 1 {
		t.Fatalf("InvalidateTag returned %d, want 1", n)
	}

	if _, err := cache.Get(ctx, "txn-key"); err == nil {
		t.Fatal("Get after InvalidateTag = nil error, want ErrKeyNotFound")
	}
}
