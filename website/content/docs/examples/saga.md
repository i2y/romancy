---
title: "Saga Pattern"
weight: 3
---

This example demonstrates automatic compensation (rollback) when a workflow fails.

## What This Example Shows

- ✅ `DefineCompensation` for compensation functions
- ✅ `WithCompensation` to link compensation to activities
- ✅ Automatic reverse-order compensation
- ✅ Saga pattern for distributed transactions
- ✅ Rollback on workflow failure

## The Problem

In distributed systems, you can't use traditional database transactions. The **Saga pattern** solves this with **compensation functions** that undo completed steps.

## Code Overview

### Define Compensation Functions

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
)

// Compensation: Release reserved inventory
var cancelInventoryReservation = romancy.DefineCompensation("cancel_inventory_reservation",
	func(ctx context.Context, orderID, itemID string) error {
		fmt.Printf("❌ Cancelled reservation for %s\n", itemID)
		return nil
	},
)

// Compensation: Refund payment
var refundPayment = romancy.DefineCompensation("refund_payment",
	func(ctx context.Context, orderID string, amount float64) error {
		fmt.Printf("❌ Refunded $%.2f\n", amount)
		return nil
	},
)
```

### Define Activities with Compensation Links

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/i2y/romancy"
)

// Result types
type ReservationResult struct {
	ReservationID string `json:"reservation_id"`
	ItemID        string `json:"item_id"`
}

type PaymentResult struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

type ShipmentResult struct {
	ShipmentID string `json:"shipment_id"`
}

// Reserve inventory (linked to cancelInventoryReservation)
var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID, itemID string) (ReservationResult, error) {
		fmt.Printf("✅ Reserved %s for order %s\n", itemID, orderID)
		return ReservationResult{
			ReservationID: fmt.Sprintf("RES-%s", itemID),
			ItemID:        itemID,
		}, nil
	},
	romancy.WithCompensation(cancelInventoryReservation), // Link compensation
)

// Charge payment (linked to refundPayment)
var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, orderID string, amount float64) (PaymentResult, error) {
		fmt.Printf("✅ Charged $%.2f for order %s\n", amount, orderID)
		return PaymentResult{
			TransactionID: fmt.Sprintf("TXN-%s", orderID),
			Amount:        amount,
		}, nil
	},
	romancy.WithCompensation(refundPayment), // Link compensation
)

// Ship order (no compensation - this will fail)
var shipOrder = romancy.DefineActivity("ship_order",
	func(ctx context.Context, orderID string) (ShipmentResult, error) {
		fmt.Printf("🚚 Attempting to ship order %s\n", orderID)
		return ShipmentResult{}, errors.New("shipping service unavailable")
	},
)
```

### Define Saga Workflow

```go
package main

import (
	"github.com/i2y/romancy"
)

// Result type for the saga workflow
type OrderSagaResult struct {
	Status string `json:"status"`
}

// orderSaga processes an order with automatic compensation on failure.
// If any step fails, Romancy automatically calls compensation functions
// for all completed steps in reverse order.
var orderSaga = romancy.DefineWorkflow("order_saga",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderSagaResult, error) {
		// Step 1: Reserve inventory
		_, err := reserveInventory.Execute(ctx, orderID, "ITEM-123")
		if err != nil {
			return OrderSagaResult{}, err
		}

		// Step 2: Charge payment
		_, err = chargePayment.Execute(ctx, orderID, 99.99)
		if err != nil {
			return OrderSagaResult{}, err
		}

		// Step 3: Ship order (will fail!)
		_, err = shipOrder.Execute(ctx, orderID)
		if err != nil {
			return OrderSagaResult{}, err
		}

		return OrderSagaResult{Status: "completed"}, nil
	},
)
```

## Expected Output

```
✅ Reserved ITEM-123 for order ORD-001
✅ Charged $99.99 for order ORD-001
🚚 Attempting to ship order ORD-001
💥 Error: shipping service unavailable

Automatic compensation (reverse order):
❌ Refunded $99.99
❌ Cancelled reservation for ITEM-123

Workflow failed with compensation completed.
```

## How It Works

1. **Step 1 completes**: Inventory reserved ✅
2. **Step 2 completes**: Payment charged ✅
3. **Step 3 fails**: Shipping fails ❌
4. **Automatic compensation** (reverse order):
   - First: Refund payment (Step 2 compensation)
   - Then: Cancel reservation (Step 1 compensation)

## Key Rules

### 1. Reverse Order Execution

Compensation functions run in **reverse order** of activity execution:

```
Activities:      reserve → charge → ship (fails)
Compensations:   cancel ← refund
```

### 2. Only Completed Activities

Only **successfully completed** activities are compensated:

```go
reserveInventory.Execute(ctx, ...)  // ✅ Completed → Will be compensated
chargePayment.Execute(ctx, ...)     // ✅ Completed → Will be compensated
shipOrder.Execute(ctx, ...)         // ❌ Failed → No compensation needed
```

### 3. Automatic Trigger

No manual compensation trigger required - Romancy handles it automatically on workflow failure.

## Real-World Use Cases

- **E-commerce**: Reserve inventory → Charge payment → Ship order
- **Hotel Booking**: Reserve room → Charge deposit → Send confirmation
- **Travel**: Book flight → Book hotel → Rent car
- **Financial**: Transfer funds → Update ledger → Send receipt

## Running the Example

Create a file named `saga_example.go` with the complete code (see below), then run:

```bash
# Initialize Go module
go mod init saga-example
go get github.com/i2y/romancy

# Run your workflow
go run saga_example.go
```

## Complete Code

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

// Result types
type ReservationResult struct {
	ReservationID string `json:"reservation_id"`
	ItemID        string `json:"item_id"`
}

type PaymentResult struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

type ShipmentResult struct {
	ShipmentID string `json:"shipment_id"`
}

type OrderSagaResult struct {
	Status string `json:"status"`
}

// Compensation functions

var cancelInventoryReservation = romancy.DefineCompensation("cancel_inventory_reservation",
	func(ctx context.Context, orderID, itemID string) error {
		fmt.Printf("❌ Cancelled reservation for %s\n", itemID)
		return nil
	},
)

var refundPayment = romancy.DefineCompensation("refund_payment",
	func(ctx context.Context, orderID string, amount float64) error {
		fmt.Printf("❌ Refunded $%.2f\n", amount)
		return nil
	},
)

// Activities with compensation links

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID, itemID string) (ReservationResult, error) {
		fmt.Printf("✅ Reserved %s for order %s\n", itemID, orderID)
		return ReservationResult{
			ReservationID: fmt.Sprintf("RES-%s", itemID),
			ItemID:        itemID,
		}, nil
	},
	romancy.WithCompensation(cancelInventoryReservation),
)

var chargePayment = romancy.DefineActivity("charge_payment",
	func(ctx context.Context, orderID string, amount float64) (PaymentResult, error) {
		fmt.Printf("✅ Charged $%.2f for order %s\n", amount, orderID)
		return PaymentResult{
			TransactionID: fmt.Sprintf("TXN-%s", orderID),
			Amount:        amount,
		}, nil
	},
	romancy.WithCompensation(refundPayment),
)

var shipOrder = romancy.DefineActivity("ship_order",
	func(ctx context.Context, orderID string) (ShipmentResult, error) {
		fmt.Printf("🚚 Attempting to ship order %s\n", orderID)
		return ShipmentResult{}, errors.New("shipping service unavailable")
	},
)

// Saga workflow

var orderSaga = romancy.DefineWorkflow("order_saga",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderSagaResult, error) {
		// Step 1: Reserve inventory
		_, err := reserveInventory.Execute(ctx, orderID, "ITEM-123")
		if err != nil {
			return OrderSagaResult{}, err
		}

		// Step 2: Charge payment
		_, err = chargePayment.Execute(ctx, orderID, 99.99)
		if err != nil {
			return OrderSagaResult{}, err
		}

		// Step 3: Ship order (will fail!)
		_, err = shipOrder.Execute(ctx, orderID)
		if err != nil {
			return OrderSagaResult{}, err
		}

		return OrderSagaResult{Status: "completed"}, nil
	},
)

func main() {
	fmt.Println("============================================================")
	fmt.Println("Romancy Framework - Saga Pattern Example")
	fmt.Println("============================================================")
	fmt.Println()

	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("saga_demo.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	// Start the app
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	fmt.Println(">>> Starting saga workflow...")
	fmt.Println()

	// Start the saga workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, orderSaga, "ORD-001")
	if err != nil {
		fmt.Printf("\n>>> Workflow failed (compensation was triggered): %v\n", err)
	} else {
		fmt.Printf("\n>>> Workflow completed with instance ID: %s\n", instanceID)
	}
}
```

## What You Learned

- ✅ **`DefineCompensation`**: Define compensation functions
- ✅ **`WithCompensation`**: Link compensation to activities
- ✅ **Automatic Execution**: Romancy handles compensation automatically
- ✅ **Reverse Order**: Compensations run in reverse order
- ✅ **Saga Pattern**: Distributed transaction management without 2PC

## Next Steps

- **[Event Waiting](/docs/examples/events)**: Wait for external events
- **[Core Concepts](/docs/getting-started/concepts)**: Deep dive into Saga pattern
