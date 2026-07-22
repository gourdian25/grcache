// File: mongo_test.go

package grcache_test

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
)

const (
	mongoTestURI      = "mongodb://root:mongo_password@localhost:27018/?directConnection=true"
	mongoTestDatabase = "grcache_test"
)

func dropMongoTestDB(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := gomongo.Connect(ctx, options.Client().ApplyURI(mongoTestURI))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Database(mongoTestDatabase).Drop(ctx); err != nil {
		t.Fatalf("drop database: %v", err)
	}
}

func newMongoCache() (grcache.Cache, error) {
	return grcache.NewMongoCache(grcache.MongoConfig{URI: mongoTestURI, Database: mongoTestDatabase})
}

func newMongoCacheForTest(t *testing.T) grcache.Cache {
	t.Helper()
	if _, err := newMongoCache(); err != nil {
		t.Skipf("MongoDB not available, skipping: %v", err)
	}
	dropMongoTestDB(t)
	cache, err := newMongoCache()
	if err != nil {
		t.Fatalf("newMongoCache: %v", err)
	}
	t.Cleanup(func() {
		_ = cache.Close()
		dropMongoTestDB(t)
	})
	return cache
}

func TestTTLIndexCreationIsIdempotent(t *testing.T) {
	c1, err := newMongoCache()
	if err != nil {
		t.Skipf("MongoDB not available, skipping: %v", err)
	}
	_ = c1.Close()

	c2, err := newMongoCache()
	if err != nil {
		t.Fatalf("newMongoCache (2nd, should re-create indexes safely): %v", err)
	}
	_ = c2.Close()
}

func TestNewMongoCache_MissingURI(t *testing.T) {
	if _, err := grcache.NewMongoCache(grcache.MongoConfig{Database: mongoTestDatabase}); err == nil {
		t.Fatal("NewMongoCache with empty URI = nil error, want error")
	}
}

func TestNewMongoCache_MissingDatabase(t *testing.T) {
	if _, err := grcache.NewMongoCache(grcache.MongoConfig{URI: mongoTestURI}); err == nil {
		t.Fatal("NewMongoCache with empty Database = nil error, want error")
	}
}

func TestNewMongoCache_MalformedURI(t *testing.T) {
	// Distinct from TestNewMongoCache_BadURI (a syntactically valid URI
	// pointing at an unreachable host, caught later at Ping): this URI
	// fails mongo.Connect's own parsing, which mongo-driver validates
	// synchronously up front (unlike the lazy connection dialing that
	// makes an unreachable-but-valid URI only fail at Ping/first use).
	_, err := grcache.NewMongoCache(grcache.MongoConfig{
		URI:      "not-a-valid-mongo-uri",
		Database: mongoTestDatabase,
	})
	if err == nil {
		t.Fatal("NewMongoCache with a malformed URI = nil error, want error")
	}
}

func TestNewMongoCache_BadURI(t *testing.T) {
	_, err := grcache.NewMongoCache(grcache.MongoConfig{
		URI:      "mongodb://localhost:1/?connectTimeoutMS=200&serverSelectionTimeoutMS=200",
		Database: mongoTestDatabase,
	})
	if err == nil {
		t.Fatal("NewMongoCache with unreachable URI = nil error, want error")
	}
}

func TestMongoWithLogger(t *testing.T) {
	if _, err := newMongoCache(); err != nil {
		t.Skipf("MongoDB not available, skipping: %v", err)
	}
	logger := &recordingLogger{}
	dropMongoTestDB(t)
	cache, err := grcache.NewMongoCache(grcache.MongoConfig{
		URI:      mongoTestURI,
		Database: mongoTestDatabase,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("NewMongoCache: %v", err)
	}
	defer dropMongoTestDB(t)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if logger.total() == 0 {
		t.Fatal("WithLogger: no messages were logged, want at least one (connect and/or close)")
	}
}

// TestExistsDuringTTLWindow exercises the window between an entry's logical
// expiry (per our own clock check) and Mongo's TTL background monitor
// physically reaping the document — Get/Exists must treat it as gone based
// on the lazy expiry check alone, without waiting for the TTL monitor.
func TestExistsDuringTTLWindow(t *testing.T) {
	ctx := context.Background()
	cache := newMongoCacheForTest(t)

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
