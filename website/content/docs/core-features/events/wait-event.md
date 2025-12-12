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

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, input string) (map[string]any, error) {
		// Wait for an event (with 5-minute timeout)
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return nil, err
		}

		// Access event data
		paymentID := event.Data["payment_id"].(string)
		amount := event.Data["amount"].(float64)

		return map[string]any{
			"payment_id": paymentID,
			"amount":     amount,
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

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, input string) (map[string]any, error) {
		// Sleep for 60 seconds
		err := romancy.Sleep(ctx, 60*time.Second)
		if err != nil {
			return nil, err
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

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		// Wait for event
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return nil, err
		}

		// Parse event data into struct
		var payment PaymentCompleted
		if err := parseEventData(event.Data, &payment); err != nil {
			return nil, err
		}

		if payment.Status == "success" {
			return map[string]any{
				"order_id":   orderID,
				"payment_id": payment.PaymentID,
			}, nil
		}

		return nil, fmt.Errorf("payment failed: %s", payment.Status)
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

var scheduledTask = romancy.DefineWorkflow("scheduled_task",
	func(ctx *romancy.WorkflowContext, taskID string) (map[string]any, error) {
		// Execute immediately
		if _, err := performInitialTask.Execute(ctx, taskID); err != nil {
			return nil, err
		}

		// Sleep for 1 hour
		if err := romancy.Sleep(ctx, 1*time.Hour); err != nil {
			return nil, err
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

var paymentWithTimeout = romancy.DefineWorkflow("payment_with_timeout",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		// Initiate payment
		paymentResult, err := initiatePayment.Execute(ctx, orderID)
		if err != nil {
			return nil, err
		}
		paymentID := paymentResult["payment_id"].(string)

		// Wait for payment completion (5-minute timeout)
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			if errors.Is(err, romancy.ErrTimeout) {
				// Timeout occurred - cancel payment and order
				if _, err := cancelPayment.Execute(ctx, paymentID); err != nil {
					return nil, err
				}
				if _, err := cancelOrder.Execute(ctx, orderID); err != nil {
					return nil, err
				}
				return map[string]any{"status": "timeout"}, nil
			}
			return nil, err
		}

		// Check payment status
		if event.Data["status"] == "success" {
			if _, err := fulfillOrder.Execute(ctx, orderID); err != nil {
				return nil, err
			}
			return map[string]any{"status": "completed"}, nil
		}

		// Payment failed
		if _, err := cancelOrder.Execute(ctx, orderID); err != nil {
			return nil, err
		}
		return map[string]any{"status": "payment_failed"}, nil
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

var approvalWorkflow = romancy.DefineWorkflow("approval_workflow",
	func(ctx *romancy.WorkflowContext, requestID string) (map[string]any, error) {
		// Send approval request to manager
		if _, err := sendApprovalRequest.Execute(ctx, requestID, "manager"); err != nil {
			return nil, err
		}

		// Wait for manager approval (24 hours)
		event, err := romancy.WaitEvent(ctx, "approval.decision",
			romancy.WithTimeout(24*time.Hour),
		)
		if err != nil {
			if errors.Is(err, romancy.ErrTimeout) {
				// Escalate to director
				if _, err := sendApprovalRequest.Execute(ctx, requestID, "director"); err != nil {
					return nil, err
				}

				// Wait for director approval (24 hours)
				event, err = romancy.WaitEvent(ctx, "approval.decision",
					romancy.WithTimeout(24*time.Hour),
				)
				if err != nil {
					return nil, err
				}

				return map[string]any{
					"approved":  event.Data["approved"],
					"approver":  event.Data["approver"],
					"escalated": true,
				}, nil
			}
			return nil, err
		}

		return map[string]any{
			"approved": event.Data["approved"],
			"approver": event.Data["approver"],
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

var batchProcessor = romancy.DefineWorkflow("batch_processor",
	func(ctx *romancy.WorkflowContext, input BatchInput) (map[string]any, error) {
		batchSize := 10
		results := make([]map[string]any, 0)

		for i := 0; i < len(input.Items); i += batchSize {
			end := i + batchSize
			if end > len(input.Items) {
				end = len(input.Items)
			}
			batch := input.Items[i:end]

			// Process batch
			batchResult, err := processBatch.Execute(ctx, batch)
			if err != nil {
				return nil, err
			}
			results = append(results, batchResult)

			// Sleep between batches (except for last batch)
			if end < len(input.Items) {
				if err := romancy.Sleep(ctx, 5*time.Second); err != nil {
					return nil, err
				}
			}
		}

		return map[string]any{
			"batch_id": input.BatchID,
			"results":  results,
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

var multiServiceOrchestration = romancy.DefineWorkflow("multi_service_orchestration",
	func(ctx *romancy.WorkflowContext, requestID string) (map[string]any, error) {
		// Start all services
		if _, err := triggerServiceA.Execute(ctx, requestID); err != nil {
			return nil, err
		}
		if _, err := triggerServiceB.Execute(ctx, requestID); err != nil {
			return nil, err
		}
		if _, err := triggerServiceC.Execute(ctx, requestID); err != nil {
			return nil, err
		}

		// Wait for all services to complete
		results := make(map[string]any)

		// Wait for Service A
		eventA, err := romancy.WaitEvent(ctx, "service.a.completed",
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return nil, err
		}
		results["service_a"] = eventA.Data

		// Wait for Service B
		eventB, err := romancy.WaitEvent(ctx, "service.b.completed",
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return nil, err
		}
		results["service_b"] = eventB.Data

		// Wait for Service C
		eventC, err := romancy.WaitEvent(ctx, "service.c.completed",
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return nil, err
		}
		results["service_c"] = eventC.Data

		// Aggregate and return results
		return aggregateResults(results), nil
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

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, paymentData PaymentData) (map[string]any, error) {
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
			return nil, err
		}

		return map[string]any{"processed": true}, nil
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
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		// Send order_id as part of event type for correlation
		if _, err := initiatePayment.Execute(ctx, orderID); err != nil {
			return nil, err
		}

		// Wait for event with matching order_id
		event, err := romancy.WaitEvent(ctx,
			fmt.Sprintf("payment.completed.%s", orderID),
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return nil, err
		}

		return map[string]any{"payment": event.Data}, nil
	},
)
```

### 5. Consider Idempotency

Ensure events can be safely redelivered:

```go
var idempotentWorkflow = romancy.DefineWorkflow("idempotent_workflow",
	func(ctx *romancy.WorkflowContext, requestID string) (map[string]any, error) {
		// Check if already processed
		result, err := isAlreadyProcessed.Execute(ctx, requestID)
		if err != nil {
			return nil, err
		}
		if result["processed"].(bool) {
			return getPreviousResult.Execute(ctx, requestID)
		}

		// Wait for event
		event, err := romancy.WaitEvent(ctx,
			fmt.Sprintf("process.%s", requestID),
			romancy.WithTimeout(10*time.Minute),
		)
		if err != nil {
			return nil, err
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
var multiEventWorkflow = romancy.DefineWorkflow("multi_event_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
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
					return nil, err
				}
			} else {
				return nil, err
			}
		}

		// Process based on event type
		if event.Type == "payment.completed" {
			return map[string]any{"status": "success"}, nil
		}
		return map[string]any{"status": "failed"}, nil
	},
)
```

### Event Filtering

Filter events based on data:

```go
var filteredEventWorkflow = romancy.DefineWorkflow("filtered_event_workflow",
	func(ctx *romancy.WorkflowContext, userID string) (map[string]any, error) {
		// Wait for event specific to this user
		event, err := romancy.WaitEvent(ctx,
			fmt.Sprintf("user.action.%s", userID),
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return nil, err
		}

		// Additional filtering can be done after receiving
		if event.Data["action"] == "purchase" {
			return processPurchase.Execute(ctx, event.Data)
		}

		return map[string]any{"action": "skipped"}, nil
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

var loggedWorkflow = romancy.DefineWorkflow("logged_workflow",
	func(ctx *romancy.WorkflowContext, input string) (map[string]any, error) {
		log.Printf("Waiting for payment event, instance: %s", ctx.InstanceID())

		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			log.Printf("Wait event error: %v", err)
			return nil, err
		}

		log.Printf("Received event: %s with data: %v", event.ID, event.Data)

		return map[string]any{"event_id": event.ID}, nil
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
