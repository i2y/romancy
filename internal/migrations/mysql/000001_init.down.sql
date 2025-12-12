-- Rollback: Drop all tables in reverse order
DROP TABLE IF EXISTS workflow_compensations;
DROP TABLE IF EXISTS workflow_outbox;
DROP TABLE IF EXISTS workflow_timer_subscriptions;
DROP TABLE IF EXISTS workflow_event_subscriptions;
DROP TABLE IF EXISTS workflow_history;
DROP TABLE IF EXISTS workflow_instances;
