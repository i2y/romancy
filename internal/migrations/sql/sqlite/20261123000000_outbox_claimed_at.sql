-- migrate:up
ALTER TABLE outbox_events ADD COLUMN claimed_at TEXT;

-- migrate:down
ALTER TABLE outbox_events DROP COLUMN claimed_at;
