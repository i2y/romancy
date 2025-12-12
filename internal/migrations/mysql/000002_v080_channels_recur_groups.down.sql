-- v0.8.0 rollback schema for Romancy (MySQL 8.0+)
-- Version: 000002
-- Description: Removes channels, recur, groups, and pagination support

-- Drop tables in reverse dependency order
DROP TABLE IF EXISTS system_locks;
DROP TABLE IF EXISTS workflow_message_subscriptions;
DROP TABLE IF EXISTS workflow_group_memberships;
DROP TABLE IF EXISTS workflow_history_archive;
DROP TABLE IF EXISTS channel_message_claims;
DROP TABLE IF EXISTS channel_delivery_cursors;
DROP TABLE IF EXISTS channel_subscriptions;
DROP TABLE IF EXISTS channel_messages;

-- Drop index
DROP INDEX idx_instances_created_at ON workflow_instances;

-- Drop column
ALTER TABLE workflow_instances DROP COLUMN continued_from;
