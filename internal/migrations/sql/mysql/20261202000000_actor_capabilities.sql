-- migrate:up
CREATE TABLE IF NOT EXISTS actor_capabilities (
    id VARCHAR(64) PRIMARY KEY,
    kind VARCHAR(32) NOT NULL,
    target VARCHAR(255) NOT NULL,
    secret_hash VARCHAR(128) NOT NULL,
    granted_by VARCHAR(64),
    expires_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_actor_caps_token ON actor_capabilities(kind, target, secret_hash);
CREATE INDEX idx_actor_caps_target ON actor_capabilities(kind, target);

-- migrate:down
DROP TABLE IF EXISTS actor_capabilities;
