-- v0.8.0 schema for Romancy (MySQL 8.0+)
-- Version: 000002
-- Description: Adds channels, recur, groups, and pagination support

-- Add continued_from column for recur pattern
ALTER TABLE workflow_instances ADD COLUMN continued_from VARCHAR(255);

-- Add index for pagination (started_at, instance_id composite cursor)
CREATE INDEX idx_instances_created_at ON workflow_instances(created_at DESC, instance_id);

-- ========================================
-- Channel Messages
-- ========================================
CREATE TABLE IF NOT EXISTS channel_messages (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    channel_name VARCHAR(255) NOT NULL,
    data_json JSON,
    data_binary LONGBLOB,
    metadata JSON,
    target_instance_id VARCHAR(255),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_channel_messages_channel (channel_name),
    INDEX idx_channel_messages_target (target_instance_id),
    INDEX idx_channel_messages_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- Channel Subscriptions
-- ========================================
CREATE TABLE IF NOT EXISTS channel_subscriptions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    channel_name VARCHAR(255) NOT NULL,
    mode VARCHAR(20) NOT NULL,
    waiting TINYINT(1) NOT NULL DEFAULT 0,
    timeout_at DATETIME(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_channel_subs_instance_channel (instance_id, channel_name),
    INDEX idx_channel_subs_channel (channel_name),
    INDEX idx_channel_subs_waiting (waiting),
    INDEX idx_channel_subs_timeout (timeout_at),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id),
    CONSTRAINT chk_channel_mode CHECK (mode IN ('broadcast', 'competing'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- Channel Delivery Cursors (for broadcast mode)
-- ========================================
CREATE TABLE IF NOT EXISTS channel_delivery_cursors (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    channel_name VARCHAR(255) NOT NULL,
    last_message_id BIGINT NOT NULL DEFAULT 0,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_delivery_cursor_instance_channel (instance_id, channel_name),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- Channel Message Claims (for competing mode)
-- ========================================
CREATE TABLE IF NOT EXISTS channel_message_claims (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    message_id BIGINT NOT NULL UNIQUE,
    instance_id VARCHAR(255) NOT NULL,
    claimed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    FOREIGN KEY (message_id) REFERENCES channel_messages(id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- Workflow History Archive (for recur pattern)
-- ========================================
CREATE TABLE IF NOT EXISTS workflow_history_archive (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    original_id BIGINT,
    instance_id VARCHAR(255) NOT NULL,
    activity_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(100) NOT NULL,
    event_data JSON,
    event_data_binary LONGBLOB,
    data_type VARCHAR(50) NOT NULL DEFAULT 'json',
    original_created_at DATETIME(6),
    archived_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_history_archive_instance (instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- Group Memberships (Erlang pg style)
-- ========================================
CREATE TABLE IF NOT EXISTS workflow_group_memberships (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    group_name VARCHAR(255) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_group_membership_instance_group (instance_id, group_name),
    INDEX idx_group_memberships_group (group_name),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- Message Subscriptions (channel message wait)
-- ========================================
CREATE TABLE IF NOT EXISTS workflow_message_subscriptions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    instance_id VARCHAR(255) NOT NULL,
    channel_name VARCHAR(255) NOT NULL,
    timeout_at DATETIME(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_message_subs_instance_channel (instance_id, channel_name),
    INDEX idx_message_subs_channel (channel_name),
    INDEX idx_message_subs_timeout (timeout_at),
    FOREIGN KEY (instance_id) REFERENCES workflow_instances(instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ========================================
-- System Locks (for background tasks)
-- ========================================
CREATE TABLE IF NOT EXISTS system_locks (
    lock_name VARCHAR(255) PRIMARY KEY,
    locked_by VARCHAR(255) NOT NULL,
    locked_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    expires_at DATETIME(6) NOT NULL,
    INDEX idx_system_locks_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
