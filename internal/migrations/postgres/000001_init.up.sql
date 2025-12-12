-- Initial schema for Romancy (PostgreSQL)
-- Version: 000001
-- Description: Creates all core tables for workflow execution

CREATE TABLE IF NOT EXISTS workflow_instances (
    instance_id TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    input_data TEXT,
    output_data TEXT,
    error_message TEXT,
    current_activity_id TEXT,
    source_code TEXT,
    locked_by TEXT,
    locked_at TIMESTAMP WITH TIME ZONE,
    lock_timeout_seconds INTEGER,
    lock_expires_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_instances_status ON workflow_instances(status);
CREATE INDEX IF NOT EXISTS idx_instances_workflow_name ON workflow_instances(workflow_name);
CREATE INDEX IF NOT EXISTS idx_instances_lock_expires ON workflow_instances(lock_expires_at);

CREATE TABLE IF NOT EXISTS workflow_history (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES workflow_instances(instance_id),
    activity_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_data TEXT,
    event_data_binary BYTEA,
    data_type TEXT NOT NULL DEFAULT 'json',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE(instance_id, activity_id)
);

CREATE INDEX IF NOT EXISTS idx_history_instance ON workflow_history(instance_id);

CREATE TABLE IF NOT EXISTS workflow_event_subscriptions (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES workflow_instances(instance_id),
    event_type TEXT NOT NULL,
    timeout_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE(instance_id, event_type)
);

CREATE INDEX IF NOT EXISTS idx_event_subs_type ON workflow_event_subscriptions(event_type);
CREATE INDEX IF NOT EXISTS idx_event_subs_timeout ON workflow_event_subscriptions(timeout_at);

CREATE TABLE IF NOT EXISTS workflow_timer_subscriptions (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES workflow_instances(instance_id),
    timer_id TEXT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    step INTEGER NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE(instance_id, timer_id)
);

CREATE INDEX IF NOT EXISTS idx_timer_subs_expires ON workflow_timer_subscriptions(expires_at);

CREATE TABLE IF NOT EXISTS workflow_outbox (
    id SERIAL PRIMARY KEY,
    event_id TEXT UNIQUE NOT NULL,
    event_type TEXT NOT NULL,
    event_source TEXT NOT NULL,
    event_data TEXT,
    data_type TEXT NOT NULL DEFAULT 'json',
    content_type TEXT DEFAULT 'application/json',
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_outbox_status ON workflow_outbox(status);

CREATE TABLE IF NOT EXISTS workflow_compensations (
    id SERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES workflow_instances(instance_id),
    activity_id TEXT NOT NULL,
    compensation_fn TEXT NOT NULL,
    compensation_arg TEXT,
    comp_order INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_compensations_instance ON workflow_compensations(instance_id);
