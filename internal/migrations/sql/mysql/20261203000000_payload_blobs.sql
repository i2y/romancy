-- migrate:up
CREATE TABLE IF NOT EXISTS payload_blobs (
    hash VARCHAR(64) PRIMARY KEY,
    data LONGBLOB NOT NULL,
    size_bytes BIGINT NOT NULL,
    created_by VARCHAR(64),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- migrate:down
DROP TABLE IF EXISTS payload_blobs;
