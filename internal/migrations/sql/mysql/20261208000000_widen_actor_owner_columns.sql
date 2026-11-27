-- migrate:up
-- Columns that hold an instance ID get the width of
-- workflow_instances.instance_id. At VARCHAR(64) a longer ID was refused
-- (strict mode) or truncated, and the owner and claimant checks then
-- refused the instance that owns the row.
ALTER TABLE actor_capabilities MODIFY COLUMN granted_by VARCHAR(255);
ALTER TABLE actor_fs MODIFY COLUMN owner VARCHAR(255);
ALTER TABLE actor_tasks MODIFY COLUMN claimed_by VARCHAR(255);
ALTER TABLE payload_blobs MODIFY COLUMN created_by VARCHAR(255);

-- migrate:down
ALTER TABLE payload_blobs MODIFY COLUMN created_by VARCHAR(64);
ALTER TABLE actor_tasks MODIFY COLUMN claimed_by VARCHAR(64);
ALTER TABLE actor_fs MODIFY COLUMN owner VARCHAR(64);
ALTER TABLE actor_capabilities MODIFY COLUMN granted_by VARCHAR(64);
