---
title: "Your First Workflow"
weight: 4
---

In this tutorial, you'll build a complete **order processing workflow** with compensation (Saga pattern). This workflow demonstrates:

- ✅ Type-safe inputs/outputs with Go structs
- ✅ Automatic compensation on failure
- ✅ Durable execution with crash recovery
- ✅ Event publishing with transactional outbox

## Prerequisites

Before starting, make sure you have Romancy installed:

```bash
go get github.com/i2y/romancy
```

If you haven't set up your Go environment, see the [Installation Guide](/docs/getting-started/installation).

## What We're Building

An e-commerce order processing system that:

1. **Reserves inventory** for ordered items
2. **Processes payment** for the order
3. **Ships the order** to the customer
4. **Publishes events** at each step
5. **Automatically rolls back** if any step fails

## Step 1: Define Data Structures

Create `main.go` and start with data structures:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"regexp"

	"github.com/i2y/romancy"
)

// OrderItem represents a single item in an order
type OrderItem struct {
	ProductID string  `json:"product_id"`
	Quantity  int     `json:"quantity"`  // At least 1
	UnitPrice float64 `json:"unit_price"` // Positive price
}

// ShippingAddress represents customer shipping address
type ShippingAddress struct {
	Street     string `json:"street"`
	City       string `json:"city"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

// OrderInput is the input for order processing workflow
type OrderInput struct {
	OrderID         string          `json:"order_id"`          // e.g., ORD-123
	CustomerEmail   string          `json:"customer_email"`
	Items           []OrderItem     `json:"items"`
	ShippingAddress ShippingAddress `json:"shipping_address"`
}

// OrderResult is the result of order processing
type OrderResult struct {
	OrderID            string  `json:"order_id"`
	Status             string  `json:"status"`
	TotalAmount        float64 `json:"total_amount"`
	ConfirmationNumber string  `json:"confirmation_number"`
}
```

## Step 2: Create Activities

Add the three main activities with compensation:

```go
// Compensation functions
var cancelInventoryReservation = romancy.DefineCompensation("cancel_inventory_reservation",
	func(ctx context.Context, orderID string) error {
		fmt.Printf("❌ Cancelling inventory reservation for %s\n", orderID)
		return nil
	},
)

var refundPayment = romancy.DefineCompensation("refund_payment",
	func(ctx context.Context, orderID string, amount float64) error {
		fmt.Printf("❌ Refunding payment for %s: $%.2f\n", orderID, amount)
		return nil
	},
)

// Activity result types
type ReservationResult struct {
	ReservationID string  `json:"reservation_id"`
	TotalAmount   float64 `json:"total_amount"`
}

type PaymentResult struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
	Status        string  `json:"status"`
}

type ShipmentResult struct {
	TrackingNumber string `json:"tracking_number"`
	Status         string `json:"status"`
}

// Activities with compensation links
var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string, items []OrderItem) (ReservationResult, error) {
		var total float64
		for _, item := range items {
			total += float64(item.Quantity) * item.UnitPrice
		}

		fmt.Printf("📦 Reserving inventory for %s: $%.2f\n", orderID, total)

		return ReservationResult{
			ReservationID: fmt.Sprintf("RES-%s", orderID),
			TotalAmount:   total,
		}, nil
	},
	romancy.WithCompensation(cancelInventoryReservation),
)

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string, amount float64, customerEmail string) (PaymentResult, error) {
		fmt.Printf("💳 Processing payment for %s: $%.2f\n", orderID, amount)

		return PaymentResult{
			TransactionID: fmt.Sprintf("TXN-%s", orderID),
			Amount:        amount,
			Status:        "completed",
		}, nil
	},
	romancy.WithCompensation(refundPayment),
)

// Activity 3: Ship Order (no compensation - final step)
var shipOrder = romancy.DefineActivity("ship_order",
	func(ctx context.Context, orderID string, address ShippingAddress) (ShipmentResult, error) {
		fmt.Printf("🚚 Shipping %s to %s, %s\n", orderID, address.City, address.Country)

		return ShipmentResult{
			TrackingNumber: fmt.Sprintf("TRACK-%s", orderID),
			Status:         "shipped",
		}, nil
	},
)
```

## Step 3: Create the Workflow

Now orchestrate the activities:

```go
var orderProcessingWorkflow = romancy.DefineWorkflow("order_processing",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderResult, error) {
		// Step 1: Reserve inventory
		reservation, err := reserveInventory.Execute(ctx, input.OrderID, input.Items)
		if err != nil {
			return OrderResult{}, err
		}

		// Step 2: Process payment
		payment, err := processPayment.Execute(ctx, input.OrderID, reservation.TotalAmount, input.CustomerEmail)
		if err != nil {
			return OrderResult{}, err
		}

		// Step 3: Ship order
		shipment, err := shipOrder.Execute(ctx, input.OrderID, input.ShippingAddress)
		if err != nil {
			return OrderResult{}, err
		}

		// Success! Return result
		return OrderResult{
			OrderID:            input.OrderID,
			Status:             "completed",
			TotalAmount:        payment.Amount,
			ConfirmationNumber: shipment.TrackingNumber,
		}, nil
	},
)
```

## Step 4: Run the Workflow

Add the main function:

```go
func main() {
	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	// Start the app (required before starting workflows)
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Create order input
	order := OrderInput{
		OrderID:       "ORD-12345",
		CustomerEmail: "customer@example.com",
		Items: []OrderItem{
			{ProductID: "PROD-1", Quantity: 2, UnitPrice: 29.99},
			{ProductID: "PROD-2", Quantity: 1, UnitPrice: 49.99},
		},
		ShippingAddress: ShippingAddress{
			Street:     "1-2-3 Dogenzaka",
			City:       "Shibuya",
			PostalCode: "150-0001",
			Country:    "Japan",
		},
	}

	// Start workflow
	fmt.Println("Starting order processing workflow...")
	instanceID, err := romancy.StartWorkflow(ctx, app, orderProcessingWorkflow, order)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\n✅ Workflow started: %s\n", instanceID)

	// Get result
	instance, err := app.Storage().GetInstance(ctx, instanceID)
	if err != nil {
		log.Fatal(err)
	}

	if instance.Status == "completed" {
		result := instance.OutputData
		fmt.Println("📊 Order completed:")
		fmt.Printf("   - Order ID: %s\n", result["order_id"])
		fmt.Printf("   - Total: $%.2f\n", result["total_amount"])
		fmt.Printf("   - Tracking: %s\n", result["confirmation_number"])
	}
}
```

## Step 5: Test Happy Path

Run the workflow:

```bash
go run main.go
```

**Expected output:**

```
Starting order processing workflow...

📦 Reserving inventory for ORD-12345: $109.97
💳 Processing payment for ORD-12345: $109.97
🚚 Shipping ORD-12345 to Shibuya, Japan

✅ Workflow started: <instance_id>
📊 Order completed:
   - Order ID: ORD-12345
   - Total: $109.97
   - Tracking: TRACK-ORD-12345
```

## Step 6: Test Failure & Compensation

Let's simulate a shipping failure to see compensation in action.

Modify `shipOrder` to fail:

```go
var shipOrder = romancy.DefineActivity("ship_order",
	func(ctx context.Context, orderID string, address ShippingAddress) (ShipmentResult, error) {
		fmt.Printf("🚚 Shipping %s to %s, %s\n", orderID, address.City, address.Country)

		// Simulate shipping failure
		return ShipmentResult{}, fmt.Errorf("shipping service unavailable")
	},
)
```

Run again:

```bash
go run main.go
```

**Expected output:**

```
📦 Reserving inventory for ORD-12345: $109.97
💳 Processing payment for ORD-12345: $109.97
🚚 Shipping ORD-12345 to Shibuya, Japan
💥 Error: shipping service unavailable

❌ Refunding payment for ORD-12345: $109.97
❌ Cancelling inventory reservation for ORD-12345

Error: shipping service unavailable
```

**What happened:**

1. Inventory reserved ✅
2. Payment processed ✅
3. Shipping failed ❌
4. **Automatic compensation in reverse order:**
   - Refund payment ✅
   - Cancel inventory reservation ✅

This is the **Saga pattern** - distributed rollback through compensation functions.

## Step 7: Understanding Crash Recovery

Romancy's durable execution ensures workflows survive crashes through **deterministic replay**. When a workflow crashes mid-execution:

1. ✅ **Activity results are saved** to the database before execution continues
2. ✅ **Workflow state is preserved** (current step, history, locks)
3. ✅ **Automatic recovery** detects and resumes stale workflows

### How Automatic Recovery Works

In production environments with long-running Romancy app instances:

- **Crash detection**: Romancy's background task checks for stale locks every 60 seconds
- **Auto-resume**: Crashed workflows are automatically resumed when their lock timeout expires
  - Both normal execution and rollback execution are automatically resumed
  - Default timeout: 5 minutes (300 seconds)
  - Workflows resume from their last checkpoint using deterministic replay
- **Deterministic replay**: Previously executed activities return cached results from history
- **Resume from checkpoint**: Only remaining activities execute fresh

### Workflows Waiting for Events or Timers

Workflows in special waiting states are handled differently:

- **Waiting for Events**: Resumed immediately when the awaited event arrives (not on a fixed schedule)
- **Waiting for Timers**: Checked every 10 seconds and resumed when the timer expires
- These workflows are **not** included in the 60-second crash recovery cycle

### Crash Recovery in Action

**Production scenario:**

```go
// Server starts and runs continuously
app := romancy.NewApp(
	romancy.WithDatabase("postgres://user:password@localhost/orders"),
	romancy.WithWorkerID("worker-1"),
)
app.Start(ctx)

// Workflow starts executing
instanceID, _ := romancy.StartWorkflow(ctx, app, orderProcessingWorkflow, order)

// Server crashes after payment step
// → inventory reservation: ✅ saved
// → payment: ✅ saved
// → shipping: ❌ not executed

// Server restarts (automatic or manual)
// → Romancy's background task detects stale workflow (lock > 5 minutes)
// → Automatically resumes workflow from last checkpoint
// → inventory reservation: ⚡ replayed from history (instant)
// → payment: ⚡ replayed from history (instant)
// → shipping: 🚚 executes fresh
```

### Why Activities Execute Exactly Once

Romancy's replay mechanism ensures idempotency:

1. **Before execution**: Check if result exists in history for current step
2. **If found**: Return cached result (replay)
3. **If not found**: Execute activity and save result to history
4. **Side effects**: External API calls, payments, etc. happen exactly once

**Example:**

```go
type PaymentResult struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
	Status        string  `json:"status"`
}

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string, amount float64) (PaymentResult, error) {
		// This code executes ONCE per workflow instance
		// On crash recovery, cached result is returned
		fmt.Printf("💳 Processing payment for %s: $%.2f\n", orderID, amount)

		paymentResult, err := externalPaymentAPI.Charge(amount)
		if err != nil {
			return PaymentResult{}, err
		}

		return PaymentResult{
			TransactionID: paymentResult.ID,
			Amount:        amount,
			Status:        "completed",
		}, nil
	},
	romancy.WithCompensation(refundPayment),
)
```

**On first execution:**

- Code executes
- External payment API is called
- Result saved to database
- Output: `💳 Processing payment for ORD-12345: $109.97`

**On crash recovery (replay):**

- Code does NOT execute
- Result loaded from database
- External payment API is NOT called again
- No output (instant return)

### Testing Crash Recovery

For a full demonstration, you would need:

1. Long-running Romancy app instance (e.g., HTTP server)
2. Workflow that crashes mid-execution
3. Wait 5+ minutes for automatic recovery
4. Observe workflow resume from last checkpoint

**Note**: Running the same script twice creates **separate workflow instances** with different UUIDs. To test replay on the **same instance**, you need a persistent server and workflow resumption logic.

## What You've Learned

- ✅ **Type-Safe Structs**: Go structs for inputs and outputs
- ✅ **Activities**: Business logic units with automatic history recording
- ✅ **Compensation**: Automatic rollback with `WithCompensation`
- ✅ **Saga Pattern**: Distributed transaction management
- ✅ **Durable Execution**: Workflows survive crashes
- ✅ **Deterministic Replay**: Activities execute exactly once

## Next Steps

- **[Saga Pattern](/docs/core-features/saga-compensation)**: Deep dive into compensation
- **[Event Handling](/docs/core-features/events/wait-event)**: Wait for external events
- **[Transactional Outbox](/docs/core-features/transactional-outbox)**: Reliable event publishing
- **[Examples](/docs/examples/ecommerce)**: More real-world examples
