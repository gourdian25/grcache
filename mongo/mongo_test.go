// File: mongo/mongo_test.go

package mongo_test

// Test URI reuses gourdiantoken's confirmed local Mongo settings exactly
// (mongodb://root:mongo_password@localhost:27018/?directConnection=true —
// port 27018 there specifically dodges a local Docker Desktop
// port-forwarding bug, so this reuses it rather than reintroducing that bug
// by picking 27017) but a distinct database name (grcache_test, not
// whatever database gourdiantoken's own suite uses) to avoid collection
// collisions.

import (
	"context"
	"testing"
	"time"

	gomongo "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/gourdian25/grcache"
	"github.com/gourdian25/grcache/conformance"
	grcachemongo "github.com/gourdian25/grcache/mongo"
)

const (
	testURI      = "mongodb://root:mongo_password@localhost:27018/?directConnection=true"
	testDatabase = "grcache_test"
)

func dropTestDB(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := gomongo.Connect(ctx, options.Client().ApplyURI(testURI))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)

	if err := client.Database(testDatabase).Drop(ctx); err != nil {
		t.Fatalf("drop database: %v", err)
	}
}

func newCache() (grcache.Cache, error) {
	return grcachemongo.NewMongoCache(grcachemongo.MongoConfig{URI: testURI, Database: testDatabase})
}

func newCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	dropTestDB(t)
	cache, err := newCache()
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	t.Cleanup(func() {
		cache.Close()
		dropTestDB(t)
	})
	return cache
}

func TestConformance(t *testing.T) {
	dropTestDB(t)
	conformance.Run(t, newCache)
	dropTestDB(t)
}

func TestTTLIndexCreationIsIdempotent(t *testing.T) {
	c1, err := newCache()
	if err != nil {
		t.Fatalf("newCache (1st): %v", err)
	}
	c1.Close()

	c2, err := newCache()
	if err != nil {
		t.Fatalf("newCache (2nd, should re-create indexes safely): %v", err)
	}
	c2.Close()
}

func TestNewMongoCache_MissingURI(t *testing.T) {
	if _, err := grcachemongo.NewMongoCache(grcachemongo.MongoConfig{Database: testDatabase}); err == nil {
		t.Fatal("NewMongoCache with empty URI = nil error, want error")
	}
}

func TestNewMongoCache_MissingDatabase(t *testing.T) {
	if _, err := grcachemongo.NewMongoCache(grcachemongo.MongoConfig{URI: testURI}); err == nil {
		t.Fatal("NewMongoCache with empty Database = nil error, want error")
	}
}

func TestNewMongoCache_BadURI(t *testing.T) {
	_, err := grcachemongo.NewMongoCache(grcachemongo.MongoConfig{
		URI:      "mongodb://localhost:1/?connectTimeoutMS=200&serverSelectionTimeoutMS=200",
		Database: testDatabase,
	})
	if err == nil {
		t.Fatal("NewMongoCache with unreachable URI = nil error, want error")
	}
}

// TestExistsDuringTTLWindow exercises the window between an entry's logical
// expiry (per our own clock check) and Mongo's TTL background monitor
// physically reaping the document — Get/Exists must treat it as gone based
// on the lazy expiry check alone, without waiting for the TTL monitor.
func TestWithLogger(t *testing.T) {
	logger := &conformance.RecordingLogger{}
	dropTestDB(t)
	cache, err := grcachemongo.NewMongoCache(grcachemongo.MongoConfig{
		URI:      testURI,
		Database: testDatabase,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("NewMongoCache: %v", err)
	}
	defer dropTestDB(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.Total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

func TestExistsDuringTTLWindow(t *testing.T) {
	ctx := context.Background()
	cache := newCacheForTest(t)

	if err := cache.Set(ctx, "ttl-window-key", []byte("v"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	// Mongo's TTL monitor runs on an interval far longer than 150ms, so the
	// document is almost certainly still physically present — but Get/Exists
	// must still report it as gone via the lazy expiry check.
	if ok, err := cache.Exists(ctx, "ttl-window-key"); err != nil || ok {
		t.Fatalf("Exists (in TTL window) = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := cache.Get(ctx, "ttl-window-key"); err == nil {
		t.Fatal("Get (in TTL window) = nil error, want ErrKeyNotFound")
	}
}
