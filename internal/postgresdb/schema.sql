-- File: internal/postgresdb/schema.sql

CREATE TABLE IF NOT EXISTS grcache_entries (
    key TEXT PRIMARY KEY,
    value BYTEA NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_grcache_expires_at ON grcache_entries (expires_at);

CREATE TABLE IF NOT EXISTS grcache_entry_tags (
    key TEXT NOT NULL,
    tag TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_grcache_entry_tags_tag_key ON grcache_entry_tags (tag, key);
CREATE INDEX IF NOT EXISTS idx_grcache_entry_tags_key ON grcache_entry_tags (key);
