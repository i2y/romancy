-- migrate:up

-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_version (
    version INT PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT NOT NULL
);

-- Workflow definitions (source code storage)
CREATE TABLE IF NOT EXISTS workflow_definitions (
    workflow_name VARCHAR(255) NOT NULL,
    source_hash VARCHAR(64) NOT NULL,
    source_code LONGTEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workflow_name, source_hash)
);

CREATE INDEX idx_definitions_name ON workflow_definitions(workflow_name);
CREATE INDEX idx_definitions_hash ON workflow_definitions(source_hash);

-- Workflow instances with distributed locking support
CREATE TABLE IF NOT EXISTS workflow_instances (
    instance_id VARCHAR(255) PRIMARY KEY,
    workflow_name VARCHAR(255) NOT NULL,
    source_hash VARCHAR(64) NOT NULL,
    owner_service VARCHAR(255) NOT NULL,
    framework VARCHAR(50) NOT NULL DEFAULT 'python',
    status VARCHAR(50) NOT NULL DEFAULT 'running',
    current_activity_id VARCHAR(255),
    continued_from VARCHAR(255),
    started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    input_data LONGTEXT NOT NULL,
    output_data LONGTEXT,
    locked_by VARCHAR(255),
    locked_at TIMESTAMP NULL,
    lock_timeout_seconds INT,
    lock_expires_at TIMESTAMP NULL,
    CONSTRAINT valid_status CHECK (
        status IN ('running', 'completed', 'failed', 'waiting_for_event', 'waiting_for_timer', 'waiting_for_message', 'compensating', 'cancelled', 'recurred')
    ),
    FOREIGN KEY (continued_from) REFERENCES workflow_instances(instance_id),
    FOREIGN KEY (workflow_name, source_hash) REFERENCES workflow_definitions(workflow_name, source_hash)
);

CREATE INDEX idx_instances_status ON workflow_instances(status);
CREATE INDEX idx_instances_workflow ON workflow_instances(workflow_name);
CREATE INDEX idx_instances_owner ON workflow_instances(owner_service);
CREATE INDEX idx_instances_framework ON workflow_instances(framework);
CREATE INDEX idx_instances_locked ON workflow_instances(locked_by, locked_at);
CREATE INDEX idx_instances_lock_expires ON workflow_instances(lock_expires_at);
CREATE INDEX idx_instances_updated ON workflow_instances(updated_at);
CREATE INDEX idx_instances_hash ON workflow_instances(source_hash);
CREATE INDEX idx_instances_continued_from ON workflow_instances(continued_from);
CREATE INDEX idx_instances_resumable ON workflow_instances(status, locked_by);

-- Workflow execution history (for deterministic replay)
CREATE TABLE IF NOT EXISTS workflow_history (
    id INT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    activity_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(100) NOT NULL,
    data_type VARCHAR(10) NOT NULL DEFAULT 'json',
    event_data LONGTEXT,
    event_data_binary LONGBLOB,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    UNIQUE KEY unique_instance_activity (instance_id, activity_id)
);

CREATE INDEX idx_history_instance ON workflow_history(instance_id, activity_id);
CREATE INDEX idx_history_created ON workflow_history(created_at);

-- Archived workflow history (for recur pattern)
CREATE TABLE IF NOT EXISTS workflow_history_archive (
    id INT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    activity_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(100) NOT NULL,
    data_type VARCHAR(10) NOT NULL DEFAULT 'json',
    event_data LONGTEXT,
    event_data_binary LONGBLOB,
    created_at TIMESTAMP NOT NULL,
    archived_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX idx_history_archive_instance ON workflow_history_archive(instance_id);
CREATE INDEX idx_history_archive_archived ON workflow_history_archive(archived_at);

-- Compensation transactions (LIFO stack for Saga pattern)
CREATE TABLE IF NOT EXISTS workflow_compensations (
    id INT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    activity_id VARCHAR(255) NOT NULL,
    activity_name VARCHAR(255) NOT NULL,
    args LONGTEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX idx_compensations_instance ON workflow_compensations(instance_id, created_at DESC);

-- Timer subscriptions (for wait_timer)
CREATE TABLE IF NOT EXISTS workflow_timer_subscriptions (
    id INT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    timer_id VARCHAR(255) NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    activity_id VARCHAR(255),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    UNIQUE KEY unique_instance_timer (instance_id, timer_id)
);

CREATE INDEX idx_timer_subscriptions_expires ON workflow_timer_subscriptions(expires_at);
CREATE INDEX idx_timer_subscriptions_instance ON workflow_timer_subscriptions(instance_id);

-- Group memberships (Erlang pg style)
CREATE TABLE IF NOT EXISTS workflow_group_memberships (
    id INT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    group_name VARCHAR(255) NOT NULL,
    joined_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    UNIQUE KEY unique_instance_group (instance_id, group_name)
);

CREATE INDEX idx_group_memberships_group ON workflow_group_memberships(group_name);
CREATE INDEX idx_group_memberships_instance ON workflow_group_memberships(instance_id);

-- Transactional outbox pattern
CREATE TABLE IF NOT EXISTS outbox_events (
    event_id VARCHAR(255) PRIMARY KEY,
    event_type VARCHAR(255) NOT NULL,
    event_source VARCHAR(255) NOT NULL,
    data_type VARCHAR(10) NOT NULL DEFAULT 'json',
    event_data LONGTEXT,
    event_data_binary LONGBLOB,
    content_type VARCHAR(100) NOT NULL DEFAULT 'application/json',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at TIMESTAMP NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    retry_count INT DEFAULT 0,
    last_error TEXT,
    CONSTRAINT valid_outbox_status CHECK (status IN ('pending', 'processing', 'published', 'failed', 'invalid', 'expired'))
);

CREATE INDEX idx_outbox_status ON outbox_events(status, created_at);
CREATE INDEX idx_outbox_retry ON outbox_events(status, retry_count);
CREATE INDEX idx_outbox_published ON outbox_events(published_at);

-- Channel messages (persistent message queue)
CREATE TABLE IF NOT EXISTS channel_messages (
    id INT AUTO_INCREMENT PRIMARY KEY,
    channel VARCHAR(255) NOT NULL,
    message_id VARCHAR(255) NOT NULL UNIQUE,
    data_type VARCHAR(10) NOT NULL,
    data LONGTEXT,
    data_binary LONGBLOB,
    metadata TEXT,
    published_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT valid_data_type CHECK (data_type IN ('json', 'binary')),
    CONSTRAINT data_type_consistency CHECK (
        (data_type = 'json' AND data IS NOT NULL AND data_binary IS NULL) OR
        (data_type = 'binary' AND data IS NULL AND data_binary IS NOT NULL)
    )
);

CREATE INDEX idx_channel_messages_channel ON channel_messages(channel, published_at);
CREATE INDEX idx_channel_messages_id ON channel_messages(id);

-- Channel subscriptions
CREATE TABLE IF NOT EXISTS channel_subscriptions (
    id INT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    channel VARCHAR(255) NOT NULL,
    mode VARCHAR(20) NOT NULL,
    activity_id VARCHAR(255),
    cursor_message_id INT,
    timeout_at TIMESTAMP NULL,
    subscribed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT valid_mode CHECK (mode IN ('broadcast', 'competing')),
    UNIQUE KEY unique_instance_channel (instance_id, channel)
);

CREATE INDEX idx_channel_subscriptions_channel ON channel_subscriptions(channel);
CREATE INDEX idx_channel_subscriptions_instance ON channel_subscriptions(instance_id);
CREATE INDEX idx_channel_subscriptions_waiting ON channel_subscriptions(channel, activity_id);
CREATE INDEX idx_channel_subscriptions_timeout ON channel_subscriptions(timeout_at);

-- Channel delivery cursors (broadcast mode: track who read what)
CREATE TABLE IF NOT EXISTS channel_delivery_cursors (
    id INT AUTO_INCREMENT PRIMARY KEY,
    channel VARCHAR(255) NOT NULL,
    instance_id VARCHAR(255) NOT NULL,
    last_delivered_id INT NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    UNIQUE KEY unique_channel_instance (channel, instance_id)
);

CREATE INDEX idx_channel_delivery_cursors_channel ON channel_delivery_cursors(channel);

-- Channel message claims (competing mode: who is processing what)
CREATE TABLE IF NOT EXISTS channel_message_claims (
    message_id VARCHAR(255) PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    claimed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (message_id) REFERENCES channel_messages(message_id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX idx_channel_message_claims_instance ON channel_message_claims(instance_id);

-- System locks (for coordinating background tasks across pods)
CREATE TABLE IF NOT EXISTS system_locks (
    lock_name VARCHAR(255) PRIMARY KEY,
    locked_by VARCHAR(255),
    locked_at TIMESTAMP NULL,
    lock_expires_at TIMESTAMP NULL
);

CREATE INDEX idx_system_locks_expires ON system_locks(lock_expires_at);


-- migrate:down

DROP INDEX idx_channel_message_claims_instance ON channel_message_claims;
DROP TABLE IF EXISTS channel_message_claims;

DROP INDEX idx_channel_delivery_cursors_channel ON channel_delivery_cursors;
DROP TABLE IF EXISTS channel_delivery_cursors;

DROP INDEX idx_channel_subscriptions_waiting ON channel_subscriptions;
DROP INDEX idx_channel_subscriptions_instance ON channel_subscriptions;
DROP INDEX idx_channel_subscriptions_channel ON channel_subscriptions;
DROP TABLE IF EXISTS channel_subscriptions;

DROP INDEX idx_channel_messages_id ON channel_messages;
DROP INDEX idx_channel_messages_channel ON channel_messages;
DROP TABLE IF EXISTS channel_messages;

DROP INDEX idx_outbox_published ON outbox_events;
DROP INDEX idx_outbox_retry ON outbox_events;
DROP INDEX idx_outbox_status ON outbox_events;
DROP TABLE IF EXISTS outbox_events;

DROP INDEX idx_group_memberships_instance ON workflow_group_memberships;
DROP INDEX idx_group_memberships_group ON workflow_group_memberships;
DROP TABLE IF EXISTS workflow_group_memberships;

DROP INDEX idx_timer_subscriptions_instance ON workflow_timer_subscriptions;
DROP INDEX idx_timer_subscriptions_expires ON workflow_timer_subscriptions;
DROP TABLE IF EXISTS workflow_timer_subscriptions;

DROP INDEX idx_compensations_instance ON workflow_compensations;
DROP TABLE IF EXISTS workflow_compensations;

DROP INDEX idx_history_archive_archived ON workflow_history_archive;
DROP INDEX idx_history_archive_instance ON workflow_history_archive;
DROP TABLE IF EXISTS workflow_history_archive;

DROP INDEX idx_history_created ON workflow_history;
DROP INDEX idx_history_instance ON workflow_history;
DROP TABLE IF EXISTS workflow_history;

DROP INDEX idx_instances_continued_from ON workflow_instances;
DROP INDEX idx_instances_hash ON workflow_instances;
DROP INDEX idx_instances_updated ON workflow_instances;
DROP INDEX idx_instances_locked ON workflow_instances;
DROP INDEX idx_instances_framework ON workflow_instances;
DROP INDEX idx_instances_owner ON workflow_instances;
DROP INDEX idx_instances_workflow ON workflow_instances;
DROP INDEX idx_instances_status ON workflow_instances;
DROP TABLE IF EXISTS workflow_instances;

DROP INDEX idx_definitions_hash ON workflow_definitions;
DROP INDEX idx_definitions_name ON workflow_definitions;
DROP TABLE IF EXISTS workflow_definitions;

DROP TABLE IF EXISTS schema_version;
