-- Add activity_id column to channel_subscriptions for replay matching
ALTER TABLE channel_subscriptions ADD COLUMN IF NOT EXISTS activity_id TEXT;
