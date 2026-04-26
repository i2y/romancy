-- migrate:up

-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (datetime('now')),
    description TEXT NOT NULL
);

-- Workflow definitions (source code storage)
CREATE TABLE IF NOT EXISTS workflow_definitions (
    workflow_name TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    source_code TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (workflow_name, source_hash)
);

CREATE INDEX IF NOT EXISTS idx_definitions_name ON workflow_definitions(workflow_name);
CREATE INDEX IF NOT EXISTS idx_definitions_hash ON workflow_definitions(source_hash);

-- Workflow instances with distributed locking support
CREATE TABLE IF NOT EXISTS workflow_instances (
    instance_id TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    owner_service TEXT NOT NULL,
    framework TEXT NOT NULL DEFAULT 'python',
    status TEXT NOT NULL DEFAULT 'running',
    current_activity_id TEXT,
    continued_from TEXT,
    started_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    input_data TEXT NOT NULL,
    output_data TEXT,
    locked_by TEXT,
    locked_at TEXT,
    lock_timeout_seconds INTEGER,
    lock_expires_at TEXT,
    CONSTRAINT valid_status CHECK (
        status IN ('running', 'completed', 'failed', 'waiting_for_event', 'waiting_for_timer', 'waiting_for_message', 'compensating', 'cancelled', 'recurred')
    ),
    FOREIGN KEY (continued_from) REFERENCES workflow_instances(instance_id),
    FOREIGN KEY (workflow_name, source_hash) REFERENCES workflow_definitions(workflow_name, source_hash)
);

CREATE INDEX IF NOT EXISTS idx_instances_status ON workflow_instances(status);
CREATE INDEX IF NOT EXISTS idx_instances_workflow ON workflow_instances(workflow_name);
CREATE INDEX IF NOT EXISTS idx_instances_owner ON workflow_instances(owner_service);
CREATE INDEX IF NOT EXISTS idx_instances_framework ON workflow_instances(framework);
CREATE INDEX IF NOT EXISTS idx_instances_locked ON workflow_instances(locked_by, locked_at);
CREATE INDEX IF NOT EXISTS idx_instances_lock_expires ON workflow_instances(lock_expires_at);
CREATE INDEX IF NOT EXISTS idx_instances_updated ON workflow_instances(updated_at);
CREATE INDEX IF NOT EXISTS idx_instances_hash ON workflow_instances(source_hash);
CREATE INDEX IF NOT EXISTS idx_instances_continued_from ON workflow_instances(continued_from);
CREATE INDEX IF NOT EXISTS idx_instances_resumable ON workflow_instances(status, locked_by);

-- Workflow execution history (for deterministic replay)
CREATE TABLE IF NOT EXISTS workflow_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BLOB,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_instance_activity UNIQUE (instance_id, activity_id)
);

CREATE INDEX IF NOT EXISTS idx_history_instance ON workflow_history(instance_id, activity_id);
CREATE INDEX IF NOT EXISTS idx_history_created ON workflow_history(created_at);

-- Archived workflow history (for recur pattern)
CREATE TABLE IF NOT EXISTS workflow_history_archive (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BLOB,
    created_at TEXT NOT NULL,
    archived_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_history_archive_instance ON workflow_history_archive(instance_id);
CREATE INDEX IF NOT EXISTS idx_history_archive_archived ON workflow_history_archive(archived_at);

-- Compensation transactions (LIFO stack for Saga pattern)
CREATE TABLE IF NOT EXISTS workflow_compensations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    activity_name TEXT NOT NULL,
    args TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_compensations_instance ON workflow_compensations(instance_id, created_at DESC);

-- Timer subscriptions (for wait_timer)
CREATE TABLE IF NOT EXISTS workflow_timer_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    timer_id TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    activity_id TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_instance_timer UNIQUE (instance_id, timer_id)
);

CREATE INDEX IF NOT EXISTS idx_timer_subscriptions_expires ON workflow_timer_subscriptions(expires_at);
CREATE INDEX IF NOT EXISTS idx_timer_subscriptions_instance ON workflow_timer_subscriptions(instance_id);

-- Group memberships (Erlang pg style)
CREATE TABLE IF NOT EXISTS workflow_group_memberships (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    group_name TEXT NOT NULL,
    joined_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_instance_group UNIQUE (instance_id, group_name)
);

CREATE INDEX IF NOT EXISTS idx_group_memberships_group ON workflow_group_memberships(group_name);
CREATE INDEX IF NOT EXISTS idx_group_memberships_instance ON workflow_group_memberships(instance_id);

-- Transactional outbox pattern
CREATE TABLE IF NOT EXISTS outbox_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    event_source TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BLOB,
    content_type TEXT NOT NULL DEFAULT 'application/json',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    published_at TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    retry_count INTEGER DEFAULT 0,
    last_error TEXT,
    CONSTRAINT valid_outbox_status CHECK (status IN ('pending', 'processing', 'published', 'failed', 'invalid', 'expired'))
);

CREATE INDEX IF NOT EXISTS idx_outbox_status ON outbox_events(status, created_at);
CREATE INDEX IF NOT EXISTS idx_outbox_retry ON outbox_events(status, retry_count);
CREATE INDEX IF NOT EXISTS idx_outbox_published ON outbox_events(published_at);

-- Channel messages (persistent message queue)
CREATE TABLE IF NOT EXISTS channel_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel TEXT NOT NULL,
    message_id TEXT NOT NULL UNIQUE,
    data_type TEXT NOT NULL,
    data TEXT,
    data_binary BLOB,
    metadata TEXT,
    published_at TEXT NOT NULL DEFAULT (datetime('now')),
    CONSTRAINT valid_data_type CHECK (data_type IN ('json', 'binary')),
    CONSTRAINT data_type_consistency CHECK (
        (data_type = 'json' AND data IS NOT NULL AND data_binary IS NULL) OR
        (data_type = 'binary' AND data IS NULL AND data_binary IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_channel_messages_channel ON channel_messages(channel, published_at);
CREATE INDEX IF NOT EXISTS idx_channel_messages_id ON channel_messages(id);

-- Channel subscriptions
CREATE TABLE IF NOT EXISTS channel_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    channel TEXT NOT NULL,
    mode TEXT NOT NULL,
    activity_id TEXT,
    cursor_message_id INTEGER,
    timeout_at TEXT,
    subscribed_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT valid_mode CHECK (mode IN ('broadcast', 'competing')),
    CONSTRAINT unique_instance_channel UNIQUE (instance_id, channel)
);

CREATE INDEX IF NOT EXISTS idx_channel_subscriptions_channel ON channel_subscriptions(channel);
CREATE INDEX IF NOT EXISTS idx_channel_subscriptions_instance ON channel_subscriptions(instance_id);
CREATE INDEX IF NOT EXISTS idx_channel_subscriptions_waiting ON channel_subscriptions(channel, activity_id);
CREATE INDEX IF NOT EXISTS idx_channel_subscriptions_timeout ON channel_subscriptions(timeout_at);

-- Channel delivery cursors (broadcast mode: track who read what)
CREATE TABLE IF NOT EXISTS channel_delivery_cursors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    last_delivered_id INTEGER NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_channel_instance UNIQUE (channel, instance_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_delivery_cursors_channel ON channel_delivery_cursors(channel);

-- Channel message claims (competing mode: who is processing what)
CREATE TABLE IF NOT EXISTS channel_message_claims (
    message_id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL,
    claimed_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (message_id) REFERENCES channel_messages(message_id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_channel_message_claims_instance ON channel_message_claims(instance_id);

-- System locks (for coordinating background tasks across pods)
CREATE TABLE IF NOT EXISTS system_locks (
    lock_name TEXT PRIMARY KEY,
    locked_by TEXT,
    locked_at TEXT,
    lock_expires_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_system_locks_expires ON system_locks(lock_expires_at);


-- migrate:down

DROP INDEX IF EXISTS idx_channel_message_claims_instance;
DROP TABLE IF EXISTS channel_message_claims;

DROP INDEX IF EXISTS idx_channel_delivery_cursors_channel;
DROP TABLE IF EXISTS channel_delivery_cursors;

DROP INDEX IF EXISTS idx_channel_subscriptions_waiting;
DROP INDEX IF EXISTS idx_channel_subscriptions_instance;
DROP INDEX IF EXISTS idx_channel_subscriptions_channel;
DROP TABLE IF EXISTS channel_subscriptions;

DROP INDEX IF EXISTS idx_channel_messages_id;
DROP INDEX IF EXISTS idx_channel_messages_channel;
DROP TABLE IF EXISTS channel_messages;

DROP INDEX IF EXISTS idx_outbox_published;
DROP INDEX IF EXISTS idx_outbox_retry;
DROP INDEX IF EXISTS idx_outbox_status;
DROP TABLE IF EXISTS outbox_events;

DROP INDEX IF EXISTS idx_group_memberships_instance;
DROP INDEX IF EXISTS idx_group_memberships_group;
DROP TABLE IF EXISTS workflow_group_memberships;

DROP INDEX IF EXISTS idx_timer_subscriptions_instance;
DROP INDEX IF EXISTS idx_timer_subscriptions_expires;
DROP TABLE IF EXISTS workflow_timer_subscriptions;

DROP INDEX IF EXISTS idx_compensations_instance;
DROP TABLE IF EXISTS workflow_compensations;

DROP INDEX IF EXISTS idx_history_archive_archived;
DROP INDEX IF EXISTS idx_history_archive_instance;
DROP TABLE IF EXISTS workflow_history_archive;

DROP INDEX IF EXISTS idx_history_created;
DROP INDEX IF EXISTS idx_history_instance;
DROP TABLE IF EXISTS workflow_history;

DROP INDEX IF EXISTS idx_instances_continued_from;
DROP INDEX IF EXISTS idx_instances_hash;
DROP INDEX IF EXISTS idx_instances_updated;
DROP INDEX IF EXISTS idx_instances_locked;
DROP INDEX IF EXISTS idx_instances_framework;
DROP INDEX IF EXISTS idx_instances_owner;
DROP INDEX IF EXISTS idx_instances_workflow;
DROP INDEX IF EXISTS idx_instances_status;
DROP TABLE IF EXISTS workflow_instances;

DROP INDEX IF EXISTS idx_definitions_hash;
DROP INDEX IF EXISTS idx_definitions_name;
DROP TABLE IF EXISTS workflow_definitions;

DROP TABLE IF EXISTS schema_version;
