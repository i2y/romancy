-- Revert unified schema changes (PostgreSQL)
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

ALTER TABLE workflow_outbox DROP COLUMN IF EXISTS published_at;
ALTER TABLE workflow_outbox DROP COLUMN IF EXISTS last_error;
ALTER TABLE workflow_outbox DROP COLUMN IF EXISTS event_data_binary;

ALTER TABLE workflow_outbox RENAME COLUMN retry_count TO attempts;

-- ============================================================================
-- channel_message_claims: Revert changes
-- ============================================================================

ALTER TABLE channel_message_claims DROP COLUMN IF EXISTS message_id_text;

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

ALTER TABLE channel_subscriptions DROP COLUMN IF EXISTS subscribed_at;
ALTER TABLE channel_subscriptions DROP COLUMN IF EXISTS cursor_message_id;
ALTER TABLE channel_subscriptions RENAME COLUMN channel TO channel_name;

CREATE INDEX IF NOT EXISTS idx_channel_subs_channel ON channel_subscriptions(channel_name);
CREATE INDEX IF NOT EXISTS idx_channel_subs_waiting ON channel_subscriptions(waiting);

-- ============================================================================
-- channel_messages: Revert column changes
-- ============================================================================

DROP INDEX IF EXISTS idx_channel_messages_channel;

ALTER TABLE channel_messages DROP COLUMN IF EXISTS published_at;
ALTER TABLE channel_messages DROP COLUMN IF EXISTS data_type;
ALTER TABLE channel_messages DROP COLUMN IF EXISTS message_id;
ALTER TABLE channel_messages DROP COLUMN IF EXISTS data;
ALTER TABLE channel_messages RENAME COLUMN channel TO channel_name;

CREATE INDEX IF NOT EXISTS idx_channel_messages_channel ON channel_messages(channel_name);

-- ============================================================================
-- workflow_compensations: Revert column renames
-- ============================================================================

ALTER TABLE workflow_compensations RENAME COLUMN args TO compensation_arg;
ALTER TABLE workflow_compensations RENAME COLUMN activity_name TO compensation_fn;

-- ============================================================================
-- workflow_group_memberships: Revert changes
-- ============================================================================

ALTER TABLE workflow_group_memberships DROP COLUMN IF EXISTS joined_at;

-- ============================================================================
-- workflow_timer_subscriptions: Revert changes
-- ============================================================================

ALTER TABLE workflow_timer_subscriptions DROP COLUMN IF EXISTS activity_id;

-- ============================================================================
-- workflow_instances: Revert changes
-- ============================================================================

DROP INDEX IF EXISTS idx_instances_started_at;
DROP INDEX IF EXISTS idx_instances_owner;
DROP INDEX IF EXISTS idx_instances_hash;

ALTER TABLE workflow_instances DROP COLUMN IF EXISTS started_at;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS owner_service;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS source_hash;

-- ============================================================================
-- Drop new tables
-- ============================================================================

DROP TABLE IF EXISTS schema_version;
DROP TABLE IF EXISTS workflow_definitions;
