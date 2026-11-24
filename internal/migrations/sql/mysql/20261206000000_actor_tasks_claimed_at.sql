-- migrate:up
ALTER TABLE actor_tasks ADD COLUMN claimed_at TIMESTAMP NULL;
ALTER TABLE actor_tasks ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE actor_tasks DROP COLUMN claimed_at;
ALTER TABLE actor_tasks DROP COLUMN attempts;
