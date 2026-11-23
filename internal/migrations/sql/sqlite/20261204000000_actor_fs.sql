-- migrate:up
CREATE TABLE IF NOT EXISTS actor_fs (
    path TEXT PRIMARY KEY,
    owner TEXT,
    data BLOB NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_actor_fs_owner ON actor_fs(owner);

-- migrate:down
DROP TABLE IF EXISTS actor_fs;
