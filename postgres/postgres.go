// File: postgres/postgres.go

// Package postgres is a grcache backend for test/dev/CI environments that
// have a PostgreSQL instance available but not Redis or memcached — it is
// not a recommended production alternative to grcache/redis. It uses GORM
// (tracked to its latest release rather than pinned to gourdiantoken's
// version — see docs/architecture.md) and mirrors GORM conventions already
// proven in gourdiantoken's GormTokenRepository (indexed columns,
// TableName() overrides).
//
// Unlike Redis's Sets or Mongo's embedded array field, Postgres has no
// native multi-value column well-suited to tag storage without added
// complexity, so tags live in a separate join table kept in sync with the
// entries table on every Set/Delete — a deliberate difference from Redis's
// "leave stale tag-set members lying around" approach, justified because
// these are already multi-statement transactions here, so keeping
// InvalidateTag's query simple (no need to filter against still-existing
// keys) costs nothing extra.
//
// Postgres has no native key expiry at all, unlike Redis (EX) or Mongo (TTL
// indexes) — expiry here is entirely grcache's responsibility, enforced by
// a background sweep goroutine plus a lazy check on every Get/Exists.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/gourdian25/grcache"
)

const defaultSweepInterval = 30 * time.Second

// cacheEntry is the GORM model for a single cached value.
type cacheEntry struct {
	Key       string    `gorm:"primaryKey;type:varchar(512)"`
	Value     []byte    `gorm:"type:bytea;not null"`
	ExpiresAt time.Time `gorm:"index:idx_grcache_expires_at"` // zero value = no expiry
	CreatedAt time.Time `gorm:"not null"`
}

func (cacheEntry) TableName() string { return "grcache_entries" }

func (e cacheEntry) expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

// cacheEntryTag is the GORM model for the tag -> key join table.
type cacheEntryTag struct {
	Key string `gorm:"index:idx_grcache_tag_key,composite:tag_key;type:varchar(512);not null"`
	Tag string `gorm:"index:idx_grcache_tag_key,composite:tag_key;type:varchar(255);not null"`
}

func (cacheEntryTag) TableName() string { return "grcache_entry_tags" }

// PostgresConfig configures a Cache constructed by NewPostgresCache.
//
// Example:
//
//	cfg := postgres.PostgresConfig{
//		DSN: "host=localhost user=myuser password=mypass dbname=mydb port=5432 sslmode=disable",
//	}
type PostgresConfig struct {
	// DSN is a standard libpq connection string. Required.
	DSN string

	// MaxOpenConns caps the connection pool size. 0 means use database/sql's
	// own default (unlimited).
	MaxOpenConns int

	// MaxIdleConns caps idle connections retained in the pool. 0 means use
	// database/sql's own default (2).
	MaxIdleConns int

	// ConnMaxLifetime bounds how long a pooled connection may be reused
	// before being recycled. 0 means connections are reused indefinitely.
	ConnMaxLifetime time.Duration

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
	Logger grcache.Logger
}

func (cfg PostgresConfig) withDefaults() PostgresConfig {
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweepInterval
	}
	return cfg
}

// Cache is a PostgreSQL-backed implementation of grcache.Cache, using GORM.
type Cache struct {
	db     *gorm.DB
	logger grcache.Logger

	closed    atomic.Bool
	closeOnce sync.Once
	closeChan chan struct{}
	wg        sync.WaitGroup

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

var _ grcache.Cache = (*Cache)(nil)

// NewPostgresCache opens a connection per cfg, auto-migrates the schema
// (two tables: grcache_entries and grcache_entry_tags), and validates
// connectivity via Ping before returning.
//
// Parameters:
//   - cfg: PostgresConfig — DSN is required
//
// Returns:
//   - grcache.Cache: ready to use
//   - error: non-nil if DSN is empty, the connection/Ping fails, or
//     AutoMigrate fails; connection/Ping failures wrap
//     grcache.ErrCacheUnavailable
//
// Example:
//
//	cache, err := postgres.NewPostgresCache(postgres.PostgresConfig{
//		DSN: "host=localhost user=myuser password=mypass dbname=mydb port=5432 sslmode=disable",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer cache.Close()
func NewPostgresCache(cfg PostgresConfig) (grcache.Cache, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("grcache/postgres: PostgresConfig.DSN is required")
	}
	cfg = cfg.withDefaults()
	appLogger := grcache.OrNop(cfg.Logger)

	// Get(missing key) is expected control flow for a cache, not a real
	// error, so record-not-found lookups should not be logged as failures.
	gormLogger := logger.Default.LogMode(logger.Silent)
	db, err := gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{Logger: gormLogger})
	if err != nil {
		appLogger.Errorf("grcache/postgres: open failed: %v", err)
		return nil, fmt.Errorf("grcache/postgres: open: %w", grcache.ErrCacheUnavailable)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("grcache/postgres: underlying sql.DB: %w", grcache.ErrCacheUnavailable)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		appLogger.Errorf("grcache/postgres: ping failed: %v", err)
		return nil, fmt.Errorf("grcache/postgres: ping: %w", grcache.ErrCacheUnavailable)
	}

	if err := db.AutoMigrate(&cacheEntry{}, &cacheEntryTag{}); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("grcache/postgres: automigrate: %w", err)
	}

	appLogger.Infof("grcache/postgres: connected (sweep interval %s)", cfg.SweepInterval)

	c := &Cache{
		db:        db,
		logger:    appLogger,
		closeChan: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.sweepLoop(cfg.SweepInterval)

	return c, nil
}

func (c *Cache) sweepLoop(interval time.Duration) {
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

func (c *Cache) sweep() {
	now := time.Now()

	var expiredKeys []string
	err := c.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&cacheEntry{}).
			Where("expires_at IS NOT NULL AND expires_at <> ? AND expires_at <= ?", time.Time{}, now).
			Pluck("key", &expiredKeys).Error; err != nil {
			return err
		}
		if len(expiredKeys) == 0 {
			return nil
		}
		if err := tx.Where("key IN ?", expiredKeys).Delete(&cacheEntry{}).Error; err != nil {
			return err
		}
		return tx.Where("key IN ?", expiredKeys).Delete(&cacheEntryTag{}).Error
	})
	if err != nil {
		c.logger.Errorf("grcache/postgres: sweep failed: %v", err)
		return
	}

	if len(expiredKeys) > 0 {
		c.evictions.Add(uint64(len(expiredKeys)))
		c.logger.Infof("grcache/postgres: sweep reclaimed %d expired entries", len(expiredKeys))
	}
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.closed.Load() {
		return nil, grcache.ErrClosed
	}

	var e cacheEntry
	err := c.db.WithContext(ctx).First(&e, "key = ?", key).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.misses.Add(1)
			return nil, fmt.Errorf("grcache/postgres: get %q: %w", key, grcache.ErrKeyNotFound)
		}
		return nil, fmt.Errorf("grcache/postgres: get %q: %w", key, grcache.ErrCacheUnavailable)
	}

	if e.expired(time.Now()) {
		c.misses.Add(1)
		return nil, fmt.Errorf("grcache/postgres: get %q: %w", key, grcache.ErrKeyNotFound)
	}

	c.hits.Add(1)
	return e.Value, nil
}

func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration, tags ...string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}
	if ttl < 0 {
		return grcache.ErrInvalidTTL
	}

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	entry := cacheEntry{Key: key, Value: val, ExpiresAt: expiresAt, CreatedAt: time.Now()}

	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "expires_at"}),
		}).Create(&entry).Error; err != nil {
			return err
		}

		if err := tx.Where("key = ?", key).Delete(&cacheEntryTag{}).Error; err != nil {
			return err
		}

		if len(tags) > 0 {
			rows := make([]cacheEntryTag, len(tags))
			for i, tag := range tags {
				rows[i] = cacheEntryTag{Key: key, Tag: tag}
			}
			if err := tx.Create(&rows).Error; err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("grcache/postgres: set %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	if c.closed.Load() {
		return grcache.ErrClosed
	}

	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&cacheEntry{}, "key = ?", key).Error; err != nil {
			return err
		}
		return tx.Where("key = ?", key).Delete(&cacheEntryTag{}).Error
	})
	if err != nil {
		return fmt.Errorf("grcache/postgres: delete %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return nil
}

func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if c.closed.Load() {
		return false, grcache.ErrClosed
	}

	var count int64
	err := c.db.WithContext(ctx).Model(&cacheEntry{}).
		Where("key = ? AND (expires_at IS NULL OR expires_at = ? OR expires_at > ?)", key, time.Time{}, time.Now()).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("grcache/postgres: exists %q: %w", key, grcache.ErrCacheUnavailable)
	}
	return count > 0, nil
}

func (c *Cache) InvalidateTag(ctx context.Context, tag string) (int, error) {
	if c.closed.Load() {
		return 0, grcache.ErrClosed
	}

	var n int64
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sub := tx.Model(&cacheEntryTag{}).Select("key").Where("tag = ?", tag)

		result := tx.Where("key IN (?)", sub).Delete(&cacheEntry{})
		if result.Error != nil {
			return result.Error
		}
		n = result.RowsAffected

		return tx.Where("tag = ?", tag).Delete(&cacheEntryTag{}).Error
	})
	if err != nil {
		return 0, fmt.Errorf("grcache/postgres: invalidate tag %q: %w", tag, grcache.ErrCacheUnavailable)
	}

	return int(n), nil
}

func (c *Cache) Stats(ctx context.Context) (grcache.Stats, error) {
	if c.closed.Load() {
		return grcache.Stats{}, grcache.ErrClosed
	}

	var keyCount int64
	if err := c.db.WithContext(ctx).Model(&cacheEntry{}).Count(&keyCount).Error; err != nil {
		return grcache.Stats{}, fmt.Errorf("grcache/postgres: stats: %w", grcache.ErrCacheUnavailable)
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
		close(c.closeChan)
		c.wg.Wait()

		sqlDB, dbErr := c.db.DB()
		if dbErr != nil {
			err = dbErr
			return
		}
		err = sqlDB.Close()
		c.logger.Infof("grcache/postgres: cache closed")
	})
	return err
}
