// Package storage provides the storage layer for Romancy.
package storage

import (
	"context"
)

// testSchema is the schema SQL for tests.
// This is derived from schema/db/migrations/sqlite.
const testSchemaSQLite = `
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
    source_hash TEXT NOT NULL DEFAULT '',
    owner_service TEXT NOT NULL DEFAULT '',
    framework TEXT NOT NULL DEFAULT 'go',
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
    )
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
    original_id INTEGER,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BLOB,
    original_created_at TEXT,
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
`

// InitializeTestSchema applies the test schema to an SQLiteStorage.
// This is intended for use in tests only.
func InitializeTestSchema(ctx context.Context, s *SQLiteStorage) error {
	_, err := s.DB().ExecContext(ctx, testSchemaSQLite)
	return err
}

// InitializeTestSchemaForStorage initializes the test schema for any Storage interface.
// This is useful for external test packages.
func InitializeTestSchemaForStorage(ctx context.Context, s Storage) error {
	if sqliteStorage, ok := s.(*SQLiteStorage); ok {
		return InitializeTestSchema(ctx, sqliteStorage)
	}
	if pgStorage, ok := s.(*PostgresStorage); ok {
		return InitializeTestSchemaPostgres(ctx, pgStorage)
	}
	if mysqlStorage, ok := s.(*MySQLStorage); ok {
		return InitializeTestSchemaMySQL(ctx, mysqlStorage)
	}
	return nil
}

// testSchemaPostgres is the schema SQL for PostgreSQL tests.
// Derived from schema/db/migrations/postgresql, without workflow_definitions FK.
const testSchemaPostgres = `
-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT NOW(),
    description TEXT NOT NULL
);

-- Workflow definitions (source code storage)
CREATE TABLE IF NOT EXISTS workflow_definitions (
    workflow_name TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    source_code TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workflow_name, source_hash)
);

CREATE INDEX IF NOT EXISTS idx_definitions_name ON workflow_definitions(workflow_name);
CREATE INDEX IF NOT EXISTS idx_definitions_hash ON workflow_definitions(source_hash);

-- Workflow instances with distributed locking support
CREATE TABLE IF NOT EXISTS workflow_instances (
    instance_id TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    source_hash TEXT NOT NULL DEFAULT '',
    owner_service TEXT NOT NULL DEFAULT '',
    framework TEXT NOT NULL DEFAULT 'go',
    status TEXT NOT NULL DEFAULT 'running',
    current_activity_id TEXT,
    continued_from TEXT,
    started_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    input_data TEXT NOT NULL,
    output_data TEXT,
    locked_by TEXT,
    locked_at TIMESTAMP,
    lock_timeout_seconds INTEGER,
    lock_expires_at TIMESTAMP,
    CONSTRAINT valid_status CHECK (
        status IN ('running', 'completed', 'failed', 'waiting_for_event', 'waiting_for_timer', 'waiting_for_message', 'compensating', 'cancelled', 'recurred')
    )
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
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BYTEA,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_instance_activity UNIQUE (instance_id, activity_id)
);

CREATE INDEX IF NOT EXISTS idx_history_instance ON workflow_history(instance_id, activity_id);
CREATE INDEX IF NOT EXISTS idx_history_created ON workflow_history(created_at);

-- Archived workflow history (for recur pattern)
CREATE TABLE IF NOT EXISTS workflow_history_archive (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BYTEA,
    created_at TIMESTAMP NOT NULL,
    archived_at TIMESTAMP NOT NULL DEFAULT NOW(),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_history_archive_instance ON workflow_history_archive(instance_id);
CREATE INDEX IF NOT EXISTS idx_history_archive_archived ON workflow_history_archive(archived_at);

-- Compensation transactions (LIFO stack for Saga pattern)
CREATE TABLE IF NOT EXISTS workflow_compensations (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    activity_name TEXT NOT NULL,
    args TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_compensations_instance ON workflow_compensations(instance_id, created_at DESC);

-- Timer subscriptions (for wait_timer)
CREATE TABLE IF NOT EXISTS workflow_timer_subscriptions (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL,
    timer_id TEXT NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    activity_id TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_instance_timer UNIQUE (instance_id, timer_id)
);

CREATE INDEX IF NOT EXISTS idx_timer_subscriptions_expires ON workflow_timer_subscriptions(expires_at);
CREATE INDEX IF NOT EXISTS idx_timer_subscriptions_instance ON workflow_timer_subscriptions(instance_id);

-- Group memberships (Erlang pg style)
CREATE TABLE IF NOT EXISTS workflow_group_memberships (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL,
    group_name TEXT NOT NULL,
    joined_at TIMESTAMP NOT NULL DEFAULT NOW(),
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
    event_data_binary BYTEA,
    content_type TEXT NOT NULL DEFAULT 'application/json',
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    published_at TIMESTAMP,
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
    id SERIAL PRIMARY KEY,
    channel TEXT NOT NULL,
    message_id TEXT NOT NULL UNIQUE,
    data_type TEXT NOT NULL,
    data TEXT,
    data_binary BYTEA,
    metadata TEXT,
    published_at TIMESTAMP NOT NULL DEFAULT NOW(),
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
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL,
    channel TEXT NOT NULL,
    mode TEXT NOT NULL,
    activity_id TEXT,
    cursor_message_id INTEGER,
    timeout_at TIMESTAMP,
    subscribed_at TIMESTAMP NOT NULL DEFAULT NOW(),
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
    id SERIAL PRIMARY KEY,
    channel TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    last_delivered_id INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE,
    CONSTRAINT unique_channel_instance UNIQUE (channel, instance_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_delivery_cursors_channel ON channel_delivery_cursors(channel);

-- Channel message claims (competing mode: who is processing what)
CREATE TABLE IF NOT EXISTS channel_message_claims (
    message_id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL,
    claimed_at TIMESTAMP NOT NULL DEFAULT NOW(),
    FOREIGN KEY (message_id) REFERENCES channel_messages(message_id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_channel_message_claims_instance ON channel_message_claims(instance_id);

-- System locks (for coordinating background tasks across pods)
CREATE TABLE IF NOT EXISTS system_locks (
    lock_name TEXT PRIMARY KEY,
    locked_by TEXT,
    locked_at TIMESTAMP,
    lock_expires_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_system_locks_expires ON system_locks(lock_expires_at);
`

// InitializeTestSchemaPostgres applies the test schema to a PostgresStorage.
// This is intended for use in tests only.
func InitializeTestSchemaPostgres(ctx context.Context, s *PostgresStorage) error {
	_, err := s.DB().ExecContext(ctx, testSchemaPostgres)
	return err
}

// testSchemaMySQL is the schema SQL for MySQL tests.
// Derived from schema/db/migrations/mysql, without workflow_definitions FK.
const testSchemaMySQL = `
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
    source_hash VARCHAR(64) NOT NULL DEFAULT '',
    owner_service VARCHAR(255) NOT NULL DEFAULT '',
    framework VARCHAR(50) NOT NULL DEFAULT 'go',
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
    )
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
`

// InitializeTestSchemaMySQL applies the test schema to a MySQLStorage.
// This is intended for use in tests only.
func InitializeTestSchemaMySQL(ctx context.Context, s *MySQLStorage) error {
	_, err := s.DB().ExecContext(ctx, testSchemaMySQL)
	return err
}
