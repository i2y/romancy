-- migrate:up
CREATE TABLE IF NOT EXISTS actor_fs (
    path TEXT PRIMARY KEY,
    owner TEXT,
    data BYTEA NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_actor_fs_owner ON actor_fs(owner);

-- migrate:down
DROP TABLE IF EXISTS actor_fs;
