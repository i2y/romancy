-- Remove performance optimization indexes

DROP INDEX IF EXISTS idx_instances_status_locked;
DROP INDEX IF EXISTS idx_channel_subs_channel_waiting;
DROP INDEX IF EXISTS idx_channel_subs_waiting_timeout;
DROP INDEX IF EXISTS idx_msg_subs_timeout;
DROP INDEX IF EXISTS idx_timer_subs_expires;
DROP INDEX IF EXISTS idx_instances_lock_expires_status;
