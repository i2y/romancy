---
title: "Transactional Outbox"
weight: 6
---

Romancy provides built-in support for the **Transactional Outbox Pattern**, ensuring that events are published reliably and atomically with workflow execution.

## What is the Transactional Outbox Pattern?

The Transactional Outbox Pattern solves the **dual-write problem** in distributed systems:

**Problem:**
```go
// ❌ This can fail!
db.SaveOrder(order)          // Step 1: Success
broker.PublishEvent(event)   // Step 2: Fails! ← Event lost
```

If the broker is down or the network fails, the event is lost even though the order was saved.

**Solution:**
```go
// ✅ Atomic operation
tx.Begin()
db.SaveOrder(order)                // Step 1: Save order
db.SaveToOutboxTable(event)        // Step 2: Save event
tx.Commit()
// Later: Background process publishes events from outbox
```

Both operations succeed or fail together. A background **Outbox Relayer** publishes events asynchronously.

---

## Quick Start

### 1. Send Events Transactionally

Use `SendEventTransactional()` inside activities:

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
)

var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID string, amount float64) (map[string]any, error) {
		// Business logic
		fmt.Printf("Creating order %s\n", orderID)

		// Get workflow context for transactional event sending
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Send event transactionally
		// This writes to the outbox table in the same transaction
		err := romancy.SendEventTransactional(wfCtx,
			"order.created",
			"order-service",
			map[string]any{
				"order_id": orderID,
				"amount":   amount,
				"status":   "pending",
			},
		)
		if err != nil {
			return nil, err
		}

		return map[string]any{"order_id": orderID, "status": "created"}, nil
	},
)
```

### 2. Enable Outbox Relayer

Configure the app with outbox enabled:

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy"
)

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("worker-1"),
		romancy.WithOutbox(true),                              // Enable outbox relayer
		romancy.WithBrokerURL("http://localhost:8080/events"), // CloudEvents endpoint
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Your workflow code here
}
```

That's it! Events are now published reliably.

---

## Custom Database Operations (Advanced)

For **true atomic transactions** between your business logic and event publishing, you can access Romancy's managed database session using `ctx.Session()`.

### Why Use ctx.Session()?

The basic approach (`SendEventTransactional()` inside activities) uses **Romancy's internal transaction** to write to the outbox table. This is sufficient for most use cases where your business logic is:

- Read-only operations
- External API calls
- Romancy's own workflow history

However, if you need to **write to your own database** and guarantee atomicity with event publishing, use `ctx.Session()`:

### Example: Payment Processing with Custom Database Operations

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
)

// Payment model (must be in the same database as Romancy)
type Payment struct {
	OrderID string
	Amount  float64
	Status  string
}

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string, amount float64) (map[string]any, error) {
		// Get workflow context
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session (same database as Romancy)
		session := wfCtx.Session()

		// Your business logic (same transaction as Romancy)
		payment := Payment{
			OrderID: orderID,
			Amount:  amount,
			Status:  "completed",
		}

		// Insert payment record
		_, err := session.Exec(
			"INSERT INTO payments (order_id, amount, status) VALUES (?, ?, ?)",
			payment.OrderID, payment.Amount, payment.Status,
		)
		if err != nil {
			return nil, err
		}

		// Romancy event (same transaction)
		err = romancy.SendEventTransactional(wfCtx,
			"payment.processed",
			"payment-service",
			map[string]any{"order_id": orderID, "amount": amount},
		)
		if err != nil {
			return nil, err
		}

		// Romancy automatically commits (or rolls back on exception):
		// 1. Your payment record (same database)
		// 2. Romancy's outbox event
		// 3. Romancy's workflow history
		return map[string]any{"order_id": orderID, "status": "processed"}, nil
	},
)
```

### Automatic Transaction Management

**Activities automatically manage transactions:**

```go
var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string, amount float64) (map[string]any, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session
		session := wfCtx.Session()

		// Your business logic
		_, err := session.Exec("INSERT INTO payments ...")
		if err != nil {
			return nil, err // Romancy automatically rolls back
		}

		// Events in same transaction
		err = romancy.SendEventTransactional(wfCtx, ...)
		if err != nil {
			return nil, err // Romancy automatically rolls back
		}

		// Romancy automatically commits on success
		return map[string]any{"order_id": orderID}, nil
	},
)
```

**How it works:**

When you access `ctx.Session()` inside an activity, Romancy automatically:

1. **Provides managed session**: Returns the current transaction's session
2. **Single transaction**: All operations use the same session
3. **Commits or rolls back**: Commits on success, rolls back on error

This results in:

1. **Single Transaction**: Your payment data + Romancy's outbox + history commit together
2. **Atomic Guarantee**: Either all succeed or all fail (no data inconsistency)
3. **Rollback Safety**: If any operation fails, everything rolls back

### Key Points

- ✅ **Atomic Guarantee**: Your data + outbox event + history commit together
- ✅ **Single Transaction**: All operations use Romancy's managed session
- ✅ **Automatic Management**: Romancy handles begin/commit/rollback
- ✅ **Rollback Safety**: If any operation fails, everything rolls back
- ⚠️ **Database Requirement**: See [Database Requirements](#database-requirements-for-custom-database-operations) below

### When to Use ctx.Session()

| Use Case | Recommended Approach |
|----------|---------------------|
| **Writing to your own database + events** | `ctx.Session()` - Ensures atomicity |
| **Read-only operations** | Standard activity - No custom DB access needed |
| **External API calls** | Standard activity - No database transaction needed |

---

## Advanced Transaction Control

Activities are automatically transactional (see [Automatic Transaction Management](#automatic-transaction-management) above). However, in advanced scenarios, you may need finer control over transaction boundaries using `ctx.Transaction()`.

### When to Use Advanced Transaction Control

- **Nested transactions (savepoints)** - Partial rollback within activities
- **Workflow-level transactions** - Manual control in workflow functions (not activities)

### Nested Transactions (Savepoints) in Activities

Within an activity, you can use `ctx.Transaction()` for savepoints:

```go
var processOrderWithOptionalPayment = romancy.DefineActivity("process_order_with_optional_payment",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)
		session := wfCtx.Session()

		// Outer transaction (automatic, managed by activity)
		_, err := session.Exec(
			"INSERT INTO orders (order_id, status) VALUES (?, ?)",
			orderID, "pending",
		)
		if err != nil {
			return nil, err
		}

		err = romancy.SendEventTransactional(wfCtx,
			"order.created",
			"order-service",
			map[string]any{"order_id": orderID},
		)
		if err != nil {
			return nil, err
		}

		// Nested transaction (savepoint)
		err = wfCtx.Transaction(func() error {
			// Risky payment operation
			err := romancy.SendEventTransactional(wfCtx,
				"payment.attempted",
				"payment-service",
				map[string]any{"order_id": orderID},
			)
			if err != nil {
				return err
			}

			_, err = session.Exec(
				"INSERT INTO payments (order_id, amount) VALUES (?, ?)",
				orderID, 100.0,
			)
			if err != nil {
				return err
			}

			// Simulate failure
			return fmt.Errorf("payment gateway unavailable")
		})

		if err != nil {
			// Inner transaction rolled back to savepoint
			// Order creation still succeeds, but payment failed
			err = romancy.SendEventTransactional(wfCtx,
				"payment.failed",
				"payment-service",
				map[string]any{"order_id": orderID, "reason": "gateway_unavailable"},
			)
			if err != nil {
				return nil, err
			}
		}

		// Outer transaction commits successfully
		return map[string]any{"order_id": orderID, "status": "created"}, nil
	},
)
```

**Key points:**

- Outer transaction is **automatically managed** by the activity
- Inner `ctx.Transaction()` creates a **savepoint**
- Inner transaction failure doesn't affect outer transaction
- Order and payment failure event are both committed

---

## How It Works

The Transactional Outbox Pattern has two phases:

### Phase 1: Write to Outbox (Synchronous)

When you call `SendEventTransactional()`:

```mermaid
sequenceDiagram
    participant W as Workflow
    participant A as Activity
    participant DB as Database

    W->>A: Execute activity
    A->>DB: BEGIN TRANSACTION
    A->>DB: Execute business logic
    A->>DB: INSERT INTO outbox_events
    A->>DB: INSERT INTO workflow_history
    A->>DB: COMMIT TRANSACTION
    A->>W: Return result
```

**Key Points:**

- Event is written to `outbox_events` table
- Same transaction as business logic
- If transaction fails, event is not stored
- If transaction succeeds, event will eventually be published

### Phase 2: Publish Events (Asynchronous)

The **Outbox Relayer** runs in the background:

```mermaid
sequenceDiagram
    participant R as Outbox Relayer
    participant DB as Database
    participant B as Message Broker

    loop Every 1 second (configurable)
        R->>DB: SELECT * FROM outbox_events WHERE status='pending'
        DB->>R: Return pending events

        loop For each event
            R->>B: POST CloudEvent
            alt Success
                B->>R: 200 OK
                R->>DB: UPDATE status='published'
            else Failure
                B->>R: 500 Error
                R->>DB: UPDATE retry_count++
            end
        end
    end
```

**Key Points:**

- Polls database every 1 second (default)
- Publishes events as CloudEvents (HTTP)
- Implements fixed-interval retry (retry on every poll until max_retries)
- At-least-once delivery guarantee

---

## Outbox Relayer Configuration

### Basic Configuration

```go
app := romancy.NewApp(
	romancy.WithDatabase("orders.db"),
	romancy.WithWorkerID("worker-1"),
	romancy.WithOutbox(true),
	romancy.WithBrokerURL("http://localhost:8080/events"),
)
```

### Advanced Configuration

```go
// Create custom relayer
relayer := romancy.NewOutboxRelayer(
	romancy.WithRelayerStorage(app.Storage()),
	romancy.WithRelayerBrokerURL("http://localhost:8080/events"),
	romancy.WithPollInterval(2*time.Second),  // Poll every 2 seconds
	romancy.WithMaxRetries(5),                 // Retry up to 5 times
	romancy.WithBatchSize(20),                 // Process 20 events per batch
	romancy.WithMaxAgeHours(24.0),             // Expire events older than 24 hours
)

ctx := context.Background()

// Start manually
if err := relayer.Start(ctx); err != nil {
	log.Fatal(err)
}

// Stop gracefully
relayer.Stop(ctx)
```

**Configuration Options:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `WithRelayerStorage` | `StorageProtocol` | Required | Storage backend for outbox events |
| `WithRelayerBrokerURL` | `string` | Required | Message Broker URL for publishing CloudEvents |
| `WithPollInterval` | `time.Duration` | `1s` | Polling interval |
| `WithMaxRetries` | `int` | `3` | Maximum retry attempts for temporary failures |
| `WithBatchSize` | `int` | `10` | Number of events to process per batch |
| `WithMaxAgeHours` | `float64` | `0` (disabled) | Maximum event age in hours before expiration |

**Max Age Timeout:**

Events older than `max_age_hours` are automatically marked as `'expired'` and will not be retried. This is useful for time-sensitive events that become meaningless after a certain period:

```go
// Example: Expire events older than 1 hour
relayer := romancy.NewOutboxRelayer(
	romancy.WithRelayerStorage(app.Storage()),
	romancy.WithRelayerBrokerURL("http://localhost:8080/events"),
	romancy.WithMaxAgeHours(1.0), // Events older than 1 hour → 'expired'
)
```

**Use Cases for Max Age Timeout:**

- **Real-time notifications**: Push notifications that are only relevant for a short time
- **Time-sensitive updates**: Stock price updates, sensor readings
- **Temporary offers**: Flash sale notifications that expire quickly
- **Session-based events**: Events tied to user sessions that timeout

### Retry Logic and Error Handling

The Outbox Relayer implements **intelligent retry with error classification**:

**How it works:**

1. **Polling**: Every `poll_interval` (default: 1s), the relayer checks for pending events
2. **Age Check** (Optional): If `max_age_hours` is set, events older than threshold → `'expired'` status
3. **Retry Check**: Before each send attempt, checks if `retry_count >= max_retries`
4. **Send Attempt**: If retry limit not exceeded, attempts to publish the event
5. **Success**: Event status → `'published'`
6. **Failure**: Error classification determines next action:
   - **4xx HTTP Error** (400, 401, 403, 404, 422, etc.) → `'invalid'` status (permanent failure, don't retry)
   - **5xx HTTP Error** (500, 502, 503, 504, etc.) → `'pending'` status, `retry_count` incremented (retry)
   - **Network Error** (connection timeout, DNS failure) → `'pending'` status, `retry_count` incremented (retry)
   - **Unknown Error** → `'pending'` status, `retry_count` incremented (retry, safety net)
7. **Max Retries Exceeded**: Event status → `'failed'` (permanently failed)

**Example with default settings** (`poll_interval=1s`, `max_retries=3`):

| Time | retry_count | Error Type | Action |
|------|-------------|-----------|--------|
| T+0s | 0 | First send attempt | |
| T+0s | 0 | → 400 Bad Request | **Stop**: Marked as `'invalid'` (client error) |
| T+0s | 0 | → 503 Service Unavailable | Continue: `retry_count` = 1 |
| T+1s | 1 | → Network timeout | Continue: `retry_count` = 2 |
| T+2s | 2 | → 500 Internal Server Error | Continue: `retry_count` = 3 |
| T+3s | 3 | N/A | **Stop**: `retry_count >= max_retries`, marked as `'failed'` |

**Important Notes:**

- **Fixed Interval**: Events are retried on every poll (no exponential backoff)
- **Retry Limit**: With `max_retries=3`, events are sent **3 times total** (at retry_count=0, 1, 2)
- **4xx Errors Don't Retry**: Client errors (malformed event, auth failure) are immediately marked as `'invalid'`
- **5xx Errors Retry**: Server errors (broker down, temporary failure) are retried up to max_retries
- **Network Errors Retry**: Connection timeouts, DNS failures are treated as temporary errors
- **Status Remains 'pending'**: During retries, event status stays `'pending'` (changes to `'published'` on success, `'failed'` after max retries, or `'invalid'` for 4xx errors)
- **Max Age Timeout**: Events older than `max_age_hours` are marked as `'expired'` and not retried

---

## Event Status Lifecycle

Events in the `outbox_events` table have five statuses:

```
pending → published  (Success)
        → failed     (After max_retries exceeded)
        → invalid    (4xx HTTP error, permanent failure)
        → expired    (Max age exceeded, too old to retry)
```

**Status Descriptions:**

| Status | Meaning | Retryable | How it happens |
|--------|---------|-----------|----------------|
| `pending` | Event waiting to be published | ✅ Yes | Initial status when event is created |
| `published` | Event successfully published | ❌ No | 2xx HTTP response from broker |
| `failed` | Event permanently failed after max retries | ❌ No | `retry_count >= max_retries` |
| `invalid` | Event is malformed or unauthorized | ❌ No | 4xx HTTP error (400, 401, 403, 404, 422) |
| `expired` | Event too old to be meaningful | ❌ No | `created_at` older than `max_age_hours` |

**Database Schema:**

```sql
-- SQLite / PostgreSQL / MySQL compatible schema
CREATE TABLE outbox_events (
    event_id VARCHAR(255) PRIMARY KEY,
    event_type VARCHAR(255) NOT NULL,
    event_source VARCHAR(255) NOT NULL,
    event_data TEXT NOT NULL,              -- JSON (unlimited size)
    content_type VARCHAR(100) NOT NULL DEFAULT 'application/json',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at TIMESTAMP,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    retry_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,

    -- Status constraint (enforced by database)
    CONSTRAINT valid_outbox_status CHECK (
        status IN ('pending', 'published', 'failed', 'invalid', 'expired')
    )
);

-- Index for efficient polling by Outbox Relayer
CREATE INDEX idx_outbox_status_created ON outbox_events(status, created_at);
```

**Query Outbox Events:**

```go
ctx := context.Background()

// Get pending events
events, err := app.Storage().GetPendingOutboxEvents(ctx, 10)
if err != nil {
	log.Fatal(err)
}

for _, event := range events {
	fmt.Printf("Event: %s, Status: %s\n", event.EventType, event.Status)
}
```

---

## Use Cases

### 1. Order Processing with Events

```go
var orderProcessingWorkflow = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, orderID string, items []OrderItem) (map[string]any, error) {
		// Step 1: Reserve inventory
		// → Sends "inventory.reserved" event
		inventory, err := reserveInventory.Execute(ctx, orderID, items)
		if err != nil {
			return nil, err
		}

		// Step 2: Charge payment
		// → Sends "payment.charged" event
		payment, err := chargePayment.Execute(ctx, orderID, inventory["total_amount"].(float64))
		if err != nil {
			return nil, err
		}

		// Step 3: Ship order
		// → Sends "order.shipped" event
		shipment, err := shipOrder.Execute(ctx, orderID)
		if err != nil {
			return nil, err
		}

		// Final event
		err = romancy.SendEventTransactional(ctx,
			"order.completed",
			"order-service",
			map[string]any{"order_id": orderID, "status": "completed"},
		)
		if err != nil {
			return nil, err
		}

		return map[string]any{"order_id": orderID, "status": "completed"}, nil
	},
)
```

### 2. Compensation with Events

Send events during compensation (rollback):

```go
var releaseInventory = romancy.DefineCompensation("release_inventory",
	func(ctx context.Context, reservationID string) error {
		fmt.Printf("Releasing inventory: %s\n", reservationID)

		wfCtx := romancy.GetWorkflowContext(ctx)

		// Send compensation event
		return romancy.SendEventTransactional(wfCtx,
			"inventory.released",
			"order-service",
			map[string]any{
				"reservation_id": reservationID,
				"reason":         "order_failed",
			},
		)
	},
)
```

### 3. Saga Pattern Integration

```go
var cancelReservation = romancy.DefineCompensation("cancel_hotel_reservation",
	func(ctx context.Context, reservationID string) error {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Send cancellation event
		return romancy.SendEventTransactional(wfCtx,
			"hotel.cancelled",
			"booking-service",
			map[string]any{"reservation_id": reservationID},
		)
	},
)

var reserveHotel = romancy.DefineActivity("reserve_hotel",
	func(ctx context.Context, bookingID string) (map[string]any, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Reserve hotel
		reservationID := fmt.Sprintf("HOTEL-%s", bookingID)

		// Send event transactionally
		err := romancy.SendEventTransactional(wfCtx,
			"hotel.reserved",
			"booking-service",
			map[string]any{"reservation_id": reservationID},
		)
		if err != nil {
			return nil, err
		}

		return map[string]any{"reservation_id": reservationID}, nil
	},
	romancy.WithCompensation(cancelReservation),
)
```

---

## Best Practices

### 1. Activity Transaction Control

Activities are **automatically transactional**. All operations within an activity (including `SendEventTransactional()`) are executed within a single transaction.

```go
// ✅ Default: Automatic transaction (recommended)
var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// All operations are automatically transactional:
		// - Workflow history recording
		// - Event publishing to outbox
		err := romancy.SendEventTransactional(wfCtx, "order.created", "order-service", ...)
		if err != nil {
			return nil, err
		}

		return map[string]any{"order_id": orderID}, nil
	},
)

// ✅ With session access for custom database operations
var createOrderWithDB = romancy.DefineActivity("create_order_with_db",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session
		session := wfCtx.Session()

		// Your database operations
		_, err := session.Exec("INSERT INTO orders (order_id) VALUES (?)", orderID)
		if err != nil {
			return nil, err
		}

		// Events in same transaction
		err = romancy.SendEventTransactional(wfCtx, "order.created", "order-service", ...)
		if err != nil {
			return nil, err
		}

		return map[string]any{"order_id": orderID}, nil
	},
)
```

**Default behavior:**

- ✅ Automatic transaction management
- ✅ Events are atomic with workflow history
- ✅ No manual begin/commit/rollback needed

### 2. Use CloudEvents Standard

Romancy publishes events as CloudEvents (HTTP), ensuring compatibility with:

- ✅ Knative Eventing
- ✅ CloudEvents SDK
- ✅ Any CloudEvents-compatible broker

### 3. Monitor Outbox Table

Monitor your outbox table regularly to detect issues:

```go
ctx := context.Background()

// Get event status summary using raw SQL
db := app.Storage().DB()

rows, err := db.QueryContext(ctx, `
	SELECT status, COUNT(*) as count
	FROM outbox_events
	GROUP BY status
`)
if err != nil {
	log.Fatal(err)
}
defer rows.Close()

statusCounts := make(map[string]int)
for rows.Next() {
	var status string
	var count int
	rows.Scan(&status, &count)
	statusCounts[status] = count
}

fmt.Println("📊 Outbox Status Summary:")
fmt.Printf("  - Pending:   %d\n", statusCounts["pending"])
fmt.Printf("  - Published: %d\n", statusCounts["published"])
fmt.Printf("  - Failed:    %d\n", statusCounts["failed"])
fmt.Printf("  - Invalid:   %d\n", statusCounts["invalid"])
fmt.Printf("  - Expired:   %d\n", statusCounts["expired"])

// Alert on invalid events
if statusCounts["invalid"] > 0 {
	fmt.Printf("⚠️ %d events marked as invalid (4xx errors)\n", statusCounts["invalid"])
	fmt.Println("   → Check event format, broker authentication, or permissions")
}

// Alert on expired events
if statusCounts["expired"] > 0 {
	fmt.Printf("⚠️ %d events expired due to age\n", statusCounts["expired"])
	fmt.Println("   → Check broker availability or consider increasing max_age_hours")
}

// Alert on permanently failed events
if statusCounts["failed"] > 0 {
	fmt.Printf("⚠️ %d events failed permanently\n", statusCounts["failed"])
	fmt.Println("   → Check broker health or network connectivity")
}
```

### 4. Handle Idempotency

Since the outbox pattern provides **at-least-once delivery**, consumers must handle duplicate events:

```go
// Consumer side (idempotency key)
func HandleEvent(w http.ResponseWriter, r *http.Request) {
	var event cloudevents.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	eventID := event.ID()

	// Check if already processed
	if isEventProcessed(eventID) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
		return
	}

	// Process event
	if err := processOrderCreated(event.Data()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Mark as processed
	markEventProcessed(eventID)

	w.WriteHeader(http.StatusOK)
}
```

---

## Complete Example: E-commerce Order Processing

This example demonstrates a real-world e-commerce order processing system using **custom database operations** with `ctx.Session()`.

### Scenario

1. Create order in your database tables (same database as Romancy)
2. Send "order.created" event to Romancy's outbox
3. Both operations are atomic (same transaction)

### Code

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/i2y/romancy"
)

// Order model (same database as Romancy)
type Order struct {
	OrderID       string
	CustomerEmail string
	Amount        float64
	Status        string
	CreatedAt     time.Time
}

// Activity with custom database operations
var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID, customerEmail string, amount float64) (map[string]any, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session (same database)
		session := wfCtx.Session()

		// 1. Save order to YOUR table (same database as Romancy)
		_, err := session.Exec(`
			INSERT INTO orders (order_id, customer_email, amount, status, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, orderID, customerEmail, amount, "pending", time.Now())
		if err != nil {
			return nil, err
		}

		// 2. Publish event to Romancy's outbox (SAME transaction)
		err = romancy.SendEventTransactional(wfCtx,
			"order.created",
			"order-service",
			map[string]any{
				"order_id":       orderID,
				"customer_email": customerEmail,
				"amount":         amount,
				"status":         "pending",
			},
		)
		if err != nil {
			return nil, err
		}

		// 3. Romancy automatically commits (or rolls back on error)
		// If event publishing fails, order creation rolls back
		// If order creation fails, event publishing rolls back

		return map[string]any{"order_id": orderID, "status": "created"}, nil
	},
)

// Workflow
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID, customerEmail string, amount float64) (map[string]any, error) {
		// Create order (atomic with event)
		result, err := createOrder.Execute(ctx, orderID, customerEmail, amount)
		if err != nil {
			return nil, err
		}

		return result, nil
	},
)

func main() {
	// Romancy app (all tables in same database)
	app := romancy.NewApp(
		romancy.WithDatabase("postgresql://user:password@localhost/ecommerce"),
		romancy.WithWorkerID("worker-1"),
		romancy.WithOutbox(true),
		romancy.WithBrokerURL("http://localhost:8080/events"),
	)

	ctx := context.Background()

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, orderWorkflow, OrderInput{
		OrderID: "ORD-12345",
		Email:   "customer@example.com",
		Amount:  149.99,
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Order created: %s\n", instanceID)

	// Event will be published by Outbox Relayer
	time.Sleep(2 * time.Second)
}
```

### Database Requirements for Custom Database Operations

When using `ctx.Session()` to access Romancy's managed session for custom database operations, your application tables must be in the **same database** as Romancy's tables:

**PostgreSQL**:

- **Requirement**: Your tables and Romancy's tables must be in the **same PostgreSQL database** (not just the same server/instance)
- **Reason**: PostgreSQL connections are bound to a single database
- **Example**: If Romancy uses `postgresql://user:pass@localhost/myapp`, your tables must also be in the `myapp` database

**MySQL**:

- **Requirement**: Your tables and Romancy's tables must be in the **same MySQL database**
- **Reason**: While MySQL technically allows cross-database transactions, we recommend using the same database for simplicity
- **Example**: If Romancy uses `mysql://user:pass@localhost/myapp`, your tables should also be in the `myapp` database

**SQLite**:

- **Requirement**: Your tables and Romancy's tables must be in the **same SQLite file**
- **Reason**: SQLite is file-based; cross-file transactions are not possible
- **Example**: If Romancy uses `workflow.db`, your tables must also be in `workflow.db`

**Best Practice**: Use the same database for both Romancy and your application tables. This simplifies deployment and ensures transactional consistency.

### Key Points

1. **Database Requirement**: See [Database Requirements](#database-requirements-for-custom-database-operations) above for PostgreSQL/MySQL/SQLite specifics
2. **Session Access**: `ctx.Session()` provides Romancy's managed session
3. **Automatic Management**: Romancy handles begin/commit/rollback automatically
4. **Atomic Guarantee**: Order creation + event publishing commit together
5. **Rollback Safety**: If any step fails, everything rolls back

### What Happens

```
1. Order saved to YOUR database (orders table)
2. Event saved to Romancy's outbox (outbox_events table)
3. Both committed atomically (COMMIT)
4. Outbox Relayer publishes event (async, background)
5. Event delivered to broker (at-least-once guarantee)
```

---

## What You Learned

- ✅ **Transactional Outbox Pattern**: Solves the dual-write problem
- ✅ **SendEventTransactional()**: Writes events to outbox table
- ✅ **ctx.Session()**: Access Romancy's managed session for custom database operations
- ✅ **Automatic Transaction Management**: Romancy handles begin/commit/rollback
- ✅ **Rollback Safety**: All operations roll back on failure
- ✅ **Outbox Relayer**: Publishes events asynchronously with retry
- ✅ **At-Least-Once Delivery**: Guaranteed event delivery
- ✅ **CloudEvents Support**: Standard event format

## See Also

- **[Saga Pattern](/docs/core-features/saga-compensation)**: Combine with compensation
- **[CloudEvents HTTP Binding](/docs/core-features/events/cloudevents-http-binding)**: CloudEvents specification compliance
- **[Lifecycle Hooks](/docs/core-features/hooks)**: Monitor outbox events with lifecycle hooks
