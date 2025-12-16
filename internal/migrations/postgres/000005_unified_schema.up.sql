-- Unified schema for Romancy-Edda compatibility (PostgreSQL)
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
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workflow_name, source_hash)
);

CREATE INDEX IF NOT EXISTS idx_definitions_name ON workflow_definitions(workflow_name);
CREATE INDEX IF NOT EXISTS idx_definitions_hash ON workflow_definitions(source_hash);

-- Schema Version (Edda compatibility)
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    description TEXT NOT NULL
);

-- ============================================================================
-- workflow_instances: Add new columns, rename created_at to started_at
-- ============================================================================

-- Add new columns
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS source_hash TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS owner_service TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS started_at TIMESTAMP WITH TIME ZONE;

-- Migrate data: created_at -> started_at
UPDATE workflow_instances SET started_at = created_at WHERE started_at IS NULL;

-- Make started_at NOT NULL with default
ALTER TABLE workflow_instances ALTER COLUMN started_at SET NOT NULL;
ALTER TABLE workflow_instances ALTER COLUMN started_at SET DEFAULT NOW();

-- Add indexes for new columns
CREATE INDEX IF NOT EXISTS idx_instances_hash ON workflow_instances(source_hash);
CREATE INDEX IF NOT EXISTS idx_instances_owner ON workflow_instances(owner_service);
CREATE INDEX IF NOT EXISTS idx_instances_started_at ON workflow_instances(started_at DESC, instance_id);

-- ============================================================================
-- workflow_timer_subscriptions: Add activity_id
-- ============================================================================

ALTER TABLE workflow_timer_subscriptions ADD COLUMN IF NOT EXISTS activity_id TEXT;

-- ============================================================================
-- workflow_group_memberships: Rename created_at to joined_at
-- ============================================================================

ALTER TABLE workflow_group_memberships ADD COLUMN IF NOT EXISTS joined_at TIMESTAMP WITH TIME ZONE;
UPDATE workflow_group_memberships SET joined_at = created_at WHERE joined_at IS NULL;
ALTER TABLE workflow_group_memberships ALTER COLUMN joined_at SET NOT NULL;
ALTER TABLE workflow_group_memberships ALTER COLUMN joined_at SET DEFAULT NOW();

-- ============================================================================
-- workflow_compensations: Rename columns
-- ============================================================================

-- Rename compensation_fn to activity_name
ALTER TABLE workflow_compensations RENAME COLUMN compensation_fn TO activity_name;

-- Rename compensation_arg to args
ALTER TABLE workflow_compensations RENAME COLUMN compensation_arg TO args;

-- ============================================================================
-- channel_messages: Rename columns and add new ones
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_messages RENAME COLUMN channel_name TO channel;

-- Rename data_json to data (TEXT for Edda compatibility)
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS data TEXT;
UPDATE channel_messages SET data = data_json::TEXT WHERE data IS NULL AND data_json IS NOT NULL;

-- Add message_id (UUID for Edda compatibility)
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS message_id TEXT UNIQUE;
UPDATE channel_messages SET message_id = gen_random_uuid()::TEXT WHERE message_id IS NULL;

-- Add data_type
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS data_type TEXT NOT NULL DEFAULT 'json';

-- Rename created_at to published_at
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS published_at TIMESTAMP WITH TIME ZONE;
UPDATE channel_messages SET published_at = created_at WHERE published_at IS NULL;
ALTER TABLE channel_messages ALTER COLUMN published_at SET NOT NULL;
ALTER TABLE channel_messages ALTER COLUMN published_at SET DEFAULT NOW();

-- Update indexes
DROP INDEX IF EXISTS idx_channel_messages_channel;
CREATE INDEX IF NOT EXISTS idx_channel_messages_channel ON channel_messages(channel, published_at);

-- ============================================================================
-- channel_subscriptions: Rename columns and add new ones
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_subscriptions RENAME COLUMN channel_name TO channel;

-- Add cursor_message_id
ALTER TABLE channel_subscriptions ADD COLUMN IF NOT EXISTS cursor_message_id INTEGER;

-- Rename created_at to subscribed_at
ALTER TABLE channel_subscriptions ADD COLUMN IF NOT EXISTS subscribed_at TIMESTAMP WITH TIME ZONE;
UPDATE channel_subscriptions SET subscribed_at = created_at WHERE subscribed_at IS NULL;
ALTER TABLE channel_subscriptions ALTER COLUMN subscribed_at SET NOT NULL;
ALTER TABLE channel_subscriptions ALTER COLUMN subscribed_at SET DEFAULT NOW();

-- Update indexes
DROP INDEX IF EXISTS idx_channel_subs_channel;
CREATE INDEX IF NOT EXISTS idx_channel_subs_channel ON channel_subscriptions(channel);
DROP INDEX IF EXISTS idx_channel_subs_waiting;
CREATE INDEX IF NOT EXISTS idx_channel_subs_waiting ON channel_subscriptions(channel, waiting);
CREATE INDEX IF NOT EXISTS idx_channel_subs_channel_activity ON channel_subscriptions(channel, activity_id);

-- ============================================================================
-- channel_delivery_cursors: Rename columns
-- ============================================================================

-- Rename channel_name to channel
ALTER TABLE channel_delivery_cursors RENAME COLUMN channel_name TO channel;

-- Rename last_message_id to last_delivered_id
ALTER TABLE channel_delivery_cursors RENAME COLUMN last_message_id TO last_delivered_id;

-- ============================================================================
-- channel_message_claims: Update message_id type
-- ============================================================================

-- Add text-based message_id column for Edda compatibility
ALTER TABLE channel_message_claims ADD COLUMN IF NOT EXISTS message_id_text TEXT;

-- Migrate existing integer references to text
UPDATE channel_message_claims cmc
SET message_id_text = cm.message_id
FROM channel_messages cm
WHERE cmc.message_id = cm.id AND cmc.message_id_text IS NULL;

-- ============================================================================
-- workflow_outbox: Rename columns and add new ones
-- ============================================================================

-- Rename attempts to retry_count
ALTER TABLE workflow_outbox RENAME COLUMN attempts TO retry_count;

-- Add new columns
ALTER TABLE workflow_outbox ADD COLUMN IF NOT EXISTS event_data_binary BYTEA;
ALTER TABLE workflow_outbox ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE workflow_outbox ADD COLUMN IF NOT EXISTS published_at TIMESTAMP WITH TIME ZONE;

-- Add indexes
CREATE INDEX IF NOT EXISTS idx_outbox_retry ON workflow_outbox(status, retry_count);
CREATE INDEX IF NOT EXISTS idx_outbox_published ON workflow_outbox(published_at);

-- ============================================================================
-- Record schema version
-- ============================================================================

INSERT INTO schema_version (version, description)
VALUES (5, 'Unified schema with Edda for cross-framework compatibility')
ON CONFLICT (version) DO NOTHING;
