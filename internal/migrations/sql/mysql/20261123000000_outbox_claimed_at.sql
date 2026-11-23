-- migrate:up
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMP NULL;

-- migrate:down
ALTER TABLE outbox_events DROP COLUMN claimed_at;
