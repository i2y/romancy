-- migrate:up
CREATE TABLE IF NOT EXISTS payload_blobs (
    hash TEXT PRIMARY KEY,
    data BYTEA NOT NULL,
    size_bytes BIGINT NOT NULL,
    created_by TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- migrate:down
DROP TABLE IF EXISTS payload_blobs;
