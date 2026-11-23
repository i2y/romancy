-- migrate:up
CREATE TABLE IF NOT EXISTS payload_blobs (
    hash TEXT PRIMARY KEY,
    data BLOB NOT NULL,
    size_bytes INTEGER NOT NULL,
    created_by TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- migrate:down
DROP TABLE IF EXISTS payload_blobs;
