-- migrate:up
CREATE TABLE IF NOT EXISTS actor_names (
    name VARCHAR(255) PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    framework VARCHAR(50),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX idx_actor_names_instance ON actor_names(instance_id);

CREATE TABLE IF NOT EXISTS actor_monitors (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    watcher_instance_id VARCHAR(255) NOT NULL,
    target_instance_id VARCHAR(255) NOT NULL,
    channel VARCHAR(255) NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT unique_watcher_target UNIQUE (watcher_instance_id, target_instance_id)
);

CREATE INDEX idx_actor_monitors_target ON actor_monitors(target_instance_id);
CREATE INDEX idx_actor_monitors_watcher ON actor_monitors(watcher_instance_id);

-- migrate:down
DROP TABLE IF EXISTS actor_monitors;
DROP TABLE IF EXISTS actor_names;
