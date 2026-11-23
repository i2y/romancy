-- migrate:up
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMP;

-- migrate:down
ALTER TABLE outbox_events DROP COLUMN IF EXISTS claimed_at;
