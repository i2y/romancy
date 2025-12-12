-- Remove activity_id column from channel_subscriptions
ALTER TABLE channel_subscriptions DROP COLUMN IF EXISTS activity_id;
