-- Revert unified schema changes (SQLite)
-- Version: 000005

-- ============================================================================
-- Remove schema version record
-- ============================================================================

DELETE FROM schema_version WHERE version = 5;

-- ============================================================================
-- workflow_outbox: Revert column changes
-- ============================================================================

DROP INDEX IF EXISTS idx_outbox_published;
DROP INDEX IF EXISTS idx_outbox_retry;

-- SQLite doesn't support DROP COLUMN before 3.35.0, so we need to recreate the table
-- For compatibility, we just rename the column back
ALTER TABLE workflow_outbox RENAME COLUMN retry_count TO attempts;

-- Note: new columns (event_data_binary, last_error, published_at) will remain
-- as SQLite doesn't support DROP COLUMN in older versions

-- ============================================================================
-- channel_message_claims: Note - cannot drop column in older SQLite
-- ============================================================================

-- message_id_text column will remain

-- ============================================================================
-- channel_delivery_cursors: Revert column renames
-- ============================================================================

ALTER TABLE channel_delivery_cursors RENAME COLUMN last_delivered_id TO last_message_id;
ALTER TABLE channel_delivery_cursors RENAME COLUMN channel TO channel_name;

-- ============================================================================
-- channel_subscriptions: Revert column changes
-- ============================================================================

DROP INDEX IF EXISTS idx_channel_subs_channel_activity;
DROP INDEX IF EXISTS idx_channel_subs_waiting;
DROP INDEX IF EXISTS idx_channel_subs_channel;

ALTER TABLE channel_subscriptions RENAME COLUMN channel TO channel_name;

CREATE INDEX IF NOT EXISTS idx_channel_subs_channel ON channel_subscriptions(channel_name);
CREATE INDEX IF NOT EXISTS idx_channel_subs_waiting ON channel_subscriptions(waiting);

-- Note: subscribed_at and cursor_message_id columns will remain

-- ============================================================================
-- channel_messages: Revert column changes
-- ============================================================================

DROP INDEX IF EXISTS idx_channel_messages_channel;

ALTER TABLE channel_messages RENAME COLUMN channel TO channel_name;

CREATE INDEX IF NOT EXISTS idx_channel_messages_channel ON channel_messages(channel_name);

-- Note: data, message_id, data_type, published_at columns will remain

-- ============================================================================
-- workflow_compensations: Revert column renames
-- ============================================================================

ALTER TABLE workflow_compensations RENAME COLUMN args TO compensation_arg;
ALTER TABLE workflow_compensations RENAME COLUMN activity_name TO compensation_fn;

-- ============================================================================
-- workflow_group_memberships: Note - cannot drop column in older SQLite
-- ============================================================================

-- joined_at column will remain

-- ============================================================================
-- workflow_timer_subscriptions: Note - cannot drop column in older SQLite
-- ============================================================================

-- activity_id column will remain

-- ============================================================================
-- workflow_instances: Revert changes
-- ============================================================================

DROP INDEX IF EXISTS idx_instances_started_at;
DROP INDEX IF EXISTS idx_instances_owner;
DROP INDEX IF EXISTS idx_instances_hash;

-- Note: source_hash, owner_service, started_at columns will remain

-- ============================================================================
-- Drop new tables
-- ============================================================================

DROP TABLE IF EXISTS schema_version;
DROP TABLE IF EXISTS workflow_definitions;
