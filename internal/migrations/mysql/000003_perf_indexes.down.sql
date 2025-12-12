-- Remove performance optimization indexes (MySQL 8.0+)

DROP INDEX idx_instances_status_locked ON workflow_instances;
DROP INDEX idx_channel_subs_channel_waiting ON channel_subscriptions;
DROP INDEX idx_channel_subs_waiting_timeout ON channel_subscriptions;
DROP INDEX idx_instances_lock_expires_status ON workflow_instances;
