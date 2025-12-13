---
title: "Event Waiting"
weight: 4
---

This example demonstrates how workflows can wait for external events without blocking worker processes.

## What This Example Shows

- ✅ `WaitEvent()` for waiting for external events
- ✅ `Sleep()` for time-based waiting
- ✅ Process-releasing behavior (workflow pauses, worker is freed)
- ✅ Event-driven workflow continuation

## The Problem

Traditional approaches keep goroutines running while waiting:

```go
// ❌ Bad: Keeps goroutine running for 1 hour
time.Sleep(time.Hour)  // Workflow state held in memory unnecessarily
```

Romancy's `WaitEvent()` and `Sleep()` **persist the workflow state to the database** and release the goroutine. The worker can then handle other workflows.

## Code Overview

### Wait for External Event

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/i2y/romancy"
)

// Result types
type PaymentStartResult struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
}

type PaymentWorkflowResult struct {
	Status        string `json:"status"`
	TransactionID string `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

var startPaymentProcessing = romancy.DefineActivity("start_payment_processing",
	func(ctx context.Context, orderID string) (PaymentStartResult, error) {
		fmt.Printf("🔄 Starting payment for order %s\n", orderID)
		// Call external payment service API...
		return PaymentStartResult{
			PaymentID: fmt.Sprintf("PAY-%s", orderID),
			Status:    "pending",
		}, nil
	},
)

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (PaymentWorkflowResult, error) {
		// Step 1: Start payment processing
		payment, err := startPaymentProcessing.Execute(ctx, orderID)
		if err != nil {
			return PaymentWorkflowResult{}, err
		}
		fmt.Printf("Payment started: %s\n", payment.PaymentID)

		// Step 2: Wait for payment completion event
		// Workflow pauses here, worker process is released
		fmt.Println("⏸️  Waiting for payment.completed event...")
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute), // 5-minute timeout
		)
		if err != nil {
			return PaymentWorkflowResult{}, err
		}

		// Step 3: Parse and process payment result
		var paymentResult PaymentCompleted
		if err := romancy.DecodeEventData(event.Data, &paymentResult); err != nil {
			return PaymentWorkflowResult{}, fmt.Errorf("invalid payment event: %w", err)
		}

		fmt.Printf("✅ Payment completed: %v\n", event.Data)
		return PaymentWorkflowResult{
			Status:        "completed",
			TransactionID: paymentResult.TransactionID,
			Amount:        paymentResult.Amount,
		}, nil
	},
)
```

### Wait for Timer

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/i2y/romancy"
)

// Result types
type OrderResult struct {
	OrderID string `json:"order_id"`
}

type PaymentStatusResult struct {
	Paid bool `json:"paid"`
}

type CancelResult struct {
	Cancelled bool `json:"cancelled"`
}

type OrderTimeoutResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID string) (OrderResult, error) {
		fmt.Printf("📦 Creating order %s\n", orderID)
		return OrderResult{OrderID: orderID}, nil
	},
)

var checkPaymentStatus = romancy.DefineActivity("check_payment_status",
	func(ctx context.Context, orderID string) (PaymentStatusResult, error) {
		// Check payment status from external service
		return PaymentStatusResult{Paid: false}, nil
	},
)

var cancelOrder = romancy.DefineActivity("cancel_order",
	func(ctx context.Context, orderID string) (CancelResult, error) {
		fmt.Printf("🚫 Cancelling order %s\n", orderID)
		return CancelResult{Cancelled: true}, nil
	},
)

var orderWithTimeout = romancy.DefineWorkflow("order_with_timeout",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderTimeoutResult, error) {
		// Step 1: Create order
		_, err := createOrder.Execute(ctx, orderID)
		if err != nil {
			return OrderTimeoutResult{}, err
		}
		fmt.Printf("Order %s created\n", orderID)

		// Step 2: Sleep 60 seconds before checking payment
		fmt.Println("⏱️  Sleeping 60 seconds before checking payment...")
		err = romancy.Sleep(ctx, 60*time.Second)
		if err != nil {
			return OrderTimeoutResult{}, err
		}

		// Step 3: Check payment status
		status, err := checkPaymentStatus.Execute(ctx, orderID)
		if err != nil {
			return OrderTimeoutResult{}, err
		}

		if status.Paid {
			fmt.Println("✅ Payment received!")
			return OrderTimeoutResult{Status: "completed"}, nil
		}

		// Step 4: Cancel order due to timeout
		fmt.Println("❌ Payment timeout - cancelling order")
		_, err = cancelOrder.Execute(ctx, orderID)
		if err != nil {
			return OrderTimeoutResult{}, err
		}

		return OrderTimeoutResult{
			Status: "cancelled",
			Reason: "payment_timeout",
		}, nil
	},
)
```

## How It Works

### Event Waiting Flow

```
1. Workflow executes: startPaymentProcessing.Execute()
2. Workflow hits: WaitEvent()
3. Workflow pauses (status="waiting_for_event")
4. Worker process is RELEASED (can handle other workflows)
5. External event arrives (e.g., CloudEvent)
6. Workflow RESUMES from WaitEvent()
7. Workflow continues: process payment result
```

### ReceivedEvent Structure

```go
event, err := romancy.WaitEvent(ctx, "payment.completed")
if err != nil {
	return nil, err
}

// event is a *romancy.ReceivedEvent
fmt.Println(event.Type)    // "payment.completed"
fmt.Println(event.Source)  // "payment-service"
fmt.Println(event.Data)    // map[string]any{"transaction_id": "...", "amount": 99.99}
```

### Type-Safe Events with Structs

Define Go structs for type-safe event data access:

```go
package main

import (
	"fmt"
	"time"

	"github.com/i2y/romancy"
)

// PaymentCompleted represents a payment completion event
type PaymentCompleted struct {
	OrderID       string  `json:"order_id"`
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
	Status        string  `json:"status"`
}

type PaymentTypedResult struct {
	Status string  `json:"status"`
	Amount float64 `json:"amount"`
}

var paymentWorkflowTyped = romancy.DefineWorkflow("payment_workflow_typed",
	func(ctx *romancy.WorkflowContext, orderID string) (PaymentTypedResult, error) {
		// Wait for event
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return PaymentTypedResult{}, err
		}

		// Parse event data into struct
		var payment PaymentCompleted
		if err := romancy.DecodeEventData(event.Data, &payment); err != nil {
			return PaymentTypedResult{}, fmt.Errorf("invalid payment event: %w", err)
		}

		// Type-safe access
		amount := payment.Amount              // float64
		transactionID := payment.TransactionID // string
		orderID = payment.OrderID              // string

		fmt.Printf("✅ Payment of $%.2f completed for order %s\n", amount, orderID)
		fmt.Printf("   Transaction ID: %s\n", transactionID)

		return PaymentTypedResult{
			Status: "completed",
			Amount: amount,
		}, nil
	},
)
```

**Benefits of struct-based event handling:**

- ✅ **Type safety**: Compile-time type checking
- ✅ **Runtime validation**: Automatic data validation when event arrives
- ✅ **Clear contracts**: Explicit event structure definition
- ✅ **Error detection**: Invalid events fail fast with clear error messages

**Without struct (map access):**
```go
event, _ := romancy.WaitEvent(ctx, "payment.completed")
amount := event.Data["amount"]  // interface{}, needs type assertion
```

**With struct:**
```go
var payment PaymentCompleted
romancy.DecodeEventData(event.Data, &payment)
amount := payment.Amount  // float64, type-safe
```

## Benefits

### 1. Resource Efficiency

```go
type EmptyResult struct{}

// ❌ Bad: Keeps goroutine running for 1 hour
var badWorkflow = romancy.DefineWorkflow("bad_workflow",
	func(ctx *romancy.WorkflowContext, input string) (EmptyResult, error) {
		time.Sleep(time.Hour) // Goroutine held in memory!
		return EmptyResult{}, nil
	},
)

// ✅ Good: Persists state and releases goroutine
var goodWorkflow = romancy.DefineWorkflow("good_workflow",
	func(ctx *romancy.WorkflowContext, input string) (EmptyResult, error) {
		romancy.Sleep(ctx, time.Hour) // Memory freed!
		return EmptyResult{}, nil
	},
)
```

**Impact:**

- Bad: 1 worker holds 1 workflow state in memory (wasted memory)
- Good: 1 worker can handle 1000s of workflows (state persisted to DB)

### 2. Long-Running Workflows

Perfect for workflows that span hours or days:

```go
type LoanApprovalResult struct {
	Status string `json:"status"`
}

var loanApprovalWorkflow = romancy.DefineWorkflow("loan_approval_workflow",
	func(ctx *romancy.WorkflowContext, applicationID string) (LoanApprovalResult, error) {
		// Submit for manual review
		_, err := submitForReview.Execute(ctx, applicationID)
		if err != nil {
			return LoanApprovalResult{}, err
		}

		// Wait up to 48 hours for approval
		event, err := romancy.WaitEvent(ctx, "loan.approved",
			romancy.WithTimeout(48*time.Hour),
		)
		if err != nil {
			return LoanApprovalResult{}, err
		}

		// Process approval
		_, err = processApproval.Execute(ctx, event.Data)
		if err != nil {
			return LoanApprovalResult{}, err
		}

		return LoanApprovalResult{Status: "approved"}, nil
	},
)
```

### 3. Event-Driven Architecture

Integrate with event-driven systems:

```go
type FulfillmentResult struct {
	Status string `json:"status"`
}

var orderFulfillment = romancy.DefineWorkflow("order_fulfillment",
	func(ctx *romancy.WorkflowContext, orderID string) (FulfillmentResult, error) {
		// Wait for warehouse to pack the order
		_, err := romancy.WaitEvent(ctx, "order.packed")
		if err != nil {
			return FulfillmentResult{}, err
		}

		// Wait for carrier to pick up
		_, err = romancy.WaitEvent(ctx, "order.picked_up")
		if err != nil {
			return FulfillmentResult{}, err
		}

		// Wait for delivery confirmation
		_, err = romancy.WaitEvent(ctx, "order.delivered")
		if err != nil {
			return FulfillmentResult{}, err
		}

		return FulfillmentResult{Status: "delivered"}, nil
	},
)
```

## Sending Events

To resume a waiting workflow, send a CloudEvent:

```bash
# Using curl
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123",
    "datacontenttype": "application/json",
    "data": {
      "order_id": "ORD-123",
      "transaction_id": "TXN-456",
      "amount": 99.99,
      "status": "success"
    }
  }'
```

**Expected Response:**

```http
HTTP/1.1 202 Accepted
Content-Type: application/json

{
  "status": "accepted"
}
```

**Status Codes:**

- **202 Accepted**: Event accepted for processing ✅
- **400 Bad Request**: Invalid CloudEvent format (non-retryable) ❌
- **500 Internal Server Error**: Server error (retryable) ⚠️

See [CloudEvents HTTP Binding](/docs/core-features/events/cloudevents-http-binding) for detailed error handling and retry logic.

Or programmatically using the CloudEvents Go SDK:

```go
package main

import (
	"context"
	"log"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

func main() {
	// Create CloudEvents client
	c, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget("http://localhost:8001/"),
	)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// Create event
	event := cloudevents.NewEvent()
	event.SetType("payment.completed")
	event.SetSource("payment-service")
	event.SetID("event-123")
	event.SetData(cloudevents.ApplicationJSON, map[string]any{
		"order_id":       "ORD-123",
		"transaction_id": "TXN-456",
		"amount":         99.99,
		"status":         "success",
	})

	// Send event
	ctx := context.Background()
	result := c.Send(ctx, event)
	if cloudevents.IsACK(result) {
		log.Println("✅ Event accepted")
	} else {
		log.Printf("❌ Failed to send: %v", result)
	}
}
```

## Running the Example

Create a file named `event_waiting_workflow.go` with the code shown above, then run:

```bash
# Initialize Go module
go mod init event-example
go get github.com/i2y/romancy

# Run your workflow
go run event_waiting_workflow.go
```

## Complete Code

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/i2y/romancy"
)

// PaymentCompleted represents a payment completion event
type PaymentCompleted struct {
	OrderID       string  `json:"order_id"`
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
	Status        string  `json:"status"`
}

// Result types
type PaymentStartResult struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
}

type PaymentWorkflowResult struct {
	Status        string  `json:"status"`
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

// Activities

var startPaymentProcessing = romancy.DefineActivity("start_payment_processing",
	func(ctx context.Context, orderID string) (PaymentStartResult, error) {
		fmt.Printf("🔄 Starting payment for order %s\n", orderID)
		return PaymentStartResult{
			PaymentID: fmt.Sprintf("PAY-%s", orderID),
			Status:    "pending",
		}, nil
	},
)

// Workflow

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (PaymentWorkflowResult, error) {
		// Step 1: Start payment processing
		payment, err := startPaymentProcessing.Execute(ctx, orderID)
		if err != nil {
			return PaymentWorkflowResult{}, err
		}
		fmt.Printf("Payment started: %s\n", payment.PaymentID)

		// Step 2: Wait for payment completion event
		fmt.Println("⏸️  Waiting for payment.completed event...")
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return PaymentWorkflowResult{}, err
		}

		// Step 3: Parse and process payment result
		var paymentResult PaymentCompleted
		if err := romancy.DecodeEventData(event.Data, &paymentResult); err != nil {
			return PaymentWorkflowResult{}, fmt.Errorf("invalid payment event: %w", err)
		}

		fmt.Printf("✅ Payment completed: $%.2f for order %s\n",
			paymentResult.Amount, paymentResult.OrderID)

		return PaymentWorkflowResult{
			Status:        "completed",
			TransactionID: paymentResult.TransactionID,
			Amount:        paymentResult.Amount,
		}, nil
	},
)

func main() {
	fmt.Println("============================================================")
	fmt.Println("Romancy Framework - Event Waiting Example")
	fmt.Println("============================================================")
	fmt.Println()

	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("event_demo.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	// Initialize the app
	if err := app.Initialize(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	fmt.Println(">>> Starting payment workflow...")
	fmt.Println()

	// Start the workflow
	instanceID, err := paymentWorkflow.Start(ctx, app, "ORD-123")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\n>>> Workflow started with instance ID: %s\n", instanceID)
	fmt.Println(">>> Send a CloudEvent to resume the workflow:")
	fmt.Println()
	fmt.Println(`curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123",
    "data": {"order_id": "ORD-123", "transaction_id": "TXN-456", "amount": 99.99}
  }'`)
}
```

## What You Learned

- ✅ **`WaitEvent()`**: Wait for external events
- ✅ **`Sleep()`**: Sleep for specific duration
- ✅ **Process Releasing**: Workers are freed during wait
- ✅ **ReceivedEvent**: Typed event data access
- ✅ **CloudEvents**: Standard event format support

## Next Steps

- **[CloudEvents HTTP Binding](/docs/core-features/events/cloudevents-http-binding)**: Deep dive into CloudEvents integration
- **[Core Concepts](/docs/getting-started/concepts)**: Learn about workflows, activities, and events
- **[Transactional Outbox](/docs/core-features/transactional-outbox)**: Reliable event publishing
