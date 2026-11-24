-- migrate:up
CREATE TABLE IF NOT EXISTS actor_tasks (
    id TEXT PRIMARY KEY,
    channel TEXT NOT NULL,
    data TEXT,
    state TEXT NOT NULL DEFAULT 'open',
    claimed_by TEXT,
    result TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_actor_tasks_channel_state ON actor_tasks(channel, state);

-- migrate:down
DROP TABLE IF EXISTS actor_tasks;
