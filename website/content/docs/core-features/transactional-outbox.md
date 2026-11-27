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

type CreateOrderResult struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

type CreateOrderInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
		// Business logic
		fmt.Printf("Creating order %s\n", input.OrderID)

		// Get workflow context for transactional event sending
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Send event transactionally
		// This writes to the outbox table in the same transaction
		err := romancy.SendEventTransactional(wfCtx,
			"order.created",
			"order-service",
			map[string]any{
				"order_id": input.OrderID,
				"amount":   input.Amount,
				"status":   "pending",
			},
		)
		if err != nil {
			return CreateOrderResult{}, err
		}

		return CreateOrderResult{OrderID: input.OrderID, Status: "created"}, nil
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

	// Register your workflows before starting the app:
	// romancy.RegisterWorkflow(app, orderWorkflow)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start workflows here
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

	"github.com/i2y/romancy"
)

// Payment model (must be in the same database as Romancy)
type Payment struct {
	OrderID string
	Amount  float64
	Status  string
}

type ProcessPaymentResult struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

type ProcessPaymentInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, input ProcessPaymentInput) (ProcessPaymentResult, error) {
		// Get workflow context
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session (same database as Romancy)
		session := wfCtx.Session()

		// Your business logic (same transaction as Romancy)
		payment := Payment{
			OrderID: input.OrderID,
			Amount:  input.Amount,
			Status:  "completed",
		}

		// Insert payment record
		_, err := session.ExecContext(ctx,
			"INSERT INTO payments (order_id, amount, status) VALUES (?, ?, ?)",
			payment.OrderID, payment.Amount, payment.Status,
		)
		if err != nil {
			return ProcessPaymentResult{}, err
		}

		// Romancy event (same transaction)
		err = romancy.SendEventTransactional(wfCtx,
			"payment.processed",
			"payment-service",
			map[string]any{"order_id": input.OrderID, "amount": input.Amount},
		)
		if err != nil {
			return ProcessPaymentResult{}, err
		}

		// Romancy automatically commits (or rolls back on exception):
		// 1. Your payment record (same database)
		// 2. Romancy's outbox event
		// 3. Romancy's workflow history
		return ProcessPaymentResult{OrderID: input.OrderID, Status: "processed"}, nil
	},
)
```

### Automatic Transaction Management

**Activities automatically manage transactions:**

```go
type PaymentResult struct {
	OrderID string `json:"order_id"`
}

type PaymentInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, input PaymentInput) (PaymentResult, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session
		session := wfCtx.Session()

		// Your business logic
		_, err := session.ExecContext(ctx, "INSERT INTO payments (order_id, amount) VALUES (?, ?)", input.OrderID, input.Amount)
		if err != nil {
			return PaymentResult{}, err // Romancy automatically rolls back
		}

		// Events in same transaction
		err = romancy.SendEventTransactional(wfCtx,
			"payment.processed",
			"payment-service",
			map[string]any{"order_id": input.OrderID},
		)
		if err != nil {
			return PaymentResult{}, err // Romancy automatically rolls back
		}

		// Romancy automatically commits on success
		return PaymentResult{OrderID: input.OrderID}, nil
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

## Transaction Boundaries

Each attempt of an activity runs in one transaction: its writes through `ctx.Session()`, the events it sends with `SendEventTransactional` and its history record commit together, or not at all. A failed attempt is rolled back before the activity is retried, so it leaves no rows and no events behind.

There are no nested transactions or savepoints inside an activity. To commit one step whatever happens to the next, put the steps in separate activities and let the workflow decide what a failure means:

```go
type OrderStepResult struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

// Commits the order and its event, whatever happens to the payment
var createOrderRecord = romancy.DefineActivity("create_order_record",
	func(ctx context.Context, orderID string) (OrderStepResult, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)
		_, err := wfCtx.Session().ExecContext(ctx,
			"INSERT INTO orders (order_id, status) VALUES (?, ?)",
			orderID, "pending",
		)
		if err != nil {
			return OrderStepResult{}, err
		}
		err = romancy.SendEventTransactional(wfCtx, "order.created", "order-service",
			map[string]any{"order_id": orderID},
		)
		if err != nil {
			return OrderStepResult{}, err
		}
		return OrderStepResult{OrderID: orderID, Status: "created"}, nil
	},
)

// Its failure rolls back its own payment row and event, nothing else
var chargeOrder = romancy.DefineActivity("charge_order",
	func(ctx context.Context, orderID string) (OrderStepResult, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)
		_, err := wfCtx.Session().ExecContext(ctx,
			"INSERT INTO payments (order_id, amount) VALUES (?, ?)",
			orderID, 100.0,
		)
		if err != nil {
			return OrderStepResult{}, err
		}
		err = romancy.SendEventTransactional(wfCtx, "payment.charged", "payment-service",
			map[string]any{"order_id": orderID},
		)
		if err != nil {
			return OrderStepResult{}, err
		}

		// Simulate failure
		return OrderStepResult{}, fmt.Errorf("payment gateway unavailable")
	},
)

var recordPaymentFailure = romancy.DefineActivity("record_payment_failure",
	func(ctx context.Context, orderID string) (OrderStepResult, error) {
		err := romancy.SendEventTransactional(romancy.GetWorkflowContext(ctx),
			"payment.failed",
			"payment-service",
			map[string]any{"order_id": orderID, "reason": "gateway_unavailable"},
		)
		if err != nil {
			return OrderStepResult{}, err
		}
		return OrderStepResult{OrderID: orderID, Status: "payment_failed"}, nil
	},
)

var orderWithOptionalPayment = romancy.DefineWorkflow("order_with_optional_payment",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderStepResult, error) {
		if _, err := createOrderRecord.Execute(ctx, orderID); err != nil {
			return OrderStepResult{}, err
		}
		if _, err := chargeOrder.Execute(ctx, orderID); err != nil {
			// The order stays created; only the payment attempts were rolled back
			return recordPaymentFailure.Execute(ctx, orderID)
		}
		return OrderStepResult{OrderID: orderID, Status: "paid"}, nil
	},
)
```

**Key points:**

- The activity is the unit of atomicity: everything one attempt does commits or rolls back together
- Split work into activities where you need separate commits
- Here the order and its `order.created` event are committed; the failed payment leaves no `payments` row and no `payment.charged` event

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

    loop Every poll (1 second by default, longer while idle)
        R->>DB: Claim pending events (status='processing')
        DB->>R: Return claimed events

        loop For each event
            R->>DB: UPDATE retry_count++
            R->>B: POST CloudEvent
            alt Delivered
                B->>R: 2xx
                R->>DB: UPDATE status='published'
            else Not delivered
                B->>R: Error
                R->>DB: UPDATE status='pending'
            end
        end
    end
```

**Key Points:**

- Polls the database every second by default; while there is nothing to send, the interval doubles up to 30 seconds and resets as soon as events show up. On PostgreSQL, LISTEN/NOTIFY wakes the relayer when an event is written
- Publishes events as CloudEvents (HTTP)
- Sends a failed event again on a later poll, up to 5 attempts in all
- Claims events atomically, so the relayers of several workers can share one database
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
	romancy.WithOutboxInterval(2*time.Second), // Poll every 2 seconds (default: 1s)
	romancy.WithOutboxBatchSize(50),           // Claim up to 50 events per poll (default: 100)
)
```

With `WithOutbox(true)`, every worker runs a relayer from `app.Start` until `app.Shutdown`.

### Advanced Configuration

To run a relayer yourself, for example in a separate process, leave `WithOutbox` off and create one with `outbox.NewRelayer`:

```go
relayer := outbox.NewRelayer(app.Storage(), outbox.RelayerConfig{
	TargetURL:    "http://localhost:8080/events",
	PollInterval: 2 * time.Second, // Poll every 2 seconds
	MaxRetries:   5,               // Send each event at most 5 times
	BatchSize:    20,              // Claim up to 20 events per poll
})

ctx := context.Background()

// Start runs the relayer in the background until Stop is called or ctx is done
relayer.Start(ctx)

// Stop gracefully
relayer.Stop()
```

**Configuration Options (`outbox.RelayerConfig`):**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `TargetURL` | `string` | Required | CloudEvents endpoint the events are posted to |
| `PollInterval` | `time.Duration` | `1s` | Polling interval while there are events to send |
| `BatchSize` | `int` | `100` | Maximum number of events claimed per poll |
| `MaxRetries` | `int` | `5` | Maximum number of send attempts per event |
| `MaxBackoff` | `time.Duration` | `30s` | Longest polling interval while there is nothing to send |
| `ProcessingTimeout` | `time.Duration` | `5m` | How long a claimed event may wait before another relayer claims it again (a relayer that crashed) |
| `CustomSender` | `outbox.EventSender` | CloudEvents over HTTP | Replaces the HTTP sender |
| `WakeEvent` | `<-chan struct{}` | `nil` | Wakes the relayer at once (the app connects PostgreSQL NOTIFY here) |

### Retry Logic and Error Handling

**How it works:**

1. **Claim**: every poll claims up to `BatchSize` pending events and marks them `'processing'`, so concurrent relayers take different events
2. **Retry Check**: an event already sent `MaxRetries` times is marked `'failed'` and not sent again
3. **Send Attempt**: the relayer increments `retry_count` and posts the event
4. **Success** (2xx response): status → `'published'`
5. **Failure** (any error: an HTTP error status, a network error, a timeout): status → `'pending'`, and a later poll sends the event again

**Example with default settings** (`PollInterval` 1s, `MaxRetries` 5):

| Poll | retry_count | Result | Status afterwards |
|------|-------------|--------|-------------------|
| 1 | 0 → 1 | 503 Service Unavailable | `'pending'` |
| 2 | 1 → 2 | Network timeout | `'pending'` |
| 3 | 2 → 3 | 400 Bad Request | `'pending'` |
| 4 | 3 → 4 | 500 Internal Server Error | `'pending'` |
| 5 | 4 → 5 | 503 Service Unavailable | `'pending'` |
| 6 | 5 | Not sent | `'failed'` |

**Important Notes:**

- **Every Error Is Retried**: 4xx responses too, so a malformed event is sent `MaxRetries` times before it is marked `'failed'`
- **Retry Timing**: a failed event is sent again on the next poll, about `PollInterval` later plus up to 30% jitter; there is no per-event backoff
- **Retry Limit**: with `MaxRetries` 5, an event is sent at most 5 times
- **Crashed Relayers**: an event claimed by a relayer that stopped is claimed again after `ProcessingTimeout`
- **Duplicates**: an event sent by a relayer that crashed before marking it `'published'` is sent again, so consumers must be idempotent

---

## Event Status Lifecycle

Romancy's relayer moves events in the `outbox_events` table through four statuses:

```
pending → processing → published  (Delivered)
                     → pending    (Not delivered, sent again later)
                     → failed     (Sent MaxRetries times without success)
```

**Status Descriptions:**

| Status | Meaning | Retryable | How it happens |
|--------|---------|-----------|----------------|
| `pending` | Event waiting to be sent | ✅ Yes | Initial status, and the status after a failed send |
| `processing` | Event claimed by a relayer | ✅ Yes | A poll claimed it; claimed again after `ProcessingTimeout` |
| `published` | Event delivered | ❌ No | 2xx HTTP response from the broker |
| `failed` | Event given up | ❌ No | `retry_count` reached `MaxRetries` |

The schema is shared with Edda, whose relayer also uses `invalid` (4xx responses) and `expired` (events past a maximum age). Romancy's relayer does not set them.

**Database Schema:**

```sql
-- SQLite (from durax-io/schema; PostgreSQL and MySQL use equivalent types)
CREATE TABLE outbox_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    event_source TEXT NOT NULL,
    data_type TEXT NOT NULL DEFAULT 'json',
    event_data TEXT,
    event_data_binary BLOB,
    content_type TEXT NOT NULL DEFAULT 'application/json',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    published_at TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    retry_count INTEGER DEFAULT 0,
    last_error TEXT,
    claimed_at TEXT,  -- when a relayer claimed the event
    CONSTRAINT valid_outbox_status CHECK (status IN ('pending', 'processing', 'published', 'failed', 'invalid', 'expired'))
);

CREATE INDEX idx_outbox_status ON outbox_events(status, created_at);
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
type InventoryResult struct {
	TotalAmount float64 `json:"total_amount"`
}

type ChargeResult struct {
	TransactionID string `json:"transaction_id"`
}

type ShipmentResult struct {
	TrackingNumber string `json:"tracking_number"`
}

type OrderProcessingResult struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

type OrderProcessingInput struct {
	OrderID string      `json:"order_id"`
	Items   []OrderItem `json:"items"`
}

var orderProcessingWorkflow = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, input OrderProcessingInput) (OrderProcessingResult, error) {
		orderID := input.OrderID
		// Step 1: Reserve inventory
		// → Sends "inventory.reserved" event
		inventory, err := reserveInventory.Execute(ctx, ReserveInput{OrderID: orderID, Items: input.Items})
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Step 2: Charge payment
		// → Sends "payment.charged" event
		_, err = chargePayment.Execute(ctx, ChargeInput{OrderID: orderID, Amount: inventory.TotalAmount})
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Step 3: Ship order
		// → Sends "order.shipped" event
		_, err = shipOrder.Execute(ctx, orderID)
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Step 4: Complete order
		// → Sends "order.completed" event
		return completeOrder.Execute(ctx, orderID)
	},
)

// Events are sent from activities: workflow code runs again on every
// replay, and an event it sent would be sent again each time.
var completeOrder = romancy.DefineActivity("complete_order",
	func(ctx context.Context, orderID string) (OrderProcessingResult, error) {
		err := romancy.SendEventTransactional(romancy.GetWorkflowContext(ctx),
			"order.completed",
			"order-service",
			map[string]any{"order_id": orderID, "status": "completed"},
		)
		if err != nil {
			return OrderProcessingResult{}, err
		}
		return OrderProcessingResult{OrderID: orderID, Status: "completed"}, nil
	},
)
```

### 2. Compensation with Events

Send events during compensation (rollback):

```go
func releaseInventory(ctx context.Context, input ReserveInput) error {
	fmt.Printf("Releasing inventory for order: %s\n", input.OrderID)

	wfCtx := romancy.GetWorkflowContext(ctx)

	// Send compensation event
	return romancy.SendEventTransactional(wfCtx,
		"inventory.released",
		"order-service",
		map[string]any{
			"order_id": input.OrderID,
			"reason":   "order_failed",
		},
	)
}
```

### 3. Saga Pattern Integration

```go
func cancelReservation(ctx context.Context, bookingID string) error {
	wfCtx := romancy.GetWorkflowContext(ctx)

	// Send cancellation event
	return romancy.SendEventTransactional(wfCtx,
		"hotel.cancelled",
		"booking-service",
		map[string]any{"booking_id": bookingID},
	)
}

type ReserveHotelResult struct {
	ReservationID string `json:"reservation_id"`
}

var reserveHotel = romancy.DefineActivity("reserve_hotel",
	func(ctx context.Context, bookingID string) (ReserveHotelResult, error) {
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
			return ReserveHotelResult{}, err
		}

		return ReserveHotelResult{ReservationID: reservationID}, nil
	},
	romancy.WithCompensation[string, ReserveHotelResult](cancelReservation),
)
```

---

## Best Practices

### 1. Activity Transaction Control

Activities are **automatically transactional**. All operations within an activity (including `SendEventTransactional()`) are executed within a single transaction.

```go
type OrderActivityResult struct {
	OrderID string `json:"order_id"`
}

// ✅ Default: Automatic transaction (recommended)
var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID string) (OrderActivityResult, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// All operations are automatically transactional:
		// - Workflow history recording
		// - Event publishing to outbox
		err := romancy.SendEventTransactional(wfCtx, "order.created", "order-service", map[string]any{"order_id": orderID})
		if err != nil {
			return OrderActivityResult{}, err
		}

		return OrderActivityResult{OrderID: orderID}, nil
	},
)

// ✅ With session access for custom database operations
var createOrderWithDB = romancy.DefineActivity("create_order_with_db",
	func(ctx context.Context, orderID string) (OrderActivityResult, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session
		session := wfCtx.Session()

		// Your database operations
		_, err := session.ExecContext(ctx, "INSERT INTO orders (order_id) VALUES (?)", orderID)
		if err != nil {
			return OrderActivityResult{}, err
		}

		// Events in same transaction
		err = romancy.SendEventTransactional(wfCtx, "order.created", "order-service", map[string]any{"order_id": orderID})
		if err != nil {
			return OrderActivityResult{}, err
		}

		return OrderActivityResult{OrderID: orderID}, nil
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
	if err := rows.Scan(&status, &count); err != nil {
		log.Fatal(err)
	}
	statusCounts[status] = count
}

fmt.Println("📊 Outbox Status Summary:")
fmt.Printf("  - Pending:    %d\n", statusCounts["pending"])
fmt.Printf("  - Processing: %d\n", statusCounts["processing"])
fmt.Printf("  - Published:  %d\n", statusCounts["published"])
fmt.Printf("  - Failed:     %d\n", statusCounts["failed"])

// Alert on permanently failed events
if statusCounts["failed"] > 0 {
	fmt.Printf("⚠️ %d events failed permanently\n", statusCounts["failed"])
	fmt.Println("   → Check broker health, the event format, or network connectivity")
}
```

### 4. Handle Idempotency

Since the outbox pattern provides **at-least-once delivery**, consumers must handle duplicate events:

```go
// Consumer side (idempotency key)
func HandleEvent(w http.ResponseWriter, r *http.Request) {
	// The relayer sends in binary mode (ce-* headers, the data as the body);
	// NewEventFromHTTPRequest reads binary and structured mode.
	event, err := cloudevents.NewEventFromHTTPRequest(r)
	if err != nil {
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

type EcommerceOrderResult struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

// Activity with custom database operations
type CreateEcommerceOrderInput struct {
	OrderID       string  `json:"order_id"`
	CustomerEmail string  `json:"customer_email"`
	Amount        float64 `json:"amount"`
}

var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, input CreateEcommerceOrderInput) (EcommerceOrderResult, error) {
		orderID := input.OrderID
		customerEmail := input.CustomerEmail
		amount := input.Amount
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Access Romancy-managed session (same database)
		session := wfCtx.Session()

		// 1. Save order to YOUR table (same database as Romancy)
		_, err := session.ExecContext(ctx, `
			INSERT INTO orders (order_id, customer_email, amount, status, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, orderID, customerEmail, amount, "pending", time.Now())
		if err != nil {
			return EcommerceOrderResult{}, err
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
			return EcommerceOrderResult{}, err
		}

		// 3. Romancy automatically commits (or rolls back on error)
		// If event publishing fails, order creation rolls back
		// If order creation fails, event publishing rolls back

		return EcommerceOrderResult{OrderID: orderID, Status: "created"}, nil
	},
)

// Workflow
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input CreateEcommerceOrderInput) (EcommerceOrderResult, error) {
		// Create order (atomic with event)
		result, err := createOrder.Execute(ctx, input)
		if err != nil {
			return EcommerceOrderResult{}, err
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
	romancy.RegisterWorkflow(app, orderWorkflow)

	ctx := context.Background()

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, orderWorkflow, CreateEcommerceOrderInput{
		OrderID:       "ORD-12345",
		CustomerEmail: "customer@example.com",
		Amount:        149.99,
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
