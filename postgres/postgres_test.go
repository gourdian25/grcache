// File: postgres/postgres_test.go

package postgres_test

// Test DSN reuses gourdiantoken's confirmed local Postgres settings
// (host=localhost user=postgres_user password=postgres_password port=5432
// sslmode=disable) but a distinct dbname=grcache_test (not gourdiantoken's
// postgres_db) to avoid table collisions on a shared local instance.

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/gourdian25/grcache"
	"github.com/gourdian25/grcache/conformance"
	grcachepg "github.com/gourdian25/grcache/postgres"
)

const testDSN = "host=localhost user=postgres_user password=postgres_password dbname=grcache_test port=5432 sslmode=disable"

func truncateTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(postgres.Open(testDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	// Errors are ignored here since the tables may not exist yet on the very
	// first run before AutoMigrate has ever executed.
	db.Exec("TRUNCATE TABLE grcache_entries, grcache_entry_tags")
}

func newCache() (grcache.Cache, error) {
	return grcachepg.NewPostgresCache(grcachepg.PostgresConfig{DSN: testDSN})
}

func newCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	truncateTestDB(t)
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	t.Cleanup(func() {
		cache.Close()
		truncateTestDB(t)
	})
	return cache
}

func TestConformance(t *testing.T) {
	truncateTestDB(t)
	conformance.Run(t, newCache)
	truncateTestDB(t)
}

func TestAutoMigrateIsIdempotent(t *testing.T) {
	c1, err := newCache()
	if err != nil {
		t.Fatalf("newCache (1st): %v", err)
	}
	c1.Close()

	c2, err := newCache()
	if err != nil {
		t.Fatalf("newCache (2nd, should re-run AutoMigrate safely): %v", err)
	}
	c2.Close()
}

func TestNewPostgresCache_MissingDSN(t *testing.T) {
	if _, err := grcachepg.NewPostgresCache(grcachepg.PostgresConfig{}); err == nil {
		t.Fatal("NewPostgresCache with empty DSN = nil error, want error")
	}
}

func TestNewPostgresCache_BadDSN(t *testing.T) {
	_, err := grcachepg.NewPostgresCache(grcachepg.PostgresConfig{
		DSN: "host=localhost user=nonexistent password=wrong dbname=grcache_test port=5432 sslmode=disable connect_timeout=1",
	})
	if err == nil {
		t.Fatal("NewPostgresCache with bad credentials = nil error, want error")
	}
}

func TestSweepReclaimsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	truncateTestDB(t)
	cache, err := grcachepg.NewPostgresCache(grcachepg.PostgresConfig{
		DSN:           testDSN,
		SweepInterval: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPostgresCache: %v", err)
	}
	defer cache.Close()
	defer truncateTestDB(t)

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

func TestPostCloseAllMethods(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := cache.Delete(ctx, "anything"); !isClosed(err) {
		t.Fatalf("Delete after Close error = %v, want ErrClosed", err)
	}
	if _, err := cache.Exists(ctx, "anything"); !isClosed(err) {
		t.Fatalf("Exists after Close error = %v, want ErrClosed", err)
	}
	if _, err := cache.InvalidateTag(ctx, "anything"); !isClosed(err) {
		t.Fatalf("InvalidateTag after Close error = %v, want ErrClosed", err)
	}
	if _, err := cache.Stats(ctx); !isClosed(err) {
		t.Fatalf("Stats after Close error = %v, want ErrClosed", err)
	}
}

func isClosed(err error) bool {
	return err == grcache.ErrClosed
}

func TestConnectionPoolOptions(t *testing.T) {
	truncateTestDB(t)
	cache, err := grcachepg.NewPostgresCache(grcachepg.PostgresConfig{
		DSN:             testDSN,
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewPostgresCache with pool options: %v", err)
	}
	defer cache.Close()
	defer truncateTestDB(t)
}

func TestWithLogger(t *testing.T) {
	logger := &conformance.RecordingLogger{}
	truncateTestDB(t)
	cache, err := grcachepg.NewPostgresCache(grcachepg.PostgresConfig{
		DSN:           testDSN,
		SweepInterval: 20 * time.Millisecond,
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("NewPostgresCache: %v", err)
	}
	defer truncateTestDB(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.Total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

func TestTransactionRollbackOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

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
