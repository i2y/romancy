-- v0.8.0 rollback schema for Romancy (PostgreSQL)
-- Version: 000002
-- Description: Removes channels, recur, groups, and pagination support

-- Drop indexes first
DROP INDEX IF EXISTS idx_system_locks_expires;
DROP INDEX IF EXISTS idx_message_subs_timeout;
DROP INDEX IF EXISTS idx_message_subs_channel;
DROP INDEX IF EXISTS idx_group_memberships_group;
DROP INDEX IF EXISTS idx_history_archive_instance;
DROP INDEX IF EXISTS idx_channel_subs_timeout;
DROP INDEX IF EXISTS idx_channel_subs_waiting;
DROP INDEX IF EXISTS idx_channel_subs_channel;
DROP INDEX IF EXISTS idx_channel_messages_created;
DROP INDEX IF EXISTS idx_channel_messages_target;
DROP INDEX IF EXISTS idx_channel_messages_channel;
DROP INDEX IF EXISTS idx_instances_created_at;

-- Drop tables in reverse dependency order
DROP TABLE IF EXISTS system_locks;
DROP TABLE IF EXISTS workflow_message_subscriptions;
DROP TABLE IF EXISTS workflow_group_memberships;
DROP TABLE IF EXISTS workflow_history_archive;
DROP TABLE IF EXISTS channel_message_claims;
DROP TABLE IF EXISTS channel_delivery_cursors;
DROP TABLE IF EXISTS channel_subscriptions;
DROP TABLE IF EXISTS channel_messages;

-- Remove continued_from column
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS continued_from;
