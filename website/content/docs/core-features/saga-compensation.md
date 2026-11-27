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

### WithCompensation

In Romancy, a compensation is a function that takes the activity's input. Link it to the activity with `WithCompensation`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
)

type ReserveInventoryInput struct {
	OrderID string `json:"order_id"`
	ItemID  string `json:"item_id"`
}

type ReservationResult struct {
	ReservationID string `json:"reservation_id"`
}

// Compensation is a function taking the activity's input type
func cancelInventoryReservation(ctx context.Context, input ReserveInventoryInput) error {
	// Cancel reservation logic
	fmt.Printf("Cancelled inventory reservation for %s\n", input.OrderID)
	return nil
}

// Define activity with compensation link
var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, input ReserveInventoryInput) (ReservationResult, error) {
		// Reserve inventory logic
		fmt.Printf("Reserved inventory for %s\n", input.OrderID)
		return ReservationResult{ReservationID: fmt.Sprintf("RES-%s", input.ItemID)}, nil
	},
	romancy.WithCompensation[ReserveInventoryInput, ReservationResult](cancelInventoryReservation), // Link compensation
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
		// Step 1
		if _, err := reserveInventory.Execute(ctx, ReserveInventoryInput{OrderID: orderID, ItemID: "ITEM-123"}); err != nil {
			return OrderSagaResult{}, err
		}
		// Step 2
		if _, err := chargePayment.Execute(ctx, ChargePaymentInput{OrderID: orderID, Amount: 99.99}); err != nil {
			return OrderSagaResult{}, err
		}
		// Step 3 (fails)
		if _, err := shipOrder.Execute(ctx, orderID); err != nil {
			// Returning the error fails the workflow, and compensations run as:
			// 1. Compensation for Step 2 (refund payment)
			// 2. Compensation for Step 1 (cancel reservation)
			// Step 3 has no compensation as it failed
			return OrderSagaResult{}, err
		}

		return OrderSagaResult{Status: "completed"}, nil
	},
)
```

### Partial Completion Handling

Only **successfully completed** activities are compensated:

- ✅ Completed activities → Compensation executed
- ❌ Failed activities → No compensation needed
- ⏭️ Not-yet-executed activities → No compensation needed

### When Compensations Do Not Run

- **The run was interrupted, not failed.** A run that loses its lock, whose worker shuts down, or that hits a database error inside a kernel primitive (`romancy.ErrRunAborted`) ends without failing the workflow. Nothing is compensated; a later run replays the workflow and carries on.
- **A recur ended the iteration.** `Recur` drops the compensations registered so far: each iteration is a finished unit, and a failure in a later iteration does not undo an earlier one.

### Cancelling During Compensation

`CancelWorkflow` on an instance that is already compensating returns `ErrWorkflowNotCancellable` and leaves it alone, so its compensations are never stopped half-way.

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

// Activity input types

type ReserveInventoryInput struct {
	OrderID string      `json:"order_id"`
	Items   []OrderItem `json:"items"`
}

type ChargePaymentInput struct {
	OrderID   string  `json:"order_id"`
	Amount    float64 `json:"amount"`
	CardToken string  `json:"card_token"`
}

type CreateShipmentInput struct {
	OrderID string          `json:"order_id"`
	Address ShippingAddress `json:"address"`
}

// Compensation functions take the activity's input type

func cancelInventoryReservation(ctx context.Context, input ReserveInventoryInput) error {
	fmt.Printf("Cancelled inventory reservations for order %s\n", input.OrderID)
	return nil
}

func refundPayment(ctx context.Context, input ChargePaymentInput) error {
	fmt.Printf("Refunded $%.2f for order %s\n", input.Amount, input.OrderID)
	return nil
}

// Define activities with compensation links

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, input ReserveInventoryInput) (InventoryReservationResult, error) {
		fmt.Printf("Reserved inventory for order %s\n", input.OrderID)
		reservationIDs := make([]string, len(input.Items))
		for i, item := range input.Items {
			reservationIDs[i] = fmt.Sprintf("RES-%s", item.ID)
		}
		return InventoryReservationResult{ReservationIDs: reservationIDs}, nil
	},
	romancy.WithCompensation[ReserveInventoryInput, InventoryReservationResult](cancelInventoryReservation),
)

var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, input ChargePaymentInput) (PaymentResult, error) {
		fmt.Printf("Charged $%.2f for order %s\n", input.Amount, input.OrderID)
		return PaymentResult{
			TransactionID: fmt.Sprintf("TXN-%s", input.OrderID),
			Amount:        input.Amount,
		}, nil
	},
	romancy.WithCompensation[ChargePaymentInput, PaymentResult](refundPayment),
)

var createShipment = romancy.DefineActivity("create_shipment",
	func(ctx context.Context, input CreateShipmentInput) (ShipmentResult, error) {
		fmt.Printf("Creating shipment for order %s\n", input.OrderID)
		// This might fail if shipping service is unavailable
		if input.Address.Street == "invalid" {
			return ShipmentResult{}, fmt.Errorf("invalid shipping address")
		}
		return ShipmentResult{ShipmentID: fmt.Sprintf("SHIP-%s", input.OrderID)}, nil
	},
)

// Define the saga workflow

var orderProcessingSaga = romancy.DefineWorkflow("order_processing_saga",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderProcessingResult, error) {
		// Reserve inventory
		reservation, err := reserveInventory.Execute(ctx, ReserveInventoryInput{OrderID: input.OrderID, Items: input.Items})
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Charge payment
		payment, err := chargePayment.Execute(ctx, ChargePaymentInput{OrderID: input.OrderID, Amount: input.Amount, CardToken: input.Token})
		if err != nil {
			return OrderProcessingResult{}, err
		}

		// Create shipment (might fail)
		shipment, err := createShipment.Execute(ctx, CreateShipmentInput{OrderID: input.OrderID, Address: input.Address})
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

	romancy.RegisterWorkflow(app, orderProcessingSaga)

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

type ChargeInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

func refundPayment(ctx context.Context, input ChargeInput) error {
	// Check if already refunded
	if isAlreadyRefunded(input.OrderID) {
		return nil // Already refunded, skip
	}

	// Perform refund
	return processRefund(input.OrderID, input.Amount)
}

var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, input ChargeInput) (ChargeResult, error) {
		transactionID, err := processPayment(input.OrderID, input.Amount)
		if err != nil {
			return ChargeResult{}, err
		}
		return ChargeResult{TransactionID: transactionID}, nil
	},
	romancy.WithCompensation[ChargeInput, ChargeResult](refundPayment),
)
```

### 2. Give the Compensation What It Needs

A compensation receives the activity's **input**, not its result. Whatever the undo needs must be in the input, or be found from it. Record what the activity creates under a key from its input (here the order ID), so the compensation can find it again:

```go
type ReservationInput struct {
	OrderID string      `json:"order_id"`
	Items   []OrderItem `json:"items"`
}

type ReservationData struct {
	ReservationIDs []string `json:"reservation_ids"`
}

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, input ReservationInput) (ReservationData, error) {
		ids := make([]string, 0, len(input.Items))
		for _, item := range input.Items {
			// The reservation is recorded under the order ID
			resID, err := reserveItemForOrder(input.OrderID, item.ID, item.Quantity)
			if err != nil {
				return ReservationData{}, err
			}
			ids = append(ids, resID)
		}
		return ReservationData{ReservationIDs: ids}, nil
	},
	romancy.WithCompensation[ReservationInput, ReservationData](
		func(ctx context.Context, input ReservationInput) error {
			// The result is not passed here: find the reservations from the input
			return releaseOrderReservations(input.OrderID)
		},
	),
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

func compensateLongRunning(ctx context.Context, input LongRunningInput) error {
	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Perform compensation with timeout
	return performCompensation(timeoutCtx, input)
}

var longRunningActivity = romancy.DefineActivity("long_running_activity",
	func(ctx context.Context, input LongRunningInput) (LongRunningResult, error) {
		result, err := performLongOperation(input)
		if err != nil {
			return LongRunningResult{}, err
		}
		return result, nil
	},
	romancy.WithCompensation[LongRunningInput, LongRunningResult](compensateLongRunning),
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

func conditionalCompensation(ctx context.Context, shouldCompensate bool) error {
	if !shouldCompensate {
		return nil // Skip compensation
	}

	return performCleanup()
}

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
	romancy.WithCompensation[bool, ConditionalResult](conditionalCompensation),
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

func compensateCritical(ctx context.Context, input CriticalInput) error {
	log.Printf("Starting compensation for %s\n", input.ID)

	err := performCompensation(input)
	if err != nil {
		log.Printf("Compensation failed: %v\n", err)
		return err
	}

	log.Printf("Compensation successful for %s\n", input.ID)
	return nil
}

var criticalActivity = romancy.DefineActivity("critical_activity",
	func(ctx context.Context, input CriticalInput) (CriticalResult, error) {
		result, err := performCriticalOperation(input)
		if err != nil {
			return CriticalResult{}, err
		}
		return result, nil
	},
	romancy.WithCompensation[CriticalInput, CriticalResult](compensateCritical),
)
```

## Related Topics

- [Workflows and Activities](/docs/core-features/workflows-activities) - Learn about basic workflow concepts
- [Durable Execution](/docs/core-features/durable-execution/replay) - Understand replay and recovery
- [Transactional Outbox](/docs/core-features/transactional-outbox) - Ensure message delivery consistency
- [Examples: Saga Pattern](/docs/examples/saga) - See practical examples
