-- migrate:up
-- Actor name registry (Erlang register/whereis equivalent)
CREATE TABLE IF NOT EXISTS actor_names (
    name TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    framework TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_actor_names_instance ON actor_names(instance_id);

-- Actor monitors: watchers receive a DOWN message on channel when target terminates
CREATE TABLE IF NOT EXISTS actor_monitors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    watcher_instance_id TEXT NOT NULL,
    target_instance_id TEXT NOT NULL,
    channel TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (watcher_instance_id, target_instance_id)
);

CREATE INDEX IF NOT EXISTS idx_actor_monitors_target ON actor_monitors(target_instance_id);
CREATE INDEX IF NOT EXISTS idx_actor_monitors_watcher ON actor_monitors(watcher_instance_id);

-- migrate:down
DROP TABLE IF EXISTS actor_monitors;
DROP TABLE IF EXISTS actor_names;
