-- migrate:up
ALTER TABLE workflow_instances ADD COLUMN sandboxed INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE workflow_instances DROP COLUMN sandboxed;
