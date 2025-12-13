---
title: PostgreSQL LISTEN/NOTIFY
weight: 30
description: Real-time notifications for workflow events using PostgreSQL LISTEN/NOTIFY
---

# PostgreSQL LISTEN/NOTIFY

When using PostgreSQL as the storage backend, Romancy can leverage PostgreSQL's LISTEN/NOTIFY mechanism for real-time event notifications. This enables near-instant workflow resumption instead of relying solely on polling.

## Overview

PostgreSQL LISTEN/NOTIFY provides:

- **Near-instant notifications** - Workflows resume immediately when events occur
- **Reduced database polling** - Less load on the database during idle periods
- **Automatic fallback** - Polling continues as a safety net if notifications fail
- **Zero additional infrastructure** - Uses existing PostgreSQL connection

## Enabling LISTEN/NOTIFY

LISTEN/NOTIFY is automatically enabled when using PostgreSQL. You can explicitly configure it:

```go
app := romancy.NewApp(
    romancy.WithDatabase("postgres://user:password@localhost:5432/dbname"),
    // Explicitly enable/disable (nil = auto-detect, enabled for PostgreSQL)
    romancy.WithListenNotify(romancy.BoolPtr(true)),
    // Fallback polling interval when LISTEN/NOTIFY is unavailable
    romancy.WithNotifyFallbackInterval(30 * time.Second),
    // Delay before reconnecting after connection failure
    romancy.WithNotifyReconnectDelay(60 * time.Second),
)
```

### Configuration Options

| Option | Default | Description |
|--------|---------|-------------|
| `WithListenNotify` | `nil` (auto) | Enable/disable LISTEN/NOTIFY. `nil` = auto-detect based on database type |
| `WithNotifyFallbackInterval` | 30s | Fallback polling interval when notifications unavailable |
| `WithNotifyReconnectDelay` | 60s | Delay before attempting reconnection after failure |

## How It Works

### Notification Channels

Romancy uses four PostgreSQL notification channels:

| Channel | Event | Purpose |
|---------|-------|---------|
| `romancy_workflow_resumable` | Workflow ready to resume | Message delivered, lock released |
| `romancy_timer_expired` | Timer registered | New timer subscription created |
| `romancy_channel_message` | Message published | New message on a channel |
| `romancy_outbox_pending` | Outbox event added | Event ready to be relayed |

### Notification Flow

1. **Event occurs** - A storage operation triggers a state change
2. **pg_notify called** - Storage layer sends notification with JSON payload
3. **Listener receives** - Background listener processes the notification
4. **Immediate action** - Handler triggers workflow resumption or message delivery

```
Storage Operation     PostgreSQL        Romancy Worker
      │                   │                  │
      │ INSERT/UPDATE     │                  │
      ├──────────────────>│                  │
      │ pg_notify()       │                  │
      ├──────────────────>│                  │
      │                   │ NOTIFY           │
      │                   ├─────────────────>│
      │                   │                  │ Resume workflow
      │                   │                  │
```

## Notification Payloads

### Workflow Resumable

```json
{
  "instance_id": "wf_abc123",
  "workflow_name": "order_processing"
}
```

### Timer Registered

```json
{
  "instance_id": "wf_abc123",
  "timer_id": "timer:sleep_1",
  "expires_at": "2025-01-15T10:30:00Z"
}
```

### Channel Message

```json
{
  "channel_name": "orders",
  "message_id": 42,
  "target_instance_id": "wf_abc123"
}
```

### Outbox Pending

```json
{
  "event_id": "evt_123"
}
```

## Fallback Behavior

LISTEN/NOTIFY is an optimization, not a requirement. Romancy maintains polling-based background tasks that serve as fallbacks:

| Task | Default Interval | Purpose |
|------|------------------|---------|
| Workflow resumption | 1s | Find resumable workflows |
| Timer check | 1s | Process expired timers |
| Channel timeout | 5s | Handle timed-out channel waits |

When LISTEN/NOTIFY is active, these tasks primarily catch edge cases:
- Notifications lost during connection issues
- Events that occurred while the listener was reconnecting
- Cross-worker load balancing

## Batch Size Configuration

Control how many items are processed per batch:

```go
app := romancy.NewApp(
    romancy.WithDatabase("postgres://..."),
    romancy.WithMaxWorkflowsPerBatch(100),  // Workflow resumption batch
    romancy.WithMaxTimersPerBatch(100),     // Timer processing batch
    romancy.WithMaxMessagesPerBatch(100),   // Channel message batch
)
```

## Connection Management

The LISTEN/NOTIFY listener maintains a dedicated connection:

- **Automatic reconnection** - Reconnects after connection failures
- **Exponential backoff** - Configurable delay between reconnection attempts
- **Graceful shutdown** - Properly closes connection on app shutdown
- **Connection pooling** - Uses separate connection from the main pool

## Monitoring

Check listener status programmatically (if needed for custom monitoring):

```go
// The listener status is logged at INFO level
// Example log output:
// INFO PostgreSQL LISTEN/NOTIFY configured fallback_interval=30s reconnect_delay=60s
// INFO PostgreSQL LISTEN/NOTIFY connection established
// WARN LISTEN/NOTIFY connection lost, reconnecting error=... reconnect_delay=60s
```

## Disabling LISTEN/NOTIFY

In some environments, you may want to disable LISTEN/NOTIFY:

```go
falseVal := false
app := romancy.NewApp(
    romancy.WithDatabase("postgres://..."),
    romancy.WithListenNotify(&falseVal),  // Explicitly disable
)
```

Common reasons to disable:
- Using a PostgreSQL-compatible proxy that doesn't support LISTEN/NOTIFY
- Testing/debugging with polling-only behavior
- Specific infrastructure requirements

## Comparison with Polling-Only

| Aspect | LISTEN/NOTIFY | Polling Only |
|--------|--------------|--------------|
| Latency | Near-instant | Up to poll interval |
| DB load (idle) | Minimal | Constant queries |
| Reliability | Eventual (with fallback) | Guaranteed |
| Complexity | Slightly higher | Simpler |

## Best Practices

1. **Keep fallback intervals reasonable** - Not too long (delays) or too short (defeats purpose)
2. **Monitor connection health** - Watch logs for reconnection events
3. **Use appropriate batch sizes** - Balance throughput vs. memory
4. **Test failure scenarios** - Ensure your system handles notification failures gracefully
