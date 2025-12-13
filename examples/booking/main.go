// Package main demonstrates human-in-the-loop workflow patterns.
//
// This example shows:
// - Waiting for human approval events
// - Event timeout handling
// - Multi-step approval processes
// - Real-world expense approval workflow
//
// Usage:
//
//	go run ./examples/booking/
//
// Then approve or reject:
//
//	# Approve
//	go run ./cmd/romancy/ event <instance_id> approval.response '{"approved": true, "approver": "manager@example.com"}' --db booking_demo.db
//
//	# Reject
//	go run ./cmd/romancy/ event <instance_id> approval.response '{"approved": false, "approver": "manager@example.com", "reason": "Over budget"}'  --db booking_demo.db
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/hooks/otel"
)

// ----- Types -----

// ExpenseInput is the input for the expense approval workflow.
type ExpenseInput struct {
	ExpenseID   string  `json:"expense_id"`
	EmployeeID  string  `json:"employee_id"`
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
	Category    string  `json:"category"` // "travel", "equipment", "software", etc.
}

// ExpenseResult is the output of the expense approval workflow.
type ExpenseResult struct {
	ExpenseID       string `json:"expense_id"`
	Status          string `json:"status"`
	ApprovedBy      string `json:"approved_by,omitempty"`
	RejectedBy      string `json:"rejected_by,omitempty"`
	RejectionReason string `json:"rejection_reason,omitempty"`
	ProcessedAt     string `json:"processed_at,omitempty"`
}

// ApprovalResponse is the event data for approval responses.
type ApprovalResponse struct {
	Approved bool   `json:"approved"`
	Approver string `json:"approver"`
	Reason   string `json:"reason,omitempty"` // Reason for rejection
}

// ----- Activities -----

// ValidateExpense validates the expense request.
var ValidateExpense = romancy.DefineActivity(
	"validate_expense",
	func(ctx context.Context, input ExpenseInput) (bool, error) {
		log.Printf("[Activity] Validating expense: %s (Amount: $%.2f, Category: %s)",
			input.ExpenseID, input.Amount, input.Category)
		time.Sleep(100 * time.Millisecond) // Simulate validation

		// Basic validation rules
		if input.Amount <= 0 {
			return false, fmt.Errorf("invalid amount: $%.2f", input.Amount)
		}
		if input.Description == "" {
			return false, fmt.Errorf("description is required")
		}

		log.Printf("[Activity] Expense validated successfully")
		return true, nil
	},
)

// NotifyApprover sends a notification to the approver.
var NotifyApprover = romancy.DefineActivity(
	"notify_approver",
	func(ctx context.Context, input struct {
		ExpenseID   string
		EmployeeID  string
		Amount      float64
		Description string
	}) (bool, error) {
		log.Printf("[Activity] Notifying approver about expense %s", input.ExpenseID)
		log.Printf("  - Employee: %s", input.EmployeeID)
		log.Printf("  - Amount: $%.2f", input.Amount)
		log.Printf("  - Description: %s", input.Description)
		time.Sleep(100 * time.Millisecond) // Simulate notification
		log.Printf("[Activity] Approver notified")
		return true, nil
	},
)

// ProcessApprovedExpense processes an approved expense.
var ProcessApprovedExpense = romancy.DefineActivity(
	"process_approved_expense",
	func(ctx context.Context, input struct {
		ExpenseID  string
		EmployeeID string
		Amount     float64
	}) (string, error) {
		log.Printf("[Activity] Processing approved expense %s", input.ExpenseID)
		log.Printf("  - Reimbursing $%.2f to employee %s", input.Amount, input.EmployeeID)
		time.Sleep(150 * time.Millisecond) // Simulate payment processing
		log.Printf("[Activity] Expense processed successfully")
		return fmt.Sprintf("Reimbursement scheduled for $%.2f", input.Amount), nil
	},
)

// NotifyEmployee sends a notification to the employee about the decision.
var NotifyEmployee = romancy.DefineActivity(
	"notify_employee",
	func(ctx context.Context, input struct {
		EmployeeID string
		ExpenseID  string
		Approved   bool
		Reason     string
	}) (bool, error) {
		status := "approved"
		if !input.Approved {
			status = "rejected"
		}
		log.Printf("[Activity] Notifying employee %s: expense %s was %s",
			input.EmployeeID, input.ExpenseID, status)
		if input.Reason != "" {
			log.Printf("  - Reason: %s", input.Reason)
		}
		time.Sleep(50 * time.Millisecond) // Simulate notification
		return true, nil
	},
)

// ----- Workflow -----

// expenseApprovalWorkflow handles the expense approval process.
var expenseApprovalWorkflow = romancy.DefineWorkflow("expense_approval_workflow",
	func(ctx *romancy.WorkflowContext, input ExpenseInput) (ExpenseResult, error) {
		log.Printf("[Workflow] Starting expense approval for: %s", input.ExpenseID)
		log.Printf("[Workflow] Employee: %s, Amount: $%.2f, Category: %s",
			input.EmployeeID, input.Amount, input.Category)

		result := ExpenseResult{
			ExpenseID: input.ExpenseID,
			Status:    "pending",
		}

		// Step 1: Validate the expense
		log.Printf("\n[Workflow] Step 1: Validating expense...")
		valid, err := ValidateExpense.Execute(ctx, input)
		if err != nil || !valid {
			result.Status = "validation_failed"
			return result, fmt.Errorf("expense validation failed: %w", err)
		}

		// Step 2: Notify approver
		log.Printf("\n[Workflow] Step 2: Notifying approver...")
		_, err = NotifyApprover.Execute(ctx, struct {
			ExpenseID   string
			EmployeeID  string
			Amount      float64
			Description string
		}{
			ExpenseID:   input.ExpenseID,
			EmployeeID:  input.EmployeeID,
			Amount:      input.Amount,
			Description: input.Description,
		})
		if err != nil {
			result.Status = "notification_failed"
			return result, fmt.Errorf("failed to notify approver: %w", err)
		}

		// Step 3: Wait for approval response (with timeout)
		log.Printf("\n[Workflow] Step 3: Waiting for approval response...")
		log.Printf("[Workflow] Instance ID: %s", ctx.InstanceID())
		log.Printf("[Workflow] To approve, run:")
		log.Printf(`  go run ./cmd/romancy/ event %s approval.response '{"approved": true, "approver": "manager@example.com"}' --db booking_demo.db`, ctx.InstanceID())
		log.Printf("[Workflow] To reject, run:")
		log.Printf(`  go run ./cmd/romancy/ event %s approval.response '{"approved": false, "approver": "manager@example.com", "reason": "Over budget"}' --db booking_demo.db`, ctx.InstanceID())

		result.Status = "waiting_for_approval"

		// Wait for approval event with 24 hour timeout (for demo, we'll use shorter timeout)
		event, err := romancy.WaitEvent[ApprovalResponse](ctx, "approval.response",
			romancy.WithEventTimeout(24*time.Hour)) // In real scenario, this would be longer
		if err != nil {
			// SuspendSignal is returned as normal workflow suspension when waiting for event
			result.Status = "waiting_for_approval"
			return result, err
		}

		log.Printf("\n[Workflow] Received approval response from: %s", event.Data.Approver)

		// Step 4: Process based on approval decision
		if event.Data.Approved {
			log.Printf("[Workflow] Step 4: Processing approved expense...")

			// Process the approved expense
			_, err := ProcessApprovedExpense.Execute(ctx, struct {
				ExpenseID  string
				EmployeeID string
				Amount     float64
			}{
				ExpenseID:  input.ExpenseID,
				EmployeeID: input.EmployeeID,
				Amount:     input.Amount,
			})
			if err != nil {
				result.Status = "processing_failed"
				return result, fmt.Errorf("failed to process expense: %w", err)
			}

			result.Status = "approved"
			result.ApprovedBy = event.Data.Approver
			result.ProcessedAt = time.Now().Format(time.RFC3339)

			log.Printf("[Workflow] Expense approved and processed!")
		} else {
			log.Printf("[Workflow] Step 4: Processing rejection...")
			result.Status = "rejected"
			result.RejectedBy = event.Data.Approver
			result.RejectionReason = event.Data.Reason
			log.Printf("[Workflow] Expense rejected. Reason: %s", event.Data.Reason)
		}

		// Step 5: Notify employee about the decision
		log.Printf("\n[Workflow] Step 5: Notifying employee...")
		_, _ = NotifyEmployee.Execute(ctx, struct {
			EmployeeID string
			ExpenseID  string
			Approved   bool
			Reason     string
		}{
			EmployeeID: input.EmployeeID,
			ExpenseID:  input.ExpenseID,
			Approved:   event.Data.Approved,
			Reason:     event.Data.Reason,
		})

		log.Printf("\n[Workflow] Expense approval workflow completed!")
		return result, nil
	},
)

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-booking"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(10*time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
	)
	return tp, nil
}

// ----- Main -----

func main() {
	ctx := context.Background()

	// Setup OpenTelemetry tracing
	tp, err := setupTracing(ctx)
	if err != nil {
		log.Printf("Warning: Failed to setup tracing: %v", err)
	}
	if tp != nil {
		defer func() {
			if err := tp.Shutdown(ctx); err != nil {
				log.Printf("Error shutting down tracer: %v", err)
			}
		}()
	}

	// Get database URL from environment or use default
	dbPath := os.Getenv("DATABASE_URL")
	if dbPath == "" {
		dbPath = "booking_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbPath),
		romancy.WithWorkerID("booking-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register the workflow
	romancy.RegisterWorkflow[ExpenseInput, ExpenseResult](app, expenseApprovalWorkflow)

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}

	log.Println("==============================================")
	log.Println("Human Approval Workflow Example")
	log.Printf("Database: %s", dbPath)
	log.Println("==============================================")

	// Start an example expense approval workflow
	expenseInput := ExpenseInput{
		ExpenseID:   fmt.Sprintf("EXP-%d", time.Now().Unix()),
		EmployeeID:  "emp-001",
		Amount:      250.00,
		Description: "Conference travel expenses",
		Category:    "travel",
	}

	instanceID, err := romancy.StartWorkflow(ctx, app, expenseApprovalWorkflow, expenseInput)
	if err != nil {
		log.Printf("Failed to start workflow: %v", err)
		return
	}

	log.Println("")
	log.Printf("Expense Approval Workflow Started!")
	log.Printf("Instance ID: %s", instanceID)
	log.Println("")
	log.Println("The workflow is now waiting for human approval.")
	log.Println("")
	log.Println("Commands:")
	log.Printf("  Check status: go run ./cmd/romancy/ get %s --db %s", instanceID, dbPath)
	log.Println("")
	log.Printf("  Approve:      go run ./cmd/romancy/ event %s approval.response '{\"approved\": true, \"approver\": \"manager@example.com\"}' --db %s", instanceID, dbPath)
	log.Println("")
	log.Printf("  Reject:       go run ./cmd/romancy/ event %s approval.response '{\"approved\": false, \"approver\": \"manager@example.com\", \"reason\": \"Over budget\"}' --db %s", instanceID, dbPath)
	log.Println("")
	log.Println("Or via HTTP:")
	fmt.Println(`curl -X POST http://localhost:8083/ \`)
	fmt.Println(`    -H "Content-Type: application/json" \`)
	fmt.Printf(`    -d '{"specversion": "1.0", "type": "approval.response", "source": "approval-ui", "id": "evt-123", "romancyinstanceid": "%s", "data": {"approved": true, "approver": "manager@example.com"}}'`+"\n", instanceID)
	log.Println("")
	log.Println("==============================================")

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in goroutine
	go func() {
		if err := app.ListenAndServe(":8083"); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Println("HTTP server running on :8083")
	log.Println("Press Ctrl+C to stop")

	// Wait for signal
	<-sigCh
	log.Println("\nShutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}

	log.Println("Goodbye!")
}
