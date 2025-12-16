-- Revert unified schema changes (MySQL)
-- Version: 000005

-- ============================================================================
-- Remove schema version record
-- ============================================================================

DELETE FROM schema_version WHERE version = 5;

-- ============================================================================
-- workflow_outbox: Revert column changes
-- ============================================================================

DROP INDEX idx_outbox_published ON workflow_outbox;
DROP INDEX idx_outbox_retry ON workflow_outbox;

ALTER TABLE workflow_outbox DROP COLUMN published_at;
ALTER TABLE workflow_outbox DROP COLUMN last_error;
ALTER TABLE workflow_outbox DROP COLUMN event_data_binary;

ALTER TABLE workflow_outbox RENAME COLUMN retry_count TO attempts;

-- ============================================================================
-- channel_message_claims: Revert changes
-- ============================================================================

ALTER TABLE channel_message_claims DROP COLUMN message_id_text;

-- ============================================================================
-- channel_delivery_cursors: Revert column renames
-- ============================================================================

ALTER TABLE channel_delivery_cursors RENAME COLUMN last_delivered_id TO last_message_id;
ALTER TABLE channel_delivery_cursors RENAME COLUMN channel TO channel_name;

-- ============================================================================
-- channel_subscriptions: Revert column changes
-- ============================================================================

DROP INDEX idx_channel_subs_channel_activity ON channel_subscriptions;
DROP INDEX idx_channel_subs_waiting ON channel_subscriptions;
DROP INDEX idx_channel_subs_channel ON channel_subscriptions;

ALTER TABLE channel_subscriptions DROP COLUMN subscribed_at;
ALTER TABLE channel_subscriptions DROP COLUMN cursor_message_id;
ALTER TABLE channel_subscriptions RENAME COLUMN channel TO channel_name;

CREATE INDEX idx_channel_subs_channel ON channel_subscriptions(channel_name(191));
CREATE INDEX idx_channel_subs_waiting ON channel_subscriptions(waiting);

-- ============================================================================
-- channel_messages: Revert column changes
-- ============================================================================

DROP INDEX idx_channel_messages_channel ON channel_messages;

ALTER TABLE channel_messages DROP COLUMN published_at;
ALTER TABLE channel_messages DROP COLUMN data_type;
ALTER TABLE channel_messages DROP COLUMN message_id;
ALTER TABLE channel_messages DROP COLUMN data;
ALTER TABLE channel_messages RENAME COLUMN channel TO channel_name;

CREATE INDEX idx_channel_messages_channel ON channel_messages(channel_name(191));

-- ============================================================================
-- workflow_compensations: Revert column renames
-- ============================================================================

ALTER TABLE workflow_compensations RENAME COLUMN args TO compensation_arg;
ALTER TABLE workflow_compensations RENAME COLUMN activity_name TO compensation_fn;

-- ============================================================================
-- workflow_group_memberships: Revert changes
-- ============================================================================

ALTER TABLE workflow_group_memberships DROP COLUMN joined_at;

-- ============================================================================
-- workflow_timer_subscriptions: Revert changes
-- ============================================================================

ALTER TABLE workflow_timer_subscriptions DROP COLUMN activity_id;

-- ============================================================================
-- workflow_instances: Revert changes
-- ============================================================================

DROP INDEX idx_instances_started_at ON workflow_instances;
DROP INDEX idx_instances_owner ON workflow_instances;
DROP INDEX idx_instances_hash ON workflow_instances;

ALTER TABLE workflow_instances DROP COLUMN started_at;
ALTER TABLE workflow_instances DROP COLUMN owner_service;
ALTER TABLE workflow_instances DROP COLUMN source_hash;

-- ============================================================================
-- Drop new tables
-- ============================================================================

DROP TABLE IF EXISTS schema_version;
DROP TABLE IF EXISTS workflow_definitions;
