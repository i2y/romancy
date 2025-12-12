-- Initial schema for Romancy (MySQL 8.0+)
-- Version: 000001
-- Description: Creates all core tables for workflow execution

CREATE TABLE IF NOT EXISTS workflow_instances (
    instance_id VARCHAR(255) PRIMARY KEY,
    workflow_name VARCHAR(255) NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    input_data LONGTEXT,
    output_data LONGTEXT,
    error_message TEXT,
    current_activity_id VARCHAR(255),
    source_code LONGTEXT,
    locked_by VARCHAR(255),
    locked_at DATETIME(6),
    lock_timeout_seconds INT,
    lock_expires_at DATETIME(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    INDEX idx_instances_status (status),
    INDEX idx_instances_workflow_name (workflow_name),
    INDEX idx_instances_lock_expires (lock_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS workflow_history (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    activity_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(100) NOT NULL,
    event_data LONGTEXT,
    event_data_binary LONGBLOB,
    data_type VARCHAR(50) NOT NULL DEFAULT 'json',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_history_instance_activity (instance_id, activity_id),
    INDEX idx_history_instance (instance_id),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS workflow_event_subscriptions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(255) NOT NULL,
    timeout_at DATETIME(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_event_subs_instance_type (instance_id, event_type),
    INDEX idx_event_subs_type (event_type),
    INDEX idx_event_subs_timeout (timeout_at),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS workflow_timer_subscriptions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    timer_id VARCHAR(255) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    step INT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_timer_subs_instance_timer (instance_id, timer_id),
    INDEX idx_timer_subs_expires (expires_at),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS workflow_outbox (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_id VARCHAR(255) NOT NULL UNIQUE,
    event_type VARCHAR(255) NOT NULL,
    event_source VARCHAR(255) NOT NULL,
    event_data LONGTEXT,
    data_type VARCHAR(50) NOT NULL DEFAULT 'json',
    content_type VARCHAR(100) DEFAULT 'application/json',
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    attempts INT NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    INDEX idx_outbox_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS workflow_compensations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    activity_id VARCHAR(255) NOT NULL,
    compensation_fn VARCHAR(255) NOT NULL,
    compensation_arg LONGTEXT,
    comp_order INT NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_compensations_instance (instance_id),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
