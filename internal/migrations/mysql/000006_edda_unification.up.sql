-- Edda unification migration (MySQL)
-- Version: 000006
-- Description: Removes Romancy-specific columns, adopts Edda's implementation approach

-- ============================================================================
-- workflow_instances: Add framework column, drop error_message
-- ============================================================================

-- Add framework column for Go/Python workflow identification
ALTER TABLE workflow_instances ADD COLUMN framework VARCHAR(20) NOT NULL DEFAULT 'go';

-- Create index for framework filtering
CREATE INDEX idx_instances_framework ON workflow_instances(framework);

-- Drop error_message column (use history events instead)
ALTER TABLE workflow_instances DROP COLUMN error_message;

-- ============================================================================
-- channel_messages: Drop target_instance_id
-- ============================================================================

-- target_instance_id is replaced by dynamic channel names (e.g., "channel:instance_id")
ALTER TABLE channel_messages DROP COLUMN target_instance_id;

-- ============================================================================
-- channel_subscriptions: Drop waiting column
-- ============================================================================

-- waiting is replaced by "activity_id IS NOT NULL" check
-- Drop the waiting-related indexes first
DROP INDEX idx_channel_subs_waiting ON channel_subscriptions;

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

INSERT IGNORE INTO schema_version (version, description)
VALUES (6, 'Edda unification - removed Romancy-specific columns');
