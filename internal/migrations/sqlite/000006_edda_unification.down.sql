-- Revert Edda unification migration (SQLite)
-- Version: 000006

-- Remove schema version record
DELETE FROM schema_version WHERE version = 6;

-- ============================================================================
-- workflow_compensations: Restore comp_order and status columns
-- ============================================================================

ALTER TABLE workflow_compensations ADD COLUMN comp_order INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_compensations ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';

-- ============================================================================
-- channel_subscriptions: Restore waiting column and indexes
-- ============================================================================

ALTER TABLE channel_subscriptions ADD COLUMN waiting INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_channel_subs_waiting ON channel_subscriptions(channel, waiting);

-- ============================================================================
-- channel_messages: Restore target_instance_id column
-- ============================================================================

ALTER TABLE channel_messages ADD COLUMN target_instance_id TEXT;

-- ============================================================================
-- workflow_instances: Remove framework, restore error_message
-- ============================================================================

DROP INDEX IF EXISTS idx_instances_framework;
ALTER TABLE workflow_instances DROP COLUMN framework;
ALTER TABLE workflow_instances ADD COLUMN error_message TEXT;
