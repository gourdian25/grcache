// File: postgres.go

// PostgreSQL backend (postgresCache) for test/dev/CI environments that have
// a PostgreSQL instance available but not Redis or memcached — it is not a
// recommended production alternative to the Redis backend. It uses pgx/v5
// with sqlc-generated queries (see internal/postgresdb), the same pattern
// gourdiantoken and grnoti use for their own Postgres backends, replacing
// the GORM-based implementation this package originally shipped.
//
// Unlike Redis's Sets or Mongo's embedded array field, Postgres has no
// native multi-value column well-suited to tag storage without added
// complexity, so tags live in a separate join table (grcache_entry_tags)
// kept in sync with the entries table on every Set/Delete inside one
// transaction.
//
// Postgres has no native key expiry at all, unlike Redis (EX) or Mongo (TTL
// indexes) — expiry here is entirely grcache's responsibility, enforced by
// a background sweep goroutine plus a lazy check on every Get/Exists.
package grcache

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gourdian25/grcache/internal/postgresdb"
)

//go:embed internal/postgresdb/schema.sql
var postgresSchemaSQL string

// grcacheSchemaLockKey is a fixed Postgres advisory-lock key used by
// applyPostgresSchema to serialize schema application across concurrent
// callers — e.g. multiple service replicas racing on first boot against a
// fresh database, where concurrent CREATE TABLE/INDEX IF NOT EXISTS
// statements are not fully race-free in Postgres. Distinct from
// gourdiantoken's (8_314_672_205) and grnoti's (7_927_100_419) own lock
// keys, since the two libraries may share a database.
const grcacheSchemaLockKey int64 = 6_442_871_953

// PostgresConfig configures a Cache constructed by NewPostgresCache.
//
// Example:
//
//	cfg := grcache.PostgresConfig{
//		DSN: "host=localhost user=myuser password=mypass dbname=mydb port=5432 sslmode=disable",
//	}
type PostgresConfig struct {
	// DSN is a standard libpq/pgx connection string. Required.
	DSN string

	// MaxConns caps the pgxpool connection pool size. 0 means use pgxpool's
	// own default.
	MaxConns int32

	// MinConns is the minimum number of connections pgxpool keeps ready.
	// 0 means use pgxpool's own default.
	MinConns int32

	// MaxConnLifetime bounds how long a pooled connection may be reused
	// before being recycled. 0 means pgxpool's own default (unlimited).
	MaxConnLifetime time.Duration

	// SweepInterval sets how often the background goroutine reclaims
	// expired entries. Unlike Redis/Mongo, Postgres has no native expiry —
	// this sweep is the only reclamation mechanism, not a backstop. Defaults
	// to 30 seconds if <= 0. Get/Exists also check expiry lazily, so
	// correctness never depends on this interval, only memory/storage
	// reclamation timing does.
	SweepInterval time.Duration

	// Logger receives optional diagnostic messages (connection failures,
	// sweep-cycle summaries, shutdown). A nil Logger disables logging
	// entirely.
	Logger Logger
}

func (cfg PostgresConfig) withDefaults() PostgresConfig {
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweepInterval
	}
	return cfg
}

// postgresCache is a PostgreSQL-backed implementation of Cache, using pgx/v5
// and sqlc-generated queries.
type postgresCache struct {
	pool   *pgxpool.Pool
	q      *postgresdb.Queries
	logger Logger

	closed    atomic.Bool
	closeOnce sync.Once
	closeChan chan struct{}
	wg        sync.WaitGroup

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ Cache = (*postgresCache)(nil)

// NewPostgresCache opens a connection pool per cfg, applies the schema
// (two tables: grcache_entries and grcache_entry_tags, serialized by a
// Postgres advisory lock so concurrent callers building a cache against the
// same fresh database don't race on the DDL), and validates connectivity
// via Ping before returning.
//
// Parameters:
//   - cfg: PostgresConfig — DSN is required
//
// Returns:
//   - Cache: ready to use
//   - error: non-nil if DSN is empty or malformed, the connection/Ping
//     fails, or schema application fails; connection/Ping failures wrap
//     ErrCacheUnavailable
//
// Example:
//
//	cache, err := grcache.NewPostgresCache(grcache.PostgresConfig{
//		DSN: "host=localhost user=myuser password=mypass dbname=mydb port=5432 sslmode=disable",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
func NewPostgresCache(cfg PostgresConfig) (Cache, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("grcache/postgres: PostgresConfig.DSN is required")
	}
	cfg = cfg.withDefaults()
	logger := OrNop(cfg.Logger)

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("grcache/postgres: parse dsn: %w", ErrCacheUnavailable)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Error("grcache/postgres: open failed", "error", err)
		return nil, fmt.Errorf("grcache/postgres: open: %w", ErrCacheUnavailable)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		logger.Error("grcache/postgres: ping failed", "error", err)
		return nil, fmt.Errorf("grcache/postgres: ping: %w", ErrCacheUnavailable)
	}

	if err := applyPostgresSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("grcache/postgres: apply schema: %w", err)
	}

	logger.Info("grcache/postgres: connected", "sweep_interval", cfg.SweepInterval)

	c := &postgresCache{
		pool:      pool,
		q:         postgresdb.New(pool),
		logger:    logger,
		closeChan: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.sweepLoop(cfg.SweepInterval)

	return c, nil
}

// applyPostgresSchema applies the embedded schema against pool, serialized
// by a Postgres session-level advisory lock (grcacheSchemaLockKey) so
// concurrent callers apply it one at a time instead of racing on catalog
// DDL. The lock/exec/unlock sequence runs on a single acquired connection,
// since advisory locks are session-scoped, and is unlocked explicitly
// before the connection is released back to the pool — a released pooled
// connection is reused, not reset, so the lock would otherwise leak onto
// whichever caller acquires that connection next.
func applyPostgresSchema(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", grcacheSchemaLockKey); err != nil {
		return fmt.Errorf("acquire schema lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", grcacheSchemaLockKey)
	}()

	if _, err := conn.Exec(ctx, postgresSchemaSQL); err != nil {
		return err
	}
	return nil
}

// pgExpiredAt reports whether a nullable expires_at column value has
// already passed as of now. A NULL column (Valid == false) means "no
// expiry" — the Postgres analog of the zero time.Time used by the memory
// backend.
func pgExpiredAt(t pgtype.Timestamptz, now time.Time) bool {
	return t.Valid && !now.Before(t.Time)
}

func pgExpiresAt(ttl time.Duration) pgtype.Timestamptz {
	if ttl <= 0 {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true}
}

func (c *postgresCache) sweepLoop(interval time.Duration) {
	defer c.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.sweep()
		case <-c.closeChan:
			return
		}
	}
}

func (c *postgresCache) sweep() {
	ctx := context.Background()

	keys, err := c.q.ListExpiredKeys(ctx)
	if err != nil {
		c.logger.Error("grcache/postgres: sweep failed", "error", err)
		return
	}
	if len(keys) == 0 {
		return
	}

	err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		q := c.q.WithTx(tx)
		if _, err := q.DeleteEntriesByKeys(ctx, keys); err != nil {
			return err
		}
		return q.DeleteEntryTagsByKeys(ctx, keys)
	})
	if err != nil {
		c.logger.Error("grcache/postgres: sweep failed", "error", err)
		return
	}

	c.evictions.Add(uint64(len(keys)))
	c.logger.Debug("grcache/postgres: sweep reclaimed expired entries", "count", len(keys))
}

func (c *postgresCache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}

	row, err := c.q.GetEntry(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/postgres: get %q: %w", key, ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/postgres: get %q: %w", key, ErrCacheUnavailable)
	}

	if pgExpiredAt(row.ExpiresAt, time.Now()) {
		c.misses.Add(1)
		return nil, fmt.Errorf("grcache/postgres: get %q: %w", key, ErrKeyNotFound)
	}

	c.hits.Add(1)
	return row.Value, nil
}

func (c *postgresCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if ttl < 0 {
		return ErrInvalidTTL
	}

	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		q := c.q.WithTx(tx)
		if err := q.UpsertEntry(ctx, postgresdb.UpsertEntryParams{
			Key:       key,
			Value:     val,
			ExpiresAt: pgExpiresAt(ttl),
		}); err != nil {
			return err
		}
		if err := q.DeleteEntryTagsByKey(ctx, key); err != nil {
			return err
		}
		for _, tag := range tags {
			if err := q.InsertEntryTag(ctx, postgresdb.InsertEntryTagParams{Key: key, Tag: tag}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("grcache/postgres: set %q: %w", key, ErrCacheUnavailable)
	}
	return nil
}

func (c *postgresCache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return ErrClosed
	}

	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		q := c.q.WithTx(tx)
		if _, err := q.DeleteEntry(ctx, key); err != nil {
			return err
		}
		return q.DeleteEntryTagsByKey(ctx, key)
	})
	if err != nil {
		return fmt.Errorf("grcache/postgres: delete %q: %w", key, ErrCacheUnavailable)
	}
	return nil
}

func (c *postgresCache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, ErrClosed
	}

	count, err := c.q.CountLiveEntry(ctx, key)
	if err != nil {
		return false, fmt.Errorf("grcache/postgres: exists %q: %w", key, ErrCacheUnavailable)
	}
	return count > 0, nil
}

func (c *postgresCache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, ErrClosed
	}

	var n int64
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		q := c.q.WithTx(tx)
		var err error
		n, err = q.DeleteEntriesByTag(ctx, tag)
		if err != nil {
			return err
		}
		return q.DeleteEntryTagsByTag(ctx, tag)
	})
	if err != nil {
		return 0, fmt.Errorf("grcache/postgres: invalidate tag %q: %w", tag, ErrCacheUnavailable)
	}

	return int(n), nil
}

func (c *postgresCache) Stats(ctx context.Context) (Stats, error) {
	if c.closed.Load() {
		return Stats{}, ErrClosed
	}

	keyCount, err := c.q.CountAllEntries(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("grcache/postgres: stats: %w", ErrCacheUnavailable)
	}

	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		KeyCount:  keyCount,
	}, nil
}

func (c *postgresCache) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.closeChan)
		c.wg.Wait()
		c.pool.Close()
		c.logger.Info("grcache/postgres: cache closed")
	})
	return nil
}
