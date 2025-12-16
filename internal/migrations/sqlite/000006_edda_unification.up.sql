-- Edda unification migration (SQLite)
-- Version: 000006
-- Description: Removes Romancy-specific columns, adopts Edda's implementation approach

-- ============================================================================
-- workflow_instances: Add framework column, drop error_message
-- ============================================================================

-- Add framework column for Go/Python workflow identification
ALTER TABLE workflow_instances ADD COLUMN framework TEXT NOT NULL DEFAULT 'go';

-- Create index for framework filtering
CREATE INDEX IF NOT EXISTS idx_instances_framework ON workflow_instances(framework);

-- Drop error_message column (use history events instead)
-- SQLite 3.35.0+ supports DROP COLUMN
ALTER TABLE workflow_instances DROP COLUMN error_message;

-- ============================================================================
-- channel_messages: Drop target_instance_id
-- ============================================================================

-- target_instance_id is replaced by dynamic channel names (e.g., "channel:instance_id")
-- Drop index first (SQLite requires this before dropping the column)
DROP INDEX IF EXISTS idx_channel_messages_target;

ALTER TABLE channel_messages DROP COLUMN target_instance_id;

-- ============================================================================
-- channel_subscriptions: Drop waiting column
-- ============================================================================

-- waiting is replaced by "activity_id IS NOT NULL" check
-- Drop the waiting-related indexes first
DROP INDEX IF EXISTS idx_channel_subs_waiting;
DROP INDEX IF EXISTS idx_channel_subs_waiting_timeout;
DROP INDEX IF EXISTS idx_channel_subs_channel_waiting;

ALTER TABLE channel_subscriptions DROP COLUMN waiting;

-- ============================================================================
-- workflow_compensations: Drop comp_order and status columns
-- ============================================================================

-- comp_order is replaced by "ORDER BY created_at DESC"
-- status is replaced by history events (CompensationExecuted, CompensationFailed)
ALTER TABLE workflow_compensations DROP COLUMN comp_order;
ALTER TABLE workflow_compensations DROP COLUMN status;

-- ============================================================================
-- Record schema version
-- ============================================================================

INSERT OR IGNORE INTO schema_version (version, description)
VALUES (6, 'Edda unification - removed Romancy-specific columns');
