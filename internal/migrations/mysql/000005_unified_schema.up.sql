-- Unified schema for Romancy-Edda compatibility (MySQL)
-- Version: 000005
-- Description: Unifies schema with Edda for cross-framework compatibility

-- ============================================================================
-- New Tables
-- ============================================================================

-- Workflow Definitions (Edda feature, optional for Romancy)
CREATE TABLE IF NOT EXISTS workflow_definitions (
    workflow_name VARCHAR(255) NOT NULL,
    source_hash VARCHAR(255) NOT NULL,
    source_code LONGTEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workflow_name, source_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_definitions_name ON workflow_definitions(workflow_name);
CREATE INDEX idx_definitions_hash ON workflow_definitions(source_hash);

-- Schema Version (Edda compatibility)
CREATE TABLE IF NOT EXISTS schema_version (
    version INT PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ============================================================================
-- workflow_instances: Add new columns, add started_at
-- ============================================================================

ALTER TABLE workflow_instances ADD COLUMN source_hash VARCHAR(255);
ALTER TABLE workflow_instances ADD COLUMN owner_service VARCHAR(255);
ALTER TABLE workflow_instances ADD COLUMN started_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP;

-- Migrate data: created_at -> started_at
UPDATE workflow_instances SET started_at = created_at WHERE started_at IS NULL;

-- Modify started_at to NOT NULL
ALTER TABLE workflow_instances MODIFY COLUMN started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Add indexes for new columns
CREATE INDEX idx_instances_hash ON workflow_instances(source_hash);
CREATE INDEX idx_instances_owner ON workflow_instances(owner_service);
CREATE INDEX idx_instances_started_at ON workflow_instances(started_at DESC, instance_id(191));

-- ============================================================================
-- workflow_timer_subscriptions: Add activity_id
-- ============================================================================

ALTER TABLE workflow_timer_subscriptions ADD COLUMN activity_id VARCHAR(255);

-- ============================================================================
-- workflow_group_memberships: Add joined_at
-- ============================================================================

ALTER TABLE workflow_group_memberships ADD COLUMN joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP;
UPDATE workflow_group_memberships SET joined_at = created_at WHERE joined_at IS NULL;
ALTER TABLE workflow_group_memberships MODIFY COLUMN joined_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- ============================================================================
-- workflow_compensations: Rename columns (MySQL 8.0+)
-- ============================================================================

ALTER TABLE workflow_compensations RENAME COLUMN compensation_fn TO activity_name;
ALTER TABLE workflow_compensations RENAME COLUMN compensation_arg TO args;

-- ============================================================================
-- channel_messages: Rename columns and add new ones
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_messages RENAME COLUMN channel_name TO channel;

-- Add new columns
ALTER TABLE channel_messages ADD COLUMN data LONGTEXT;
ALTER TABLE channel_messages ADD COLUMN message_id VARCHAR(255) UNIQUE;
ALTER TABLE channel_messages ADD COLUMN data_type VARCHAR(50) NOT NULL DEFAULT 'json';
ALTER TABLE channel_messages ADD COLUMN published_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP;

-- Migrate data
UPDATE channel_messages SET data = data_json WHERE data IS NULL AND data_json IS NOT NULL;
UPDATE channel_messages SET message_id = UUID() WHERE message_id IS NULL;
UPDATE channel_messages SET published_at = created_at WHERE published_at IS NULL;
ALTER TABLE channel_messages MODIFY COLUMN published_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Update indexes
DROP INDEX idx_channel_messages_channel ON channel_messages;
CREATE INDEX idx_channel_messages_channel ON channel_messages(channel(191), published_at);

-- ============================================================================
-- channel_subscriptions: Rename columns and add new ones
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_subscriptions RENAME COLUMN channel_name TO channel;

-- Add new columns
ALTER TABLE channel_subscriptions ADD COLUMN cursor_message_id INT;
ALTER TABLE channel_subscriptions ADD COLUMN subscribed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP;

-- Migrate data
UPDATE channel_subscriptions SET subscribed_at = created_at WHERE subscribed_at IS NULL;
ALTER TABLE channel_subscriptions MODIFY COLUMN subscribed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Update indexes
DROP INDEX idx_channel_subs_channel ON channel_subscriptions;
DROP INDEX idx_channel_subs_waiting ON channel_subscriptions;
CREATE INDEX idx_channel_subs_channel ON channel_subscriptions(channel(191));
CREATE INDEX idx_channel_subs_waiting ON channel_subscriptions(channel(191), waiting);
CREATE INDEX idx_channel_subs_channel_activity ON channel_subscriptions(channel(191), activity_id(191));

-- ============================================================================
-- channel_delivery_cursors: Rename columns
-- ============================================================================

ALTER TABLE channel_delivery_cursors RENAME COLUMN channel_name TO channel;
ALTER TABLE channel_delivery_cursors RENAME COLUMN last_message_id TO last_delivered_id;

-- ============================================================================
-- channel_message_claims: Add text-based message_id
-- ============================================================================

ALTER TABLE channel_message_claims ADD COLUMN message_id_text VARCHAR(255);

-- Migrate existing references
UPDATE channel_message_claims cmc
JOIN channel_messages cm ON cmc.message_id = cm.id
SET cmc.message_id_text = cm.message_id
WHERE cmc.message_id_text IS NULL;

-- ============================================================================
-- workflow_outbox: Rename columns and add new ones
-- ============================================================================

ALTER TABLE workflow_outbox RENAME COLUMN attempts TO retry_count;

-- Add new columns
ALTER TABLE workflow_outbox ADD COLUMN event_data_binary LONGBLOB;
ALTER TABLE workflow_outbox ADD COLUMN last_error TEXT;
ALTER TABLE workflow_outbox ADD COLUMN published_at TIMESTAMP NULL;

-- Add indexes
CREATE INDEX idx_outbox_retry ON workflow_outbox(status, retry_count);
CREATE INDEX idx_outbox_published ON workflow_outbox(published_at);

-- ============================================================================
-- Record schema version
-- ============================================================================

INSERT IGNORE INTO schema_version (version, description)
VALUES (5, 'Unified schema with Edda for cross-framework compatibility');
