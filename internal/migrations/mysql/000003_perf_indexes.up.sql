-- Performance optimization: Add composite indexes for frequently used queries (MySQL 8.0+)

-- FindResumableWorkflows() optimization
-- Query: WHERE status = 'running' AND (locked_by IS NULL OR locked_by = '')
CREATE INDEX idx_instances_status_locked ON workflow_instances(status, locked_by);

-- GetChannelSubscribersWaiting() optimization
-- Query: WHERE channel_name = ? AND waiting = 1
CREATE INDEX idx_channel_subs_channel_waiting ON channel_subscriptions(channel_name, waiting);

-- Channel subscription timeout check optimization
-- Query: WHERE waiting = 1 AND timeout_at IS NOT NULL AND timeout_at < now()
CREATE INDEX idx_channel_subs_waiting_timeout ON channel_subscriptions(waiting, timeout_at);

-- Message subscription timeout check optimization
-- Query: WHERE timeout_at IS NOT NULL AND timeout_at < now()
-- Note: idx_message_subs_timeout already exists from migration 000002, skip if exists
-- CREATE INDEX idx_msg_subs_timeout ON workflow_message_subscriptions(timeout_at);

-- Timer expiration check optimization
-- Query: WHERE expires_at <= now()
-- Note: idx_timer_subs_expires already exists from migration 000001, skip if exists
-- CREATE INDEX idx_timer_subs_expires ON workflow_timer_subscriptions(expires_at);

-- CleanupStaleLocks() optimization
-- Query: WHERE locked_by IS NOT NULL AND lock_expires_at < now() AND status IN (...)
-- Note: MySQL doesn't support partial indexes (WHERE clause)
-- Using full composite index including locked_by for similar optimization
CREATE INDEX idx_instances_lock_expires_status ON workflow_instances(lock_expires_at, status, locked_by);
