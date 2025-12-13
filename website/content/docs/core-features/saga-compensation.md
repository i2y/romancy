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

type ReservationResult struct {
	ReservationID string `json:"reservation_id"`
}

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
	func(ctx context.Context, orderID, itemID string) (ReservationResult, error) {
		// Reserve inventory logic
		fmt.Printf("Reserved inventory for %s\n", orderID)
		return ReservationResult{ReservationID: fmt.Sprintf("RES-%s", itemID)}, nil
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
type OrderSagaResult struct {
	Status string `json:"status"`
}

var orderSaga = romancy.DefineWorkflow("order_saga",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderSagaResult, error) {
		_, _ = reserveInventory.Execute(ctx, orderID, "ITEM-123")  // Step 1
		_, _ = chargePayment.Execute(ctx, orderID, 99.99)          // Step 2
		_, _ = shipOrder.Execute(ctx, orderID)                     // Step 3 (fails)

		// On failure, compensations run as:
		// 1. Compensation for Step 2 (refund payment)
		// 2. Compensation for Step 1 (cancel reservation)
		// Step 3 has no compensation as it failed

		return OrderSagaResult{Status: "completed"}, nil
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

type OrderInput struct {
	OrderID string          `json:"order_id"`
	Items   []OrderItem     `json:"items"`
	Amount  float64         `json:"amount"`
	Token   string          `json:"token"`
	Address ShippingAddress `json:"address"`
}

// Result structures
type InventoryReservationResult struct {
	ReservationIDs []string `json:"reservation_ids"`
}

type PaymentResult struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

type ShipmentResult struct {
	ShipmentID string `json:"shipment_id"`
}

type OrderProcessingResult struct {
	OrderID     string                     `json:"order_id"`
	Reservation InventoryReservationResult `json:"reservation"`
	Payment     PaymentResult              `json:"payment"`
	Shipment    ShipmentResult             `json:"shipment"`
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
	func(ctx context.Context, orderID string, items []OrderItem) (InventoryReservationResult, error) {
		fmt.Printf("Reserved inventory for order %s\n", orderID)
		reservationIDs := make([]string, len(items))
		for i, item := range items {
			reservationIDs[i] = fmt.Sprintf("RES-%s", item.ID)
		}
		return InventoryReservationResult{ReservationIDs: reservationIDs}, nil
	},
	romancy.WithCompensation(cancelInventoryReservation),
)

var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, orderID string, amount float64, cardToken string) (PaymentResult, error) {
		fmt.Printf("Charged $%.2f for order %s\n", amount, orderID)
		return PaymentResult{
			TransactionID: fmt.Sprintf("TXN-%s", orderID),
			Amount:        amount,
		}, nil
	},
	romancy.WithCompensation(refundPayment),
)

var createShipment = romancy.DefineActivity("create_shipment",
	func(ctx context.Context, orderID string, address ShippingAddress) (ShipmentResult, error) {
		fmt.Printf("Creating shipment for order %s\n", orderID)
		// This might fail if shipping service is unavailable
		if address.Street == "invalid" {
			return ShipmentResult{}, fmt.Errorf("invalid shipping address")
		}
		return ShipmentResult{ShipmentID: fmt.Sprintf("SHIP-%s", orderID)}, nil
	},
)

// Define the saga workflow

var orderProcessingSaga = romancy.DefineWorkflow("order_processing_saga",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderProcessingResult, error) {
		// Reserve inventory
		reservation, err := reserveInventory.Execute(ctx, input.OrderID, input.Items)
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Charge payment
		payment, err := chargePayment.Execute(ctx, input.OrderID, input.Amount, input.Token)
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Create shipment (might fail)
		shipment, err := createShipment.Execute(ctx, input.OrderID, input.Address)
		if err != nil {
			return OrderProcessingResult{}, err
		}

		return OrderProcessingResult{
			OrderID:     input.OrderID,
			Reservation: reservation,
			Payment:     payment,
			Shipment:    shipment,
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
type ChargeResult struct {
	TransactionID string `json:"transaction_id"`
}

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
	func(ctx context.Context, orderID string, amount float64) (ChargeResult, error) {
		transactionID, err := processPayment(orderID, amount)
		if err != nil {
			return ChargeResult{}, err
		}
		return ChargeResult{TransactionID: transactionID}, nil
	},
	romancy.WithCompensation(refundPayment),
)
```

### 2. Store Compensation Data

Activities should return data needed for compensation:

```go
type ReservationData struct {
	ReservationIDs []string    `json:"reservation_ids"`
	Items          []OrderItem `json:"items"`
}

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string, items []OrderItem) (ReservationData, error) {
		reservationIDs := make([]string, 0)
		for _, item := range items {
			resID, err := reserveItem(item.ID, item.Quantity)
			if err != nil {
				return ReservationData{}, err
			}
			reservationIDs = append(reservationIDs, resID)
		}

		// Return data needed for compensation
		return ReservationData{
			ReservationIDs: reservationIDs,
			Items:          items,
		}, nil
	},
	romancy.WithCompensation(cancelReservation),
)
```

### 3. Handle Partial State

Consider partial completion within activities:

```go
type MultiReservationResult struct {
	AllReserved []string `json:"all_reserved"`
}

var reserveMultipleItems = romancy.DefineActivity("reserve_multiple_items",
	func(ctx context.Context, items []OrderItem) (MultiReservationResult, error) {
		reserved := make([]string, 0)

		for _, item := range items {
			resID, err := reserveItem(item.ID, item.Quantity)
			if err != nil {
				// Manually compensate partial reservations
				for _, r := range reserved {
					cancelReservation(r)
				}
				return MultiReservationResult{}, err
			}
			reserved = append(reserved, resID)
		}

		return MultiReservationResult{AllReserved: reserved}, nil
	},
)
```

### 4. Timeout Handling

Set appropriate timeouts for compensation functions:

```go
type LongRunningInput struct {
	TaskID string `json:"task_id"`
	Data   string `json:"data"`
}

type LongRunningResult struct {
	TaskID  string `json:"task_id"`
	Success bool   `json:"success"`
}

var compensateLongRunning = romancy.DefineCompensation("compensate_long_running",
	func(ctx context.Context, input LongRunningInput) error {
		// Create context with timeout
		timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// Perform compensation with timeout
		return performCompensation(timeoutCtx, input)
	},
)

var longRunningActivity = romancy.DefineActivity("long_running_activity",
	func(ctx context.Context, input LongRunningInput) (LongRunningResult, error) {
		result, err := performLongOperation(input)
		if err != nil {
			return LongRunningResult{}, err
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
type ConditionalResult struct {
	ActionID          string `json:"action_id"`
	NeedsCompensation bool   `json:"needs_compensation"`
}

var conditionalCompensation = romancy.DefineCompensation("conditional_compensation",
	func(ctx context.Context, shouldCompensate bool) error {
		if !shouldCompensate {
			return nil // Skip compensation
		}

		return performCleanup()
	},
)

var conditionalActivity = romancy.DefineActivity("conditional_activity",
	func(ctx context.Context, shouldCompensate bool) (ConditionalResult, error) {
		actionID, err := performAction()
		if err != nil {
			return ConditionalResult{}, err
		}
		return ConditionalResult{
			ActionID:          actionID,
			NeedsCompensation: shouldCompensate,
		}, nil
	},
	romancy.WithCompensation(conditionalCompensation),
)
```

### Nested Sagas

Sagas can call other sagas, with compensation cascading through the hierarchy:

```go
type ParentSagaInput struct {
	ChildData  ChildInput  `json:"child_data"`
	ParentData ParentInput `json:"parent_data"`
}

type ParentSagaResult struct {
	Child  ChildResult  `json:"child"`
	Parent ParentResult `json:"parent"`
}

var parentSaga = romancy.DefineWorkflow("parent_saga",
	func(ctx *romancy.WorkflowContext, input ParentSagaInput) (ParentSagaResult, error) {
		// If child saga fails, its compensations run first
		childResult, err := childSaga.Execute(ctx, input.ChildData)
		if err != nil {
			return ParentSagaResult{}, err
		}

		// Then parent activities
		parentResult, err := parentActivity.Execute(ctx, input.ParentData)
		if err != nil {
			return ParentSagaResult{}, err
		}

		return ParentSagaResult{
			Child:  childResult,
			Parent: parentResult,
		}, nil
	},
)
```

### Manual Compensation Trigger

While Romancy handles automatic compensation, you can also manually trigger compensation:

```go
type RiskyInput struct {
	ActionType string `json:"action_type"`
}

type RiskyResult struct {
	Success       bool `json:"success"`
	NeedsRollback bool `json:"needs_rollback"`
}

var manualCompensationSaga = romancy.DefineWorkflow("manual_compensation_saga",
	func(ctx *romancy.WorkflowContext, input RiskyInput) (RiskyResult, error) {
		result, err := riskyActivity.Execute(ctx, input)
		if err != nil {
			return RiskyResult{}, err
		}

		if result.NeedsRollback {
			// Manually trigger compensation by returning error
			return RiskyResult{}, fmt.Errorf("manual rollback triggered")
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
type CriticalInput struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

type CriticalResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
}

var compensateCritical = romancy.DefineCompensation("compensate_critical",
	func(ctx context.Context, input CriticalInput) error {
		log.Printf("Starting compensation for %s\n", input.ID)

		err := performCompensation(input)
		if err != nil {
			log.Printf("Compensation failed: %v\n", err)
			return err
		}

		log.Printf("Compensation successful for %s\n", input.ID)
		return nil
	},
)

var criticalActivity = romancy.DefineActivity("critical_activity",
	func(ctx context.Context, input CriticalInput) (CriticalResult, error) {
		result, err := performCriticalOperation(input)
		if err != nil {
			return CriticalResult{}, err
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
