// File: internal_coverage_test.go

package grcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests construct a real backend Cache then close/disconnect its
// underlying client or pool directly (reaching into the unexported
// concrete type, hence package grcache rather than grcache_test) while the
// Cache object itself still believes it is open — deterministically
// reaching every method's ErrCacheUnavailable-wrapping branch, the same
// "close the connection out from under an otherwise-valid client" technique
// used throughout the gourdian ecosystem (see gourdiantoken's repository
// coverage tests). Confirmed empirically that go-redis, gomemcache, pgx,
// and mongo-driver all return clean, non-panicking errors from a
// closed/disconnected client rather than panicking.
//
// Skips gracefully if the corresponding live service isn't reachable,
// matching the rest of this repo's test convention.

const (
	covRedisAddr     = "localhost:6379"
	covRedisPassword = "redis_password"
	covRedisDB       = 14

	covMemcachedAddr = "localhost:11211"

	covPostgresDSN = "host=localhost user=postgres_user password=postgres_password dbname=grcache_test port=5432 sslmode=disable"

	covMongoURI      = "mongodb://root:mongo_password@localhost:27018/?directConnection=true"
	covMongoDatabase = "grcache_test"
)

func TestRedisCache_OperationsAfterClientClosed(t *testing.T) {
	cache, err := NewRedisCache(RedisConfig{Addr: covRedisAddr, Password: covRedisPassword, DB: covRedisDB})
	if err != nil {
		t.Skipf("Redis not available, skipping: %v", err)
	}
	rc := cache.(*redisCache)
	_ = rc.client.Close()

	ctx := context.Background()
	if _, err := cache.Get(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Get after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Set(ctx, "x", []byte("v"), time.Minute); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Set after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Delete(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Delete after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Exists(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Exists after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.InvalidateTag(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("InvalidateTag after client closed error = %v, want ErrCacheUnavailable", err)
	}
}

// TestMemcachedCache_OperationsAfterClientClosed constructs a
// memcachedCache directly (bypassing NewMemcachedCache, whose Ping check
// would reject this) with a client pointed at an address nothing listens
// on. Unlike go-redis/pgx/mongo-driver, gomemcache.Client dials connections
// lazily per request rather than holding one persistent connection, so
// closing an already-connected client's idle pool doesn't actually break
// its next request (confirmed empirically — Close() alone never reached
// the intended error branch). Pointing a fresh client at an unreachable
// address is the reliable way to force every real operation to fail.
func TestMemcachedCache_OperationsAfterClientClosed(t *testing.T) {
	cache := &memcachedCache{client: memcache.New("localhost:1"), logger: NopLogger()}

	ctx := context.Background()
	if _, err := cache.Get(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Get after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Set(ctx, "x", []byte("v"), time.Minute, "tag"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Set after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Delete(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Delete after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Exists(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Exists after client closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.InvalidateTag(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("InvalidateTag after client closed error = %v, want ErrCacheUnavailable", err)
	}
}

func TestPostgresCache_OperationsAfterPoolClosed(t *testing.T) {
	cache, err := NewPostgresCache(PostgresConfig{DSN: covPostgresDSN})
	if err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	pc := cache.(*postgresCache)
	pc.pool.Close()

	ctx := context.Background()
	if _, err := cache.Get(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Get after pool closed error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Set(ctx, "x", []byte("v"), time.Minute, "tag"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Set after pool closed error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Delete(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Delete after pool closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Exists(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Exists after pool closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.InvalidateTag(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("InvalidateTag after pool closed error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Stats(ctx); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Stats after pool closed error = %v, want ErrCacheUnavailable", err)
	}
}

func TestMongoCache_OperationsAfterClientDisconnected(t *testing.T) {
	cache, err := NewMongoCache(MongoConfig{URI: covMongoURI, Database: covMongoDatabase})
	if err != nil {
		t.Skipf("MongoDB not available, skipping: %v", err)
	}
	mc := cache.(*mongoCache)
	_ = mc.client.Disconnect(context.Background())

	ctx := context.Background()
	if _, err := cache.Get(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Get after client disconnected error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Set(ctx, "x", []byte("v"), time.Minute, "tag"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Set after client disconnected error = %v, want ErrCacheUnavailable", err)
	}
	if err := cache.Delete(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Delete after client disconnected error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Exists(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Exists after client disconnected error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.InvalidateTag(ctx, "x"); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("InvalidateTag after client disconnected error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Stats(ctx); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Stats after client disconnected error = %v, want ErrCacheUnavailable", err)
	}
}

// TestExpirationSeconds_SubSecondRoundsUpToOne exercises expirationSeconds'
// one branch no live-server test happens to hit: every ttl used elsewhere
// in this suite is either 0 (no expiry) or >= 1 second.
func TestExpirationSeconds_SubSecondRoundsUpToOne(t *testing.T) {
	if got := expirationSeconds(500 * time.Millisecond); got != 1 {
		t.Fatalf("expirationSeconds(500ms) = %d, want 1", got)
	}
}

// TestApplyPostgresSchema_AcquireFailsOnClosedPool exercises
// applyPostgresSchema's own Acquire-failure branch directly: a closed pool
// can never hand out a connection. Not reachable through NewPostgresCache
// itself (which always applies the schema against a pool it just
// successfully Pinged), so tested by calling the unexported function
// directly against a pool closed just beforehand.
func TestApplyPostgresSchema_AcquireFailsOnClosedPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, covPostgresDSN)
	if err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	pool.Close()

	if err := applyPostgresSchema(ctx, pool); err == nil {
		t.Fatal("applyPostgresSchema against a closed pool returned nil error, want an acquire failure")
	}
}

// TestPostgresCache_SweepDirectly exercises sweep's own no-expired-keys
// early return and its ListExpiredKeys-fails branch directly. Neither is
// reliably reachable through the background sweepLoop goroutine: the
// no-expired-keys case depends on winning a timing race against the
// interval ticker, and the fails branch needs the pool broken specifically
// at sweep time, not throughout the test (which the standard
// operations-after-pool-closed tests above already cover for every
// exported method instead).
func TestPostgresCache_SweepDirectly(t *testing.T) {
	cache, err := NewPostgresCache(PostgresConfig{DSN: covPostgresDSN})
	if err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	pc := cache.(*postgresCache)
	defer func() { _ = cache.Close() }()

	// No expired keys currently present (a fresh cache): must return
	// promptly without error.
	pc.sweep()

	// Break the pool, then sweep directly: ListExpiredKeys must fail and
	// sweep must log and return rather than panic.
	pc.pool.Close()
	pc.sweep()
}

// TestMemcachedWriteTagList_EmptyMembersDeletesKey exercises writeTagList's
// own empty-members branch directly. No current call path ever reaches
// it through the public API (addToTagList only ever appends a member, so
// members is always non-empty by the time it calls writeTagList) — this is
// a defensive branch for writeTagList's own general contract ("write
// whatever list you're given, however many members it has"), covered here
// by calling it directly.
func TestMemcachedWriteTagList_EmptyMembersDeletesKey(t *testing.T) {
	cache, err := NewMemcachedCache(MemcachedConfig{Servers: []string{covMemcachedAddr}})
	if err != nil {
		t.Skipf("memcached not available, skipping: %v", err)
	}
	defer func() { _ = cache.Close() }()
	mc := cache.(*memcachedCache)

	const tag = "write-tag-list-empty-test"
	if err := mc.writeTagList(tag, []string{"member"}); err != nil {
		t.Fatalf("writeTagList (seed): %v", err)
	}
	if err := mc.writeTagList(tag, nil); err != nil {
		t.Fatalf("writeTagList (empty): %v", err)
	}

	members, err := mc.readTagList(tag)
	if err != nil {
		t.Fatalf("readTagList: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("readTagList after writeTagList(empty) = %v, want empty (key should have been deleted)", members)
	}
}
