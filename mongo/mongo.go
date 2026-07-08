// File: mongo/mongo.go

// Package mongo is a grcache backend added for parity with gourdiantoken's
// MongoTokenRepository. It uses go.mongodb.org/mongo-driver v1 — the same
// driver family gourdiantoken depends on, tracked to its latest v1.x
// release rather than pinned to gourdiantoken's exact version (see
// docs/architecture.md's "Latest dependency versions" divergence). The v1
// module is upstream-deprecated in favor of go.mongodb.org/mongo-driver/v2,
// but migrating to that would be a breaking API rewrite out of scope for a
// routine dependency bump.
//
// Unlike Postgres's separate join table, tags live directly as an array
// field on the same document — Mongo's document model and multikey indexes
// handle this natively, so value + tags + expiry stay atomic in a single
// ReplaceOne, with no second table to keep in sync.
//
// This is the one backend besides Redis where expiry is the database's job,
// not grcache's: a TTL index on expiresAt (mirroring gourdiantoken's own
// confirmed TTL-index usage) reaps expired documents automatically, so there
// is no background sweep goroutine here. Because Mongo's TTL background
// monitor runs on an interval (historically ~60s), not instantly, Get/Exists
// still perform a lazy expiry check client-side as a correctness backstop —
// the same pattern used by the memory and postgres backends.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/gourdian25/grcache"
)

const defaultCollection = "grcache_entries"

// cacheDocument is the BSON document shape for a single cached value.
type cacheDocument struct {
	Key       string    `bson:"_id"`
	Value     []byte    `bson:"value"`
	Tags      []string  `bson:"tags,omitempty"`
	ExpiresAt time.Time `bson:"expiresAt,omitempty"` // field omitted entirely when ttl=0 (no expiry)
}

func (d cacheDocument) expired(now time.Time) bool {
	return !d.ExpiresAt.IsZero() && !now.Before(d.ExpiresAt)
}

// MongoConfig configures a Cache constructed by NewMongoCache. Unlike
// gourdiantoken's NewMongoTokenRepository(mongoDB *mongo.Database,
// transactionsEnabled bool) — whose extra positional bool the plan
// explicitly flagged as an inconsistent shape — grcache owns a single
// Config struct so every backend constructor has the identical
// New<Backend>Cache(cfg Config) (grcache.Cache, error) signature.
//
// Example:
//
//	cfg := mongo.MongoConfig{
//		URI:      "mongodb://localhost:27017",
//		Database: "myapp",
//	}
type MongoConfig struct {
	// URI is the MongoDB connection string, e.g.
	// "mongodb://localhost:27017". Required.
	URI string

	// Database is the database name to use. Required.
	Database string

	// Collection is the collection name to store entries in. Defaults to
	// "grcache_entries" if empty.
	Collection string

	// Logger receives optional diagnostic messages (connection failures,
	// shutdown). A nil Logger disables logging entirely.
	Logger grcache.Logger
}

func (cfg MongoConfig) withDefaults() MongoConfig {
	if cfg.Collection == "" {
		cfg.Collection = defaultCollection
	}
	return cfg
}

// Cache is a MongoDB-backed implementation of grcache.Cache.
type Cache struct {
	client     *mongo.Client
	collection *mongo.Collection
	logger     grcache.Logger

	closed    atomic.Bool
	closeOnce sync.Once

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ grcache.Cache = (*Cache)(nil)

// NewMongoCache connects to cfg.URI, validates connectivity with Ping,
// ensures the TTL and tag indexes exist, and returns a ready-to-use Cache.
//
// Parameters:
//   - cfg: MongoConfig — URI and Database are required
//
// Returns:
//   - grcache.Cache: ready to use
//   - error: non-nil if URI/Database is empty, the connection/Ping fails
//     (wrapping grcache.ErrCacheUnavailable), or index creation fails
//
// Example:
//
//	cache, err := mongo.NewMongoCache(mongo.MongoConfig{
//		URI:      "mongodb://localhost:27017",
//		Database: "myapp",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
func NewMongoCache(cfg MongoConfig) (grcache.Cache, error) {
	if cfg.URI == "" {
		return nil, fmt.Errorf("grcache/mongo: MongoConfig.URI is required")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("grcache/mongo: MongoConfig.Database is required")
	}
	cfg = cfg.withDefaults()
	logger := grcache.OrNop(cfg.Logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.URI))
	if err != nil {
		logger.Errorf("grcache/mongo: connect failed: %v", err)
		return nil, fmt.Errorf("grcache/mongo: connect: %w", grcache.ErrCacheUnavailable)
	}

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(ctx)
		logger.Errorf("grcache/mongo: ping failed: %v", err)
		return nil, fmt.Errorf("grcache/mongo: ping: %w", grcache.ErrCacheUnavailable)
	}

	collection := client.Database(cfg.Database).Collection(cfg.Collection)

	if err := ensureIndexes(ctx, collection); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("grcache/mongo: ensure indexes: %w", err)
	}

	logger.Infof("grcache/mongo: connected to database %q collection %q", cfg.Database, cfg.Collection)
	return &Cache{client: client, collection: collection, logger: logger}, nil
}

func ensureIndexes(ctx context.Context, collection *mongo.Collection) error {
	// TTL index: expires a document exactly at the stored expiresAt
	// timestamp. Documents with the field omitted (ttl=0, "no expiry") are
	// simply never touched by Mongo's TTL monitor.
	ttlIndex := mongo.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	}
	if _, err := collection.Indexes().CreateOne(ctx, ttlIndex); err != nil {
		return err
	}

	// Regular multikey index on tags for efficient InvalidateTag queries.
	tagIndex := mongo.IndexModel{
		Keys: bson.D{{Key: "tags", Value: 1}},
	}
	if _, err := collection.Indexes().CreateOne(ctx, tagIndex); err != nil {
		return err
	}

	return nil
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, grcache.ErrClosed
	}

	var doc cacheDocument
	err := c.collection.FindOne(ctx, bson.M{"_id": key}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/mongo: get %q: %w", key, grcache.ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/mongo: get %q: %w", key, grcache.ErrCacheUnavailable)
	}

	if doc.expired(time.Now()) {
		c.misses.Add(1)
		return nil, fmt.Errorf("grcache/mongo: get %q: %w", key, grcache.ErrKeyNotFound)
	}

	c.hits.Add(1)
	return doc.Value, nil
}

func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}
	if ttl < 0 {
		return grcache.ErrInvalidTTL
	}

	doc := cacheDocument{Key: key, Value: val, Tags: tags}
	if ttl > 0 {
		doc.ExpiresAt = time.Now().Add(ttl)
	}

	_, err := c.collection.ReplaceOne(ctx, bson.M{"_id": key}, doc, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("grcache/mongo: set %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}

	if _, err := c.collection.DeleteOne(ctx, bson.M{"_id": key}); err != nil {
		return fmt.Errorf("grcache/mongo: delete %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, grcache.ErrClosed
	}

	var doc cacheDocument
	err := c.collection.FindOne(ctx, bson.M{"_id": key}, options.FindOne().SetProjection(bson.M{"expiresAt": 1})).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		return false, fmt.Errorf("grcache/mongo: exists %q: %w", key, grcache.ErrCacheUnavailable)
	}

	if doc.expired(time.Now()) {
		return false, nil
	}
	return true, nil
}

func (c *Cache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, grcache.ErrClosed
	}

	result, err := c.collection.DeleteMany(ctx, bson.M{"tags": tag})
	if err != nil {
		return 0, fmt.Errorf("grcache/mongo: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
	}
	return int(result.DeletedCount), nil
}

func (c *Cache) Stats(ctx context.Context) (grcache.Stats, error) {
	if c.closed.Load() {
		return grcache.Stats{}, grcache.ErrClosed
	}

	keyCount, err := c.collection.EstimatedDocumentCount(ctx)
	if err != nil {
		return grcache.Stats{}, fmt.Errorf("grcache/mongo: stats: %w", grcache.ErrCacheUnavailable)
	}

	return grcache.Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  keyCount,
	}, nil
}

func (c *Cache) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = c.client.Disconnect(ctx)
		c.logger.Infof("grcache/mongo: cache closed")
	})
	return err
}
