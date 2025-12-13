---
title: "Event Waiting & Timers"
weight: 1
---

Romancy provides powerful event waiting capabilities that allow workflows to pause and wait for external events or timers without blocking worker processes. This enables efficient resource utilization and event-driven workflow orchestration.

## Overview

Event waiting in Romancy allows workflows to:

- **Pause execution** and wait for external events
- **Release worker processes** while waiting (non-blocking)
- **Resume automatically** when events arrive
- **Handle timeouts** gracefully
- **Wait for specific durations** with timers

## Key Functions

### `WaitEvent()`

Wait for an external event with optional timeout:

```go
package main

import (
	"context"
	"time"

	"github.com/i2y/romancy"
)

// PaymentResult represents the result of payment processing
type PaymentResult struct {
	PaymentID string  `json:"payment_id"`
	Amount    float64 `json:"amount"`
}

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, input string) (PaymentResult, error) {
		// Wait for an event (with 5-minute timeout)
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return PaymentResult{}, err
		}

		// Access event data
		paymentID := event.Data["payment_id"].(string)
		amount := event.Data["amount"].(float64)

		return PaymentResult{
			PaymentID: paymentID,
			Amount:    amount,
		}, nil
	},
)
```

### `Sleep()`

Sleep for a specific duration:

```go
package main

import (
	"context"
	"time"

	"github.com/i2y/romancy"
)

type SleepResult struct {
	Status string `json:"status"`
}

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, input string) (SleepResult, error) {
		// Sleep for 60 seconds
		err := romancy.Sleep(ctx, 60*time.Second)
		if err != nil {
			return SleepResult{}, err
		}

		// Continue execution after timer expires
		return processTimeout(ctx)
	},
)
```

## Detailed API Reference

### WaitEvent()

```go
func WaitEvent(
	ctx *WorkflowContext,
	eventType string,
	opts ...WaitEventOption,
) (*ReceivedEvent, error)
```

#### Parameters

- `ctx`: The workflow context
- `eventType`: CloudEvents type to wait for (e.g., "payment.completed")
- `opts`: Optional configuration (timeout, etc.)

#### Options

- `WithTimeout(d time.Duration)`: Set timeout duration
- `WithEventID(id string)`: Set custom event wait ID (auto-generated if not provided)

#### Returns

`*ReceivedEvent` containing:

- `Data`: Event data (`map[string]any`)
- `Type`: CloudEvents type
- `Source`: Event source
- `Subject`: Optional event subject
- `Time`: Event timestamp
- `ID`: Event ID

#### Error Handling

```go
event, err := romancy.WaitEvent(ctx, "payment.completed",
	romancy.WithTimeout(5*time.Minute),
)
if err != nil {
	if errors.Is(err, romancy.ErrTimeout) {
		// Handle timeout
		return nil, fmt.Errorf("payment timed out")
	}
	return nil, err
}
```

#### Example with Type-Safe Struct

```go
package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/i2y/romancy"
)

// PaymentCompleted represents payment completion data
type PaymentCompleted struct {
	PaymentID string  `json:"payment_id"`
	Amount    float64 `json:"amount"`
	Status    string  `json:"status"`
}

// PaymentWorkflowResult is the result of the payment workflow
type PaymentWorkflowResult struct {
	OrderID   string `json:"order_id"`
	PaymentID string `json:"payment_id"`
}

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (PaymentWorkflowResult, error) {
		// Wait for event
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return PaymentWorkflowResult{}, err
		}

		// Parse event data into struct
		var payment PaymentCompleted
		if err := parseEventData(event.Data, &payment); err != nil {
			return PaymentWorkflowResult{}, err
		}

		if payment.Status == "success" {
			return PaymentWorkflowResult{
				OrderID:   orderID,
				PaymentID: payment.PaymentID,
			}, nil
		}

		return PaymentWorkflowResult{}, fmt.Errorf("payment failed: %s", payment.Status)
	},
)

// parseEventData converts map to struct
func parseEventData(data map[string]any, target any) error {
	bytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}
```

### Sleep()

```go
func Sleep(
	ctx *WorkflowContext,
	duration time.Duration,
	opts ...SleepOption,
) error
```

#### Parameters

- `ctx`: The workflow context
- `duration`: Duration to sleep
- `opts`: Optional configuration

#### Options

- `WithSleepID(id string)`: Set custom timer ID (auto-generated if not provided)

#### Example

```go
package main

import (
	"context"
	"time"

	"github.com/i2y/romancy"
)

type ScheduledTaskResult struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

var scheduledTask = romancy.DefineWorkflow("scheduled_task",
	func(ctx *romancy.WorkflowContext, taskID string) (ScheduledTaskResult, error) {
		// Execute immediately
		if _, err := performInitialTask.Execute(ctx, taskID); err != nil {
			return ScheduledTaskResult{}, err
		}

		// Sleep for 1 hour
		if err := romancy.Sleep(ctx, 1*time.Hour); err != nil {
			return ScheduledTaskResult{}, err
		}

		// Execute after delay
		return performDelayedTask.Execute(ctx, taskID)
	},
)
```

## Common Patterns

### Payment Processing with Timeout

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/i2y/romancy"
)

type PaymentInitResult struct {
	PaymentID string `json:"payment_id"`
}

type PaymentTimeoutResult struct {
	Status string `json:"status"`
}

var paymentWithTimeout = romancy.DefineWorkflow("payment_with_timeout",
	func(ctx *romancy.WorkflowContext, orderID string) (PaymentTimeoutResult, error) {
		// Initiate payment
		paymentResult, err := initiatePayment.Execute(ctx, orderID)
		if err != nil {
			return PaymentTimeoutResult{}, err
		}
		paymentID := paymentResult.PaymentID

		// Wait for payment completion (5-minute timeout)
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			if errors.Is(err, romancy.ErrTimeout) {
				// Timeout occurred - cancel payment and order
				if _, err := cancelPayment.Execute(ctx, paymentID); err != nil {
					return PaymentTimeoutResult{}, err
				}
				if _, err := cancelOrder.Execute(ctx, orderID); err != nil {
					return PaymentTimeoutResult{}, err
				}
				return PaymentTimeoutResult{Status: "timeout"}, nil
			}
			return PaymentTimeoutResult{}, err
		}

		// Check payment status
		if event.Data["status"] == "success" {
			if _, err := fulfillOrder.Execute(ctx, orderID); err != nil {
				return PaymentTimeoutResult{}, err
			}
			return PaymentTimeoutResult{Status: "completed"}, nil
		}

		// Payment failed
		if _, err := cancelOrder.Execute(ctx, orderID); err != nil {
			return PaymentTimeoutResult{}, err
		}
		return PaymentTimeoutResult{Status: "payment_failed"}, nil
	},
)
```

### Approval Workflow

```go
package main

import (
	"context"
	"errors"
	"time"

	"github.com/i2y/romancy"
)

type ApprovalResult struct {
	Approved  bool   `json:"approved"`
	Approver  string `json:"approver"`
	Escalated bool   `json:"escalated"`
}

var approvalWorkflow = romancy.DefineWorkflow("approval_workflow",
	func(ctx *romancy.WorkflowContext, requestID string) (ApprovalResult, error) {
		// Send approval request to manager
		if _, err := sendApprovalRequest.Execute(ctx, requestID, "manager"); err != nil {
			return ApprovalResult{}, err
		}

		// Wait for manager approval (24 hours)
		event, err := romancy.WaitEvent(ctx, "approval.decision",
			romancy.WithTimeout(24*time.Hour),
		)
		if err != nil {
			if errors.Is(err, romancy.ErrTimeout) {
				// Escalate to director
				if _, err := sendApprovalRequest.Execute(ctx, requestID, "director"); err != nil {
					return ApprovalResult{}, err
				}

				// Wait for director approval (24 hours)
				event, err = romancy.WaitEvent(ctx, "approval.decision",
					romancy.WithTimeout(24*time.Hour),
				)
				if err != nil {
					return ApprovalResult{}, err
				}

				return ApprovalResult{
					Approved:  event.Data["approved"].(bool),
					Approver:  event.Data["approver"].(string),
					Escalated: true,
				}, nil
			}
			return ApprovalResult{}, err
		}

		return ApprovalResult{
			Approved: event.Data["approved"].(bool),
			Approver: event.Data["approver"].(string),
		}, nil
	},
)
```

### Batch Processing with Delays

```go
package main

import (
	"context"
	"time"

	"github.com/i2y/romancy"
)

// BatchItem represents an item to process
type BatchItem struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

type BatchResult struct {
	Processed int `json:"processed"`
}

type BatchProcessorResult struct {
	BatchID string        `json:"batch_id"`
	Results []BatchResult `json:"results"`
}

var batchProcessor = romancy.DefineWorkflow("batch_processor",
	func(ctx *romancy.WorkflowContext, input BatchInput) (BatchProcessorResult, error) {
		batchSize := 10
		results := make([]BatchResult, 0)

		for i := 0; i < len(input.Items); i += batchSize {
			end := i + batchSize
			if end > len(input.Items) {
				end = len(input.Items)
			}
			batch := input.Items[i:end]

			// Process batch
			batchResult, err := processBatch.Execute(ctx, batch)
			if err != nil {
				return BatchProcessorResult{}, err
			}
			results = append(results, batchResult)

			// Sleep between batches (except for last batch)
			if end < len(input.Items) {
				if err := romancy.Sleep(ctx, 5*time.Second); err != nil {
					return BatchProcessorResult{}, err
				}
			}
		}

		return BatchProcessorResult{
			BatchID: input.BatchID,
			Results: results,
		}, nil
	},
)

type BatchInput struct {
	BatchID string      `json:"batch_id"`
	Items   []BatchItem `json:"items"`
}
```

### Orchestrating Multiple Services

```go
package main

import (
	"context"
	"time"

	"github.com/i2y/romancy"
)

type ServiceResult struct {
	Status string `json:"status"`
	Data   string `json:"data"`
}

type OrchestrationResult struct {
	ServiceA ServiceResult `json:"service_a"`
	ServiceB ServiceResult `json:"service_b"`
	ServiceC ServiceResult `json:"service_c"`
}

var multiServiceOrchestration = romancy.DefineWorkflow("multi_service_orchestration",
	func(ctx *romancy.WorkflowContext, requestID string) (OrchestrationResult, error) {
		// Start all services
		if _, err := triggerServiceA.Execute(ctx, requestID); err != nil {
			return OrchestrationResult{}, err
		}
		if _, err := triggerServiceB.Execute(ctx, requestID); err != nil {
			return OrchestrationResult{}, err
		}
		if _, err := triggerServiceC.Execute(ctx, requestID); err != nil {
			return OrchestrationResult{}, err
		}

		var result OrchestrationResult

		// Wait for Service A
		eventA, err := romancy.WaitEvent(ctx, "service.a.completed",
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return OrchestrationResult{}, err
		}
		result.ServiceA = ServiceResult{
			Status: eventA.Data["status"].(string),
			Data:   eventA.Data["data"].(string),
		}

		// Wait for Service B
		eventB, err := romancy.WaitEvent(ctx, "service.b.completed",
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return OrchestrationResult{}, err
		}
		result.ServiceB = ServiceResult{
			Status: eventB.Data["status"].(string),
			Data:   eventB.Data["data"].(string),
		}

		// Wait for Service C
		eventC, err := romancy.WaitEvent(ctx, "service.c.completed",
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return OrchestrationResult{}, err
		}
		result.ServiceC = ServiceResult{
			Status: eventC.Data["status"].(string),
			Data:   eventC.Data["data"].(string),
		}

		// Return aggregated results
		return result, nil
	},
)
```

## Sending Events to Waiting Workflows

### Using CloudEvents HTTP

Send CloudEvents to resume waiting workflows:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

func sendPaymentCompletedEvent() error {
	// Create CloudEvent
	event := cloudevents.NewEvent()
	event.SetType("payment.completed")
	event.SetSource("payment-service")
	event.SetData(cloudevents.ApplicationJSON, map[string]any{
		"payment_id": "PAY-123",
		"amount":     99.99,
		"status":     "success",
	})

	// Convert to JSON
	eventBytes, err := json.Marshal(event)
	if err != nil {
		return err
	}

	// Send to Romancy
	req, err := http.NewRequest("POST", "http://localhost:8001/", bytes.NewBuffer(eventBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}
```

### Using curl

```bash
# Send CloudEvent via curl
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "evt-001",
    "data": {
      "payment_id": "PAY-123",
      "amount": 99.99,
      "status": "success"
    }
  }'
```

### Using send_event() from Activities

From within workflows or activities:

```go
package main

import (
	"context"

	"github.com/i2y/romancy"
)

type ProcessPaymentResult struct {
	Processed bool `json:"processed"`
}

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, paymentData PaymentData) (ProcessPaymentResult, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)

		// Process payment...

		// Send event to resume waiting workflows
		err := romancy.SendEvent(wfCtx,
			"payment.completed",
			"payment-processor",
			map[string]any{
				"payment_id": paymentData.ID,
				"amount":     paymentData.Amount,
				"status":     "success",
			},
		)
		if err != nil {
			return ProcessPaymentResult{}, err
		}

		return ProcessPaymentResult{Processed: true}, nil
	},
)

type PaymentData struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
}
```

## Best Practices

### 1. Always Set Timeouts

Prevent workflows from waiting indefinitely:

```go
// Good: Has timeout
event, err := romancy.WaitEvent(ctx, "payment.completed",
	romancy.WithTimeout(5*time.Minute),
)

// Bad: No timeout (could wait forever)
event, err := romancy.WaitEvent(ctx, "payment.completed")
```

### 2. Handle Timeout Gracefully

```go
event, err := romancy.WaitEvent(ctx, "approval.decision",
	romancy.WithTimeout(1*time.Hour),
)
if err != nil {
	if errors.Is(err, romancy.ErrTimeout) {
		// Handle timeout scenario
		return handleTimeoutScenario(ctx)
	}
	return nil, err
}
// Process approval
```

### 3. Use Unique Event Types

Avoid event type collisions:

```go
// Good: Specific event types
event, _ := romancy.WaitEvent(ctx, "order.payment.completed",
	romancy.WithTimeout(5*time.Minute),
)
event, _ := romancy.WaitEvent(ctx, "subscription.payment.completed",
	romancy.WithTimeout(5*time.Minute),
)

// Bad: Generic event type
event, _ := romancy.WaitEvent(ctx, "completed",
	romancy.WithTimeout(5*time.Minute),
) // Too generic
```

### 4. Include Correlation Data in Event Type

For matching specific events:

```go
type OrderWorkflowResult struct {
	PaymentID string  `json:"payment_id"`
	Amount    float64 `json:"amount"`
}

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderWorkflowResult, error) {
		// Send order_id as part of event type for correlation
		if _, err := initiatePayment.Execute(ctx, orderID); err != nil {
			return OrderWorkflowResult{}, err
		}

		// Wait for event with matching order_id
		event, err := romancy.WaitEvent(ctx,
			fmt.Sprintf("payment.completed.%s", orderID),
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		return OrderWorkflowResult{
			PaymentID: event.Data["payment_id"].(string),
			Amount:    event.Data["amount"].(float64),
		}, nil
	},
)
```

### 5. Consider Idempotency

Ensure events can be safely redelivered:

```go
type ProcessCheckResult struct {
	Processed bool `json:"processed"`
}

type IdempotentResult struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

var idempotentWorkflow = romancy.DefineWorkflow("idempotent_workflow",
	func(ctx *romancy.WorkflowContext, requestID string) (IdempotentResult, error) {
		// Check if already processed
		result, err := isAlreadyProcessed.Execute(ctx, requestID)
		if err != nil {
			return IdempotentResult{}, err
		}
		if result.Processed {
			return getPreviousResult.Execute(ctx, requestID)
		}

		// Wait for event
		event, err := romancy.WaitEvent(ctx,
			fmt.Sprintf("process.%s", requestID),
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return IdempotentResult{}, err
		}

		// Process idempotently
		return processIdempotently.Execute(ctx, event.Data)
	},
)
```

## Advanced Topics

### Multiple Event Types

Wait for different event types sequentially:

```go
type MultiEventResult struct {
	Status string `json:"status"`
}

var multiEventWorkflow = romancy.DefineWorkflow("multi_event_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (MultiEventResult, error) {
		// Option 1: Sequential checks with short timeouts
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(1*time.Minute),
		)
		if err != nil {
			if errors.Is(err, romancy.ErrTimeout) {
				// Try waiting for failure event
				event, err = romancy.WaitEvent(ctx, "payment.failed",
					romancy.WithTimeout(1*time.Minute),
				)
				if err != nil {
					return MultiEventResult{}, err
				}
			} else {
				return MultiEventResult{}, err
			}
		}

		// Process based on event type
		if event.Type == "payment.completed" {
			return MultiEventResult{Status: "success"}, nil
		}
		return MultiEventResult{Status: "failed"}, nil
	},
)
```

### Event Filtering

Filter events based on data:

```go
type FilteredEventResult struct {
	Action string `json:"action"`
	Status string `json:"status"`
}

var filteredEventWorkflow = romancy.DefineWorkflow("filtered_event_workflow",
	func(ctx *romancy.WorkflowContext, userID string) (FilteredEventResult, error) {
		// Wait for event specific to this user
		event, err := romancy.WaitEvent(ctx,
			fmt.Sprintf("user.action.%s", userID),
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return FilteredEventResult{}, err
		}

		// Additional filtering can be done after receiving
		if event.Data["action"] == "purchase" {
			return processPurchase.Execute(ctx, event.Data)
		}

		return FilteredEventResult{Action: "skipped"}, nil
	},
)
```

### Cancellation

Workflows waiting for events can be cancelled:

```go
package main

import (
	"context"
	"net/http"

	"github.com/i2y/romancy"
)

// Cancel via HTTP API
// POST /cancel/{instance_id}

// Or programmatically
func cancelWaitingWorkflow(ctx context.Context, app *romancy.App, instanceID string) error {
	return app.CancelWorkflow(ctx, instanceID)
}
```

## Monitoring and Debugging

### Viewer UI

The Romancy Viewer shows waiting workflows:

- Status: `waiting_for_event` or `waiting_for_timer`
- Event type being waited for
- Timeout information
- Time elapsed

### Logging

Add logging for debugging:

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/i2y/romancy"
)

type LoggedResult struct {
	EventID string `json:"event_id"`
}

var loggedWorkflow = romancy.DefineWorkflow("logged_workflow",
	func(ctx *romancy.WorkflowContext, input string) (LoggedResult, error) {
		log.Printf("Waiting for payment event, instance: %s", ctx.InstanceID())

		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			log.Printf("Wait event error: %v", err)
			return LoggedResult{}, err
		}

		log.Printf("Received event: %s with data: %v", event.ID, event.Data)

		return LoggedResult{EventID: event.ID}, nil
	},
)
```

### Using Lifecycle Hooks

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy"
)

type DebugHooks struct {
	romancy.HooksBase
}

func (h *DebugHooks) OnEventReceived(ctx context.Context, instanceID, eventType string, eventData any) {
	log.Printf("Event received for workflow %s: type=%s", instanceID, eventType)
}

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("workflow.db"),
		romancy.WithHooks(&DebugHooks{}),
	)
	// ...
}
```

## Performance Considerations

### Worker Release

- Waiting workflows don't consume worker resources
- Workers can handle other workflows while waiting
- Scales to thousands of waiting workflows

### Event Matching

- Event type matching is exact (string comparison)
- Consider using hierarchical event types for flexibility
- Database indexes on event_type improve performance

### Timer Precision

- Timers checked every 10 seconds by default
- Maximum 10-second delay in timer expiration
- Adequate for most workflow scheduling needs

## Related Topics

- [CloudEvents HTTP Binding](/docs/core-features/events/cloudevents-http-binding) - CloudEvents integration
- [Workflows and Activities](/docs/core-features/workflows-activities) - Basic workflow concepts
- [Saga Pattern](/docs/core-features/saga-compensation) - Compensation and rollback
- [Examples: Event Waiting](/docs/examples/events) - Practical examples
