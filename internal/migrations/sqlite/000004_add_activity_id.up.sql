-- Add activity_id column to channel_subscriptions for replay matching
ALTER TABLE channel_subscriptions ADD COLUMN activity_id TEXT;
