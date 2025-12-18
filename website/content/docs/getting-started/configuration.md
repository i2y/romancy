---
title: "Configuration Reference"
weight: 5
---

This page provides a comprehensive reference for all configuration options available when creating a Romancy `App`.

## Basic Configuration

```go
app := romancy.NewApp(
    romancy.WithDatabase("postgres://user:password@localhost/workflows"),
    romancy.WithWorkerID("worker-1"),
    romancy.WithServiceName("my-service"),
)
```

## Database Configuration

### WithDatabase(url)

Sets the database connection URL.

```go
// SQLite (default)
romancy.WithDatabase("file:romancy.db")
romancy.WithDatabase(":memory:")  // in-memory for testing

// PostgreSQL
romancy.WithDatabase("postgres://user:password@localhost:5432/dbname")

// MySQL 8.0+
romancy.WithDatabase("mysql://user:password@localhost:3306/dbname")
```

**Default**: `"file:romancy.db"`

### WithAutoMigrate(enabled)

Enables or disables automatic database migration on startup.

```go
// Default: auto-migration enabled
app := romancy.NewApp(
    romancy.WithDatabase("postgres://user:password@localhost/db"),
)

// Disable auto-migration
app := romancy.NewApp(
    romancy.WithDatabase("postgres://user:password@localhost/db"),
    romancy.WithAutoMigrate(false),
)
```

**Default**: `true` (enabled)

When enabled, Romancy automatically applies pending [dbmate](https://github.com/amacneil/dbmate)-compatible migrations during `app.Start()`. Migration files are embedded in the binary, so no external files are needed at runtime.

See [Database Migrations](#database-migrations) section below for details.

### WithMigrationsFS(fs)

Sets a custom filesystem for database migrations.

```go
//go:embed custom_migrations
var customMigrations embed.FS

app := romancy.NewApp(
    romancy.WithDatabase("postgres://..."),
    romancy.WithMigrationsFS(customMigrations),
)
```

**Default**: Embedded migrations from `schema/db/migrations/`

## Worker Configuration

### WithWorkerID(id)

Sets a custom worker ID for identification.

```go
romancy.WithWorkerID("worker-1")
romancy.WithWorkerID(os.Getenv("HOSTNAME"))  // Use pod name in K8s
```

**Default**: Auto-generated UUID

### WithServiceName(name)

Sets the service name for identification.

```go
romancy.WithServiceName("order-service")
```

**Default**: `"romancy-service"`

## Outbox Configuration

For the transactional outbox pattern. See [Transactional Outbox](/docs/core-features/transactional-outbox) for details.

### WithOutbox(enabled)

Enables the transactional outbox pattern.

```go
romancy.WithOutbox(true)
```

**Default**: `false`

### WithBrokerURL(url)

Sets the CloudEvents broker URL for event publishing.

```go
romancy.WithBrokerURL("http://broker-ingress.knative-eventing.svc.cluster.local/default/default")
```

**Default**: `""` (empty)

### WithOutboxInterval(duration)

Sets the polling interval for the outbox relayer.

```go
romancy.WithOutboxInterval(500 * time.Millisecond)  // More frequent polling
```

**Default**: `1 * time.Second`

### WithOutboxBatchSize(size)

Sets the batch size for outbox processing.

```go
romancy.WithOutboxBatchSize(50)
```

**Default**: `100`

## PostgreSQL LISTEN/NOTIFY

For real-time event delivery with PostgreSQL. See [PostgreSQL LISTEN/NOTIFY](/docs/core-features/events/postgres-notify) for details.

### WithListenNotify(enabled)

Configures PostgreSQL LISTEN/NOTIFY usage.

```go
// Auto-detect (default) - enabled for PostgreSQL, disabled for others
romancy.WithListenNotify(nil)

// Force enable
enabled := true
romancy.WithListenNotify(&enabled)

// Force disable (polling only)
disabled := false
romancy.WithListenNotify(&disabled)
```

**Default**: `nil` (auto-detect)

### WithNotifyReconnectDelay(duration)

Sets the delay before reconnecting after a LISTEN/NOTIFY connection failure.

```go
romancy.WithNotifyReconnectDelay(30 * time.Second)
```

**Default**: `60 * time.Second`

## Performance Tuning

### Concurrency Limits

Control the number of concurrent goroutines for background tasks.

```go
romancy.WithMaxConcurrentResumptions(20)  // Max concurrent workflow resumptions
romancy.WithMaxConcurrentTimers(20)       // Max concurrent timer handlers
romancy.WithMaxConcurrentMessages(20)     // Max concurrent message handlers
```

| Option | Description | Default |
|--------|-------------|---------|
| `WithMaxConcurrentResumptions(n)` | Max concurrent workflow resumptions | `10` |
| `WithMaxConcurrentTimers(n)` | Max concurrent timer handlers | `10` |
| `WithMaxConcurrentMessages(n)` | Max concurrent message handlers | `10` |

### Batch Sizes

Control how many items are processed per polling cycle.

```go
romancy.WithMaxWorkflowsPerBatch(200)  // More workflows per cycle
romancy.WithMaxTimersPerBatch(200)     // More timers per cycle
romancy.WithMaxMessagesPerBatch(200)   // More messages per cycle
```

| Option | Description | Default |
|--------|-------------|---------|
| `WithMaxWorkflowsPerBatch(n)` | Workflows per polling cycle | `100` |
| `WithMaxTimersPerBatch(n)` | Timers per polling cycle | `100` |
| `WithMaxMessagesPerBatch(n)` | Channel messages per polling cycle | `100` |

## Background Task Intervals

Control how frequently background tasks run.

```go
app := romancy.NewApp(
    romancy.WithDatabase("postgres://..."),
    romancy.WithStaleLockInterval(30 * time.Second),
    romancy.WithTimerCheckInterval(5 * time.Second),
)
```

| Option | Description | Default |
|--------|-------------|---------|
| `WithStaleLockInterval(d)` | Stale lock cleanup frequency | `60s` |
| `WithStaleLockTimeout(d)` | Lock timeout threshold | `5m` (300s) |
| `WithTimerCheckInterval(d)` | Timer expiry check | `10s` |
| `WithEventTimeoutInterval(d)` | Event timeout check | `30s` |
| `WithMessageCheckInterval(d)` | Channel message check | `5s` |
| `WithRecurCheckInterval(d)` | Recurred workflow check | `5s` |
| `WithChannelCleanupInterval(d)` | Channel message cleanup | `5m` (300s) |
| `WithChannelMessageRetention(d)` | Message retention period | `24h` |
| `WithWorkflowResumptionInterval(d)` | Workflow resumption frequency | `1s` |

## Singleton Task Configuration

Control which background tasks run as singletons (only one worker executes at a time).

```go
romancy.WithSingletonStaleLockCleanup(true)   // Only one worker cleans locks
romancy.WithSingletonChannelCleanup(true)     // Only one worker cleans channels
```

| Option | Description | Default |
|--------|-------------|---------|
| `WithSingletonStaleLockCleanup(enabled)` | Stale lock cleanup as singleton | `true` |
| `WithSingletonChannelCleanup(enabled)` | Channel cleanup as singleton | `true` |

## Leader Election

Configure leader election for coordinating singleton background tasks across multiple workers. Only the leader executes singleton tasks like stale lock cleanup and channel cleanup.

### WithLeaderHeartbeatInterval(duration)

Sets the interval at which the leader renews its lease.

```go
romancy.WithLeaderHeartbeatInterval(10 * time.Second)
```

**Default**: `15 seconds`

### WithLeaderLeaseDuration(duration)

Sets how long a leader holds its lease before it expires.

```go
romancy.WithLeaderLeaseDuration(30 * time.Second)
```

**Default**: `45 seconds`

**Important**: Should be at least 3x the heartbeat interval to allow for missed heartbeats.

| Option | Description | Default |
|--------|-------------|---------|
| `WithLeaderHeartbeatInterval(d)` | Leader lease renewal interval | `15s` |
| `WithLeaderLeaseDuration(d)` | Leader lease expiration time | `45s` |

## Observability

### WithHooks(hooks)

Sets workflow lifecycle hooks for observability. See [Lifecycle Hooks](/docs/core-features/hooks) for details.

```go
import "github.com/i2y/romancy/hooks/otel"

app := romancy.NewApp(
    romancy.WithDatabase("postgres://..."),
    romancy.WithHooks(otel.NewOTelHooks(tracerProvider)),
)
```

**Default**: No-op hooks

## Lifecycle

### WithShutdownTimeout(duration)

Sets the timeout for graceful shutdown.

```go
romancy.WithShutdownTimeout(60 * time.Second)
```

**Default**: `30 * time.Second`

## Complete Example

```go
package main

import (
    "time"

    "github.com/i2y/romancy"
    "github.com/i2y/romancy/hooks/otel"
)

func main() {
    enabled := true

    app := romancy.NewApp(
        // Database
        romancy.WithDatabase("postgres://user:password@localhost:5432/workflows"),

        // Worker identity
        romancy.WithWorkerID("worker-1"),
        romancy.WithServiceName("order-service"),

        // PostgreSQL optimizations
        romancy.WithListenNotify(&enabled),
        romancy.WithNotifyReconnectDelay(30 * time.Second),

        // Performance tuning
        romancy.WithMaxConcurrentResumptions(20),
        romancy.WithMaxWorkflowsPerBatch(200),
        romancy.WithTimerCheckInterval(5 * time.Second),

        // Outbox (if needed)
        romancy.WithOutbox(true),
        romancy.WithBrokerURL("http://broker/events"),

        // Observability
        romancy.WithHooks(otel.NewOTelHooks(tracerProvider)),

        // Lifecycle
        romancy.WithShutdownTimeout(60 * time.Second),
    )

    // ...
}
```

## Database Migrations

Romancy **automatically applies database migrations** on startup. Migration files are embedded in the binary using Go's `embed` package, so no external files are needed at runtime.

### How It Works

1. **Automatic**: When `app.Start()` is called, Romancy checks for pending migrations
2. **dbmate-compatible**: Uses the same `schema_migrations` table and SQL format as [dbmate](https://github.com/amacneil/dbmate)
3. **Multi-worker safe**: Multiple pods/processes can start simultaneously without conflicts (uses UNIQUE constraint handling)
4. **Embedded**: Migration files are bundled into the binary

### Configuration

```go
// Default: auto-migration enabled
app := romancy.NewApp(
    romancy.WithDatabase("postgres://user:password@localhost/db"),
)

// Disable auto-migration (manage manually with dbmate CLI)
app := romancy.NewApp(
    romancy.WithDatabase("postgres://user:password@localhost/db"),
    romancy.WithAutoMigrate(false),
)
```

### Cross-Framework Compatibility

The database schema is managed in the [durax-io/schema](https://github.com/durax-io/schema) repository, which is shared between:
- **Romancy** (Go)
- **Edda** (Python)

This ensures that both frameworks can work with the same database.

### Manual Migration with dbmate (Optional)

If you prefer to manage migrations externally (e.g., in CI/CD pipelines), disable auto-migration and use the dbmate CLI:

```bash
# Install dbmate
brew install dbmate  # macOS
# or: go install github.com/amacneil/dbmate@latest

# Run migrations
dbmate --url "sqlite:workflow.db" --migrations-dir schema/db/migrations/sqlite up
dbmate --url "postgres://user:password@localhost/db?sslmode=disable" --migrations-dir schema/db/migrations/postgresql up
dbmate --url "mysql://user:password@localhost/db" --migrations-dir schema/db/migrations/mysql up
```

### Migration Commands (dbmate)

| Command | Description |
|---------|-------------|
| `dbmate up` | Run all pending migrations |
| `dbmate down` | Rollback the last migration |
| `dbmate status` | Show migration status |
| `dbmate create NAME` | Create a new migration file |

### Schema Submodule (For Romancy Contributors)

{{< callout type="info" >}}
**Regular users can skip this section.** Migration files are embedded in the romancy binary, so submodule setup is not required.
{{< /callout >}}

Only needed if you are contributing to romancy itself and need to modify the database schema:

```bash
git submodule add https://github.com/durax-io/schema.git schema
git submodule update --init --recursive
```
