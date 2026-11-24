-- migrate:up
CREATE TABLE IF NOT EXISTS actor_tasks (
    id VARCHAR(64) PRIMARY KEY,
    channel VARCHAR(255) NOT NULL,
    data TEXT,
    state VARCHAR(16) NOT NULL DEFAULT 'open',
    claimed_by VARCHAR(64),
    result TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);
CREATE INDEX idx_actor_tasks_channel_state ON actor_tasks(channel, state);

-- migrate:down
DROP TABLE IF EXISTS actor_tasks;
