-- migrate:up
CREATE TABLE IF NOT EXISTS actor_capabilities (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    target TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    granted_by TEXT,
    expires_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_actor_caps_token ON actor_capabilities(kind, target, secret_hash);
CREATE INDEX IF NOT EXISTS idx_actor_caps_target ON actor_capabilities(kind, target);

-- migrate:down
DROP TABLE IF EXISTS actor_capabilities;
