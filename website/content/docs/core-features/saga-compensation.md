---
title: "Saga Pattern & Compensation"
weight: 2
---

The Saga pattern is a core feature of Romancy that enables distributed transaction management across multiple services and activities. It provides automatic compensation (rollback) when workflows fail, ensuring data consistency without requiring traditional database transactions.

## What is the Saga Pattern?

The Saga pattern is a design pattern for managing distributed transactions by breaking them into a series of local transactions. Each local transaction updates data within a single service and publishes an event or message. If a local transaction fails, a series of compensating transactions are executed to undo the changes made by previous transactions.

## Key Concepts

### Compensation Functions

Compensation functions are special activities that undo the effects of previously executed activities. They run automatically when a workflow fails, executing in reverse order of the original activities.

### DefineCompensation and WithCompensation

In Romancy, you define compensation functions using `DefineCompensation` and link them to activities using `WithCompensation`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
)

// Define compensation function
var cancelInventoryReservation = romancy.DefineCompensation("cancel_inventory_reservation",
	func(ctx context.Context, orderID, itemID string) error {
		// Cancel reservation logic
		fmt.Printf("Cancelled inventory reservation for %s\n", orderID)
		return nil
	},
)

// Define activity with compensation link
var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID, itemID string) (map[string]any, error) {
		// Reserve inventory logic
		fmt.Printf("Reserved inventory for %s\n", orderID)
		return map[string]any{"reservation_id": fmt.Sprintf("RES-%s", itemID)}, nil
	},
	romancy.WithCompensation(cancelInventoryReservation), // Link compensation
)
```

## How Compensation Works

### Automatic Triggering

When a workflow fails (returns an error), Romancy automatically:

1. Stops workflow execution
2. Identifies all successfully completed activities
3. Executes their compensation functions in reverse order
4. Marks the workflow as failed with compensation completed

### Execution Order

Compensation functions always execute in **reverse order** of activity execution:

```go
var orderSaga = romancy.DefineWorkflow("order_saga",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		reserveInventory.Execute(ctx, orderID, "ITEM-123")  // Step 1
		chargePayment.Execute(ctx, orderID, 99.99)          // Step 2
		shipOrder.Execute(ctx, orderID)                     // Step 3 (fails)

		// On failure, compensations run as:
		// 1. Compensation for Step 2 (refund payment)
		// 2. Compensation for Step 1 (cancel reservation)
		// Step 3 has no compensation as it failed

		return map[string]any{"status": "completed"}, nil
	},
)
```

### Partial Completion Handling

Only **successfully completed** activities are compensated:

- ✅ Completed activities → Compensation executed
- ❌ Failed activities → No compensation needed
- ⏭️ Not-yet-executed activities → No compensation needed

## Implementation Example

### Complete Order Processing Saga

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

// Data structures
type OrderItem struct {
	ID       string `json:"id"`
	Quantity int    `json:"qty"`
}

type ShippingAddress struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

// Define compensation functions

var cancelInventoryReservation = romancy.DefineCompensation("cancel_inventory_reservation",
	func(ctx context.Context, orderID string, items []OrderItem) error {
		fmt.Printf("Cancelled inventory reservations for order %s\n", orderID)
		return nil
	},
)

var refundPayment = romancy.DefineCompensation("refund_payment",
	func(ctx context.Context, orderID string, amount float64, cardToken string) error {
		fmt.Printf("Refunded $%.2f for order %s\n", amount, orderID)
		return nil
	},
)

// Define activities with compensation links

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string, items []OrderItem) (map[string]any, error) {
		fmt.Printf("Reserved inventory for order %s\n", orderID)
		reservationIDs := make([]string, len(items))
		for i, item := range items {
			reservationIDs[i] = fmt.Sprintf("RES-%s", item.ID)
		}
		return map[string]any{"reservation_ids": reservationIDs}, nil
	},
	romancy.WithCompensation(cancelInventoryReservation),
)

var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, orderID string, amount float64, cardToken string) (map[string]any, error) {
		fmt.Printf("Charged $%.2f for order %s\n", amount, orderID)
		return map[string]any{
			"transaction_id": fmt.Sprintf("TXN-%s", orderID),
			"amount":         amount,
		}, nil
	},
	romancy.WithCompensation(refundPayment),
)

var createShipment = romancy.DefineActivity("create_shipment",
	func(ctx context.Context, orderID string, address ShippingAddress) (map[string]any, error) {
		fmt.Printf("Creating shipment for order %s\n", orderID)
		// This might fail if shipping service is unavailable
		if address.Street == "invalid" {
			return nil, fmt.Errorf("invalid shipping address")
		}
		return map[string]any{"shipment_id": fmt.Sprintf("SHIP-%s", orderID)}, nil
	},
)

// Define the saga workflow

var orderProcessingSaga = romancy.DefineWorkflow("order_processing_saga",
	func(ctx *romancy.WorkflowContext, orderID string, items []OrderItem, amount float64, cardToken string, shippingAddress ShippingAddress) (map[string]any, error) {
		// Reserve inventory
		reservation, err := reserveInventory.Execute(ctx, orderID, items)
		if err != nil {
			return nil, err
		}

		// Charge payment
		payment, err := chargePayment.Execute(ctx, orderID, amount, cardToken)
		if err != nil {
			return nil, err
		}

		// Create shipment (might fail)
		shipment, err := createShipment.Execute(ctx, orderID, shippingAddress)
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"order_id":    orderID,
			"reservation": reservation,
			"payment":     payment,
			"shipment":    shipment,
		}, nil
	},
)

// Application setup

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// This will succeed
	successID, err := romancy.StartWorkflow(ctx, app, orderProcessingSaga,
		OrderInput{
			OrderID: "ORD-001",
			Items:   []OrderItem{{ID: "ITEM-1", Quantity: 2}},
			Amount:  99.99,
			Token:   "tok_valid",
			Address: ShippingAddress{Street: "123 Main St", City: "Springfield"},
		})
	if err != nil {
		log.Printf("Success workflow started: %s\n", successID)
	}

	// This will fail and trigger compensation
	failID, err := romancy.StartWorkflow(ctx, app, orderProcessingSaga,
		OrderInput{
			OrderID: "ORD-002",
			Items:   []OrderItem{{ID: "ITEM-2", Quantity: 1}},
			Amount:  49.99,
			Token:   "tok_valid",
			Address: ShippingAddress{Street: "invalid", City: "Unknown"},
		})
	if err != nil {
		log.Printf("Failed workflow (compensation triggered): %s, error: %v\n", failID, err)
	}
}
```

## Best Practices

### 1. Idempotent Compensations

Make compensation functions idempotent - they should handle being called multiple times safely:

```go
var refundPayment = romancy.DefineCompensation("refund_payment",
	func(ctx context.Context, orderID string, amount float64) error {
		// Check if already refunded
		if isAlreadyRefunded(orderID) {
			return nil // Already refunded, skip
		}

		// Perform refund
		return processRefund(orderID, amount)
	},
)

var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, orderID string, amount float64) (map[string]any, error) {
		transactionID, err := processPayment(orderID, amount)
		if err != nil {
			return nil, err
		}
		return map[string]any{"transaction_id": transactionID}, nil
	},
	romancy.WithCompensation(refundPayment),
)
```

### 2. Store Compensation Data

Activities should return data needed for compensation:

```go
var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string, items []OrderItem) (map[string]any, error) {
		reservationIDs := make([]string, 0)
		for _, item := range items {
			resID, err := reserveItem(item.ID, item.Quantity)
			if err != nil {
				return nil, err
			}
			reservationIDs = append(reservationIDs, resID)
		}

		// Return data needed for compensation
		return map[string]any{
			"reservation_ids": reservationIDs,
			"items":           items,
		}, nil
	},
	romancy.WithCompensation(cancelReservation),
)
```

### 3. Handle Partial State

Consider partial completion within activities:

```go
var reserveMultipleItems = romancy.DefineActivity("reserve_multiple_items",
	func(ctx context.Context, items []OrderItem) (map[string]any, error) {
		reserved := make([]string, 0)

		for _, item := range items {
			resID, err := reserveItem(item.ID, item.Quantity)
			if err != nil {
				// Manually compensate partial reservations
				for _, r := range reserved {
					cancelReservation(r)
				}
				return nil, err
			}
			reserved = append(reserved, resID)
		}

		return map[string]any{"all_reserved": reserved}, nil
	},
)
```

### 4. Timeout Handling

Set appropriate timeouts for compensation functions:

```go
var compensateLongRunning = romancy.DefineCompensation("compensate_long_running",
	func(ctx context.Context, data map[string]any) error {
		// Create context with timeout
		timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// Perform compensation with timeout
		return performCompensation(timeoutCtx, data)
	},
)

var longRunningActivity = romancy.DefineActivity("long_running_activity",
	func(ctx context.Context, data map[string]any) (map[string]any, error) {
		result, err := performLongOperation(data)
		if err != nil {
			return nil, err
		}
		return result, nil
	},
	romancy.WithCompensation(compensateLongRunning),
)
```

## Advanced Features

### Conditional Compensation

You can conditionally execute compensation based on activity results:

```go
var conditionalCompensation = romancy.DefineCompensation("conditional_compensation",
	func(ctx context.Context, shouldCompensate bool) error {
		if !shouldCompensate {
			return nil // Skip compensation
		}

		return performCleanup()
	},
)

var conditionalActivity = romancy.DefineActivity("conditional_activity",
	func(ctx context.Context, shouldCompensate bool) (map[string]any, error) {
		result, err := performAction()
		if err != nil {
			return nil, err
		}
		result["needs_compensation"] = shouldCompensate
		return result, nil
	},
	romancy.WithCompensation(conditionalCompensation),
)
```

### Nested Sagas

Sagas can call other sagas, with compensation cascading through the hierarchy:

```go
var parentSaga = romancy.DefineWorkflow("parent_saga",
	func(ctx *romancy.WorkflowContext, orderData map[string]any) (map[string]any, error) {
		// If child saga fails, its compensations run first
		childData := orderData["child_data"].(map[string]any)
		childResult, err := childSaga.Execute(ctx, childData)
		if err != nil {
			return nil, err
		}

		// Then parent activities
		parentData := orderData["parent_data"].(map[string]any)
		parentResult, err := parentActivity.Execute(ctx, parentData)
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"child":  childResult,
			"parent": parentResult,
		}, nil
	},
)
```

### Manual Compensation Trigger

While Romancy handles automatic compensation, you can also manually trigger compensation:

```go
var manualCompensationSaga = romancy.DefineWorkflow("manual_compensation_saga",
	func(ctx *romancy.WorkflowContext, data map[string]any) (map[string]any, error) {
		result, err := riskyActivity.Execute(ctx, data)
		if err != nil {
			return nil, err
		}

		if result["needs_rollback"].(bool) {
			// Manually trigger compensation by returning error
			return nil, fmt.Errorf("manual rollback triggered")
		}

		return result, nil
	},
)
```

## Common Use Cases

### E-commerce Order Processing
- Reserve inventory → Charge payment → Create shipment → Send confirmation
- On failure: Cancel shipment → Refund payment → Release inventory

### Travel Booking
- Book flight → Reserve hotel → Rent car → Process payment
- On failure: Cancel car → Cancel hotel → Cancel flight → Refund payment

### Financial Transactions
- Lock source account → Lock target account → Transfer funds → Update ledgers
- On failure: Reverse ledgers → Unlock accounts → Restore balances

### Microservices Orchestration
- Call Service A → Call Service B → Call Service C → Aggregate results
- On failure: Compensate C → Compensate B → Compensate A

## Monitoring and Debugging

### Logging Compensation

Add detailed logging to compensation functions:

```go
var compensateCritical = romancy.DefineCompensation("compensate_critical",
	func(ctx context.Context, data map[string]any) error {
		log.Printf("Starting compensation for %v\n", data["id"])

		err := performCompensation(data)
		if err != nil {
			log.Printf("Compensation failed: %v\n", err)
			return err
		}

		log.Printf("Compensation successful for %v\n", data["id"])
		return nil
	},
)

var criticalActivity = romancy.DefineActivity("critical_activity",
	func(ctx context.Context, data map[string]any) (map[string]any, error) {
		result, err := performCriticalOperation(data)
		if err != nil {
			return nil, err
		}
		return result, nil
	},
	romancy.WithCompensation(compensateCritical),
)
```

## Related Topics

- [Workflows and Activities](/docs/core-features/workflows-activities) - Learn about basic workflow concepts
- [Durable Execution](/docs/core-features/durable-execution/replay) - Understand replay and recovery
- [Transactional Outbox](/docs/core-features/transactional-outbox) - Ensure message delivery consistency
- [Examples: Saga Pattern](/docs/examples/saga) - See practical examples
