-- v0.8.0 schema for Romancy (SQLite)
-- Version: 000002
-- Description: Adds channels, recur, groups, and pagination support

-- Add continued_from column for recur pattern
ALTER TABLE workflow_instances ADD COLUMN continued_from TEXT;

-- Add index for pagination (started_at, instance_id composite cursor)
CREATE INDEX IF NOT EXISTS idx_instances_created_at ON workflow_instances(created_at DESC, instance_id);

-- ========================================
-- Channel Messages
-- ========================================
CREATE TABLE IF NOT EXISTS channel_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_name TEXT NOT NULL,
    data_json TEXT,
    data_binary BLOB,
    metadata TEXT,
    target_instance_id TEXT,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_channel_messages_channel ON channel_messages(channel_name);
CREATE INDEX IF NOT EXISTS idx_channel_messages_target ON channel_messages(target_instance_id);
CREATE INDEX IF NOT EXISTS idx_channel_messages_created ON channel_messages(created_at);

-- ========================================
-- Channel Subscriptions
-- ========================================
CREATE TABLE IF NOT EXISTS channel_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    channel_name TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('broadcast', 'competing')),
    waiting INTEGER NOT NULL DEFAULT 0,
    timeout_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id),
    UNIQUE(instance_id, channel_name)
);

CREATE INDEX IF NOT EXISTS idx_channel_subs_channel ON channel_subscriptions(channel_name);
CREATE INDEX IF NOT EXISTS idx_channel_subs_waiting ON channel_subscriptions(waiting);
CREATE INDEX IF NOT EXISTS idx_channel_subs_timeout ON channel_subscriptions(timeout_at);

-- ========================================
-- Channel Delivery Cursors (for broadcast mode)
-- ========================================
CREATE TABLE IF NOT EXISTS channel_delivery_cursors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    channel_name TEXT NOT NULL,
    last_message_id INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id),
    UNIQUE(instance_id, channel_name)
);

-- ========================================
-- Channel Message Claims (for competing mode)
-- ========================================
CREATE TABLE IF NOT EXISTS channel_message_claims (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL UNIQUE,
    instance_id TEXT NOT NULL,
    claimed_at DATETIME NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (message_id) REFERENCES channel_messages(id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
);

-- ========================================
-- Workflow History Archive (for recur pattern)
-- ========================================
CREATE TABLE IF NOT EXISTS workflow_history_archive (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    original_id INTEGER,
    instance_id TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_data TEXT,
    event_data_binary BLOB,
    data_type TEXT NOT NULL DEFAULT 'json',
    original_created_at DATETIME,
    archived_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_history_archive_instance ON workflow_history_archive(instance_id);

-- ========================================
-- Group Memberships (Erlang pg style)
-- ========================================
CREATE TABLE IF NOT EXISTS workflow_group_memberships (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    group_name TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id),
    UNIQUE(instance_id, group_name)
);

CREATE INDEX IF NOT EXISTS idx_group_memberships_group ON workflow_group_memberships(group_name);

-- ========================================
-- Message Subscriptions (channel message wait)
-- ========================================
CREATE TABLE IF NOT EXISTS workflow_message_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id TEXT NOT NULL,
    channel_name TEXT NOT NULL,
    timeout_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id),
    UNIQUE(instance_id, channel_name)
);

CREATE INDEX IF NOT EXISTS idx_message_subs_channel ON workflow_message_subscriptions(channel_name);
CREATE INDEX IF NOT EXISTS idx_message_subs_timeout ON workflow_message_subscriptions(timeout_at);

-- ========================================
-- System Locks (for background tasks)
-- ========================================
CREATE TABLE IF NOT EXISTS system_locks (
    lock_name TEXT PRIMARY KEY,
    locked_by TEXT NOT NULL,
    locked_at DATETIME NOT NULL DEFAULT (datetime('now')),
    expires_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_system_locks_expires ON system_locks(expires_at);
