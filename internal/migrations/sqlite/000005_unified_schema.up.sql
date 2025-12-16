-- Unified schema for Romancy-Edda compatibility (SQLite)
-- Version: 000005
-- Description: Unifies schema with Edda for cross-framework compatibility

-- ============================================================================
-- New Tables
-- ============================================================================

-- Workflow Definitions (Edda feature, optional for Romancy)
CREATE TABLE IF NOT EXISTS workflow_definitions (
    workflow_name TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    source_code TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (workflow_name, source_hash)
);

CREATE INDEX IF NOT EXISTS idx_definitions_name ON workflow_definitions(workflow_name);
CREATE INDEX IF NOT EXISTS idx_definitions_hash ON workflow_definitions(source_hash);

-- Schema Version (Edda compatibility)
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (datetime('now')),
    description TEXT NOT NULL
);

-- ============================================================================
-- workflow_instances: Add new columns, add started_at
-- ============================================================================

ALTER TABLE workflow_instances ADD COLUMN source_hash TEXT;
ALTER TABLE workflow_instances ADD COLUMN owner_service TEXT;
ALTER TABLE workflow_instances ADD COLUMN started_at TEXT;

-- Migrate data: created_at -> started_at
UPDATE workflow_instances SET started_at = COALESCE(created_at, datetime('now')) WHERE started_at IS NULL;

-- Add indexes for new columns
CREATE INDEX IF NOT EXISTS idx_instances_hash ON workflow_instances(source_hash);
CREATE INDEX IF NOT EXISTS idx_instances_owner ON workflow_instances(owner_service);
CREATE INDEX IF NOT EXISTS idx_instances_started_at ON workflow_instances(started_at DESC, instance_id);

-- ============================================================================
-- workflow_timer_subscriptions: Add activity_id
-- ============================================================================

ALTER TABLE workflow_timer_subscriptions ADD COLUMN activity_id TEXT;

-- ============================================================================
-- workflow_group_memberships: Add joined_at
-- ============================================================================

ALTER TABLE workflow_group_memberships ADD COLUMN joined_at TEXT;
UPDATE workflow_group_memberships SET joined_at = COALESCE(created_at, datetime('now')) WHERE joined_at IS NULL;

-- ============================================================================
-- workflow_compensations: Rename columns (SQLite 3.25.0+)
-- ============================================================================

ALTER TABLE workflow_compensations RENAME COLUMN compensation_fn TO activity_name;
ALTER TABLE workflow_compensations RENAME COLUMN compensation_arg TO args;

-- ============================================================================
-- channel_messages: Rename columns and add new ones
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_messages RENAME COLUMN channel_name TO channel;

-- Add new columns
ALTER TABLE channel_messages ADD COLUMN data TEXT;
ALTER TABLE channel_messages ADD COLUMN message_id TEXT;
ALTER TABLE channel_messages ADD COLUMN data_type TEXT DEFAULT 'json';
ALTER TABLE channel_messages ADD COLUMN published_at TEXT;

-- Migrate data
UPDATE channel_messages SET data = data_json WHERE data IS NULL AND data_json IS NOT NULL;
UPDATE channel_messages SET message_id = lower(hex(randomblob(16))) WHERE message_id IS NULL;
UPDATE channel_messages SET data_type = 'json' WHERE data_type IS NULL;
UPDATE channel_messages SET published_at = COALESCE(created_at, datetime('now')) WHERE published_at IS NULL;

-- Update indexes
DROP INDEX IF EXISTS idx_channel_messages_channel;
CREATE INDEX IF NOT EXISTS idx_channel_messages_channel ON channel_messages(channel, published_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_messages_message_id ON channel_messages(message_id);

-- ============================================================================
-- channel_subscriptions: Rename columns and add new ones
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_subscriptions RENAME COLUMN channel_name TO channel;

-- Add new columns
ALTER TABLE channel_subscriptions ADD COLUMN cursor_message_id INTEGER;
ALTER TABLE channel_subscriptions ADD COLUMN subscribed_at TEXT;

-- Migrate data
UPDATE channel_subscriptions SET subscribed_at = COALESCE(created_at, datetime('now')) WHERE subscribed_at IS NULL;

-- Update indexes
DROP INDEX IF EXISTS idx_channel_subs_channel;
DROP INDEX IF EXISTS idx_channel_subs_waiting;
CREATE INDEX IF NOT EXISTS idx_channel_subs_channel ON channel_subscriptions(channel);
CREATE INDEX IF NOT EXISTS idx_channel_subs_waiting ON channel_subscriptions(channel, waiting);
CREATE INDEX IF NOT EXISTS idx_channel_subs_channel_activity ON channel_subscriptions(channel, activity_id);

-- ============================================================================
-- channel_delivery_cursors: Rename columns
-- ============================================================================

ALTER TABLE channel_delivery_cursors RENAME COLUMN channel_name TO channel;
ALTER TABLE channel_delivery_cursors RENAME COLUMN last_message_id TO last_delivered_id;

-- ============================================================================
-- channel_message_claims: Add text-based message_id
-- ============================================================================

ALTER TABLE channel_message_claims ADD COLUMN message_id_text TEXT;

-- Migrate existing references
UPDATE channel_message_claims SET message_id_text = (
    SELECT message_id FROM channel_messages WHERE channel_messages.id = channel_message_claims.message_id
) WHERE message_id_text IS NULL;

-- ============================================================================
-- workflow_outbox: Rename columns and add new ones
-- ============================================================================

ALTER TABLE workflow_outbox RENAME COLUMN attempts TO retry_count;

-- Add new columns
ALTER TABLE workflow_outbox ADD COLUMN event_data_binary BLOB;
ALTER TABLE workflow_outbox ADD COLUMN last_error TEXT;
ALTER TABLE workflow_outbox ADD COLUMN published_at TEXT;

-- Add indexes
CREATE INDEX IF NOT EXISTS idx_outbox_retry ON workflow_outbox(status, retry_count);
CREATE INDEX IF NOT EXISTS idx_outbox_published ON workflow_outbox(published_at);

-- ============================================================================
-- Record schema version
-- ============================================================================

INSERT OR IGNORE INTO schema_version (version, description)
VALUES (5, 'Unified schema with Edda for cross-framework compatibility');
