package romancy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// PaymentEvent represents payment confirmation data.
type PaymentEvent struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
	Status        string  `json:"status"`
}

// OrderResult represents the output of the order workflow.
type OrderResult struct {
	OrderID       string  `json:"order_id"`
	PaymentTxnID  string  `json:"payment_txn_id"`
	PaymentAmount float64 `json:"payment_amount"`
	Status        string  `json:"status"`
}

// TestWorkflowWithWaitEvent tests a workflow that waits for an event.
func TestWorkflowWithWaitEvent(t *testing.T) {
	// Create app with pre-initialized test database
	app, cleanup := createTestApp(t, WithWorkflowResumptionInterval(100*time.Millisecond))
	defer cleanup()

	// Define a workflow that waits for an event
	orderWorkflow := DefineWorkflow[string, OrderResult](
		"order_workflow",
		func(ctx *WorkflowContext, orderID string) (OrderResult, error) {
			// Wait for payment confirmation event
			event, err := WaitEvent[PaymentEvent](ctx, "payment.confirmed")
			if err != nil {
				return OrderResult{}, err
			}

			return OrderResult{
				OrderID:       orderID,
				PaymentTxnID:  event.Data.TransactionID,
				PaymentAmount: event.Data.Amount,
				Status:        "completed",
			}, nil
		},
	)

	// Register workflow
	RegisterWorkflow(app, orderWorkflow)

	// Start the app
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Start the workflow
	instanceID, err := StartWorkflow(ctx, app, orderWorkflow, "ORD-123")
	if err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}

	// Wait a bit for the workflow to start and register the event subscription
	time.Sleep(200 * time.Millisecond)

	// Verify workflow is waiting for event
	instance, err := app.GetInstance(ctx, instanceID)
	if err != nil {
		t.Fatalf("failed to get instance: %v", err)
	}
	// WaitEvent now uses channel system internally, so status is waiting_for_message
	if instance.Status != storage.StatusWaitingForMessage {
		t.Errorf("expected status 'waiting_for_message', got '%s'", instance.Status)
	}

	// Simulate sending the payment event
	paymentEvent := &CloudEvent{
		ID:          "evt-1",
		Type:        "payment.confirmed",
		Source:      "payment-service",
		SpecVersion: "1.0",
		Data: mustMarshal(PaymentEvent{
			TransactionID: "TXN-456",
			Amount:        99.99,
			Status:        "success",
		}),
	}

	// Process the cloud event
	app.processCloudEvent(ctx, paymentEvent)

	// Wait for the workflow to complete
	time.Sleep(500 * time.Millisecond)

	// Get the result
	result, err := GetWorkflowResult[OrderResult](ctx, app, instanceID)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	if result.Status != "completed" {
		t.Errorf("expected workflow status 'completed', got '%s'", result.Status)
	}

	if result.Output.OrderID != "ORD-123" {
		t.Errorf("expected order ID 'ORD-123', got '%s'", result.Output.OrderID)
	}

	if result.Output.PaymentTxnID != "TXN-456" {
		t.Errorf("expected payment txn ID 'TXN-456', got '%s'", result.Output.PaymentTxnID)
	}

	if result.Output.PaymentAmount != 99.99 {
		t.Errorf("expected payment amount 99.99, got %f", result.Output.PaymentAmount)
	}
}

// TestWorkflowWithEventTimeout tests event timeout handling.
func TestWorkflowWithEventTimeout(t *testing.T) {
	// Create app with pre-initialized test database
	app, cleanup := createTestApp(t,
		WithEventTimeoutInterval(100*time.Millisecond),
		WithWorkflowResumptionInterval(100*time.Millisecond),
	)
	defer cleanup()

	// Define a workflow that waits for an event with a short timeout
	timeoutWorkflow := DefineWorkflow[string, string](
		"timeout_workflow",
		func(ctx *WorkflowContext, input string) (string, error) {
			// Wait for event with 200ms timeout
			_, err := WaitEvent[any](ctx, "some.event", WithEventTimeout(200*time.Millisecond))
			if err != nil {
				return "", err
			}
			return "received", nil
		},
	)

	// Register workflow
	RegisterWorkflow(app, timeoutWorkflow)

	// Start the app
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Start the workflow
	instanceID, err := StartWorkflow(ctx, app, timeoutWorkflow, "test")
	if err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}

	// Wait for timeout to occur.
	// SQLite datetime comparisons work at second granularity, so we need to wait
	// for the timeout (200ms) to expire AND for the timeout check loop (100ms) to run
	// AND for the datetime comparison to evaluate to true (needs at least 1 second past).
	// Using 2 seconds to ensure reliable timeout detection.
	time.Sleep(2 * time.Second)

	// Verify workflow failed due to timeout
	instance, err := app.GetInstance(ctx, instanceID)
	if err != nil {
		t.Fatalf("failed to get instance: %v", err)
	}
	if instance.Status != storage.StatusFailed {
		t.Errorf("expected status 'failed', got '%s'", instance.Status)
	}
}

// TestWorkflowWithSleep tests sleep functionality.
func TestWorkflowWithSleep(t *testing.T) {
	// Create app with pre-initialized test database
	app, cleanup := createTestApp(t, WithTimerCheckInterval(100*time.Millisecond))
	defer cleanup()

	// Define a workflow with a sleep
	sleepWorkflow := DefineWorkflow[string, string](
		"sleep_workflow",
		func(ctx *WorkflowContext, input string) (string, error) {
			// Sleep for 1 second (long enough to check status)
			if err := Sleep(ctx, 1*time.Second); err != nil {
				return "", err
			}

			return "completed after sleep", nil
		},
	)

	// Register workflow
	RegisterWorkflow(app, sleepWorkflow)

	// Start the app
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Start the workflow
	instanceID, err := StartWorkflow(ctx, app, sleepWorkflow, "test")
	if err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}

	// Wait a bit for workflow to register sleep timer
	time.Sleep(100 * time.Millisecond)

	// Verify workflow is waiting for timer
	instance, err := app.GetInstance(ctx, instanceID)
	if err != nil {
		t.Fatalf("failed to get instance: %v", err)
	}
	if instance.Status != storage.StatusWaitingForTimer {
		t.Errorf("expected status 'waiting_for_timer', got '%s'", instance.Status)
	}

	// Wait for sleep to expire and workflow to complete
	time.Sleep(1500 * time.Millisecond)

	// Get the result
	result, err := GetWorkflowResult[string](ctx, app, instanceID)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	if result.Status != "completed" {
		t.Errorf("expected status 'completed', got '%s'", result.Status)
	}

	if result.Output != "completed after sleep" {
		t.Errorf("expected output 'completed after sleep', got '%s'", result.Output)
	}
}

// mustMarshal marshals data to JSON, panicking on error.
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
