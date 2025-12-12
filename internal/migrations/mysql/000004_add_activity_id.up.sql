-- Add activity_id column to channel_subscriptions for replay matching
-- MySQL requires conditional check via procedure or ignore errors
ALTER TABLE channel_subscriptions ADD COLUMN activity_id VARCHAR(255);
