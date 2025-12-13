// Package main demonstrates sleep/timer functionality in workflows.
//
// This example shows:
// - Sleep for relative delays
// - SleepUntil for absolute time
// - Time-based workflow scheduling
//
// Usage:
//
//	go run ./examples/timer/
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

// ReminderInput is the input for the reminder workflow.
type ReminderInput struct {
	UserID       string `json:"user_id"`
	Message      string `json:"message"`
	DelaySeconds int    `json:"delay_seconds"`
}

// ReminderResult is the output of the reminder workflow.
type ReminderResult struct {
	UserID  string `json:"user_id"`
	Message string `json:"message"`
	SentAt  string `json:"sent_at"`
	Status  string `json:"status"`
}

// ScheduledTaskInput is the input for the scheduled task workflow.
type ScheduledTaskInput struct {
	TaskID      string    `json:"task_id"`
	ScheduledAt time.Time `json:"scheduled_at"`
}

// ScheduledTaskResult is the output of the scheduled task workflow.
type ScheduledTaskResult struct {
	TaskID     string `json:"task_id"`
	ExecutedAt string `json:"executed_at"`
	Status     string `json:"status"`
}

// ----- Activities -----

// SendReminder simulates sending a reminder notification.
var SendReminder = romancy.DefineActivity(
	"send_reminder",
	func(ctx context.Context, input struct {
		UserID  string
		Message string
	}) (string, error) {
		log.Printf("[Activity] Sending reminder to user %s: %s", input.UserID, input.Message)
		time.Sleep(100 * time.Millisecond) // Simulate sending
		return fmt.Sprintf("Reminder sent at %s", time.Now().Format(time.RFC3339)), nil
	},
)

// ExecuteScheduledTask simulates executing a scheduled task.
var ExecuteScheduledTask = romancy.DefineActivity(
	"execute_scheduled_task",
	func(ctx context.Context, taskID string) (string, error) {
		log.Printf("[Activity] Executing scheduled task: %s", taskID)
		time.Sleep(100 * time.Millisecond) // Simulate work
		return fmt.Sprintf("Task %s executed successfully", taskID), nil
	},
)

// ----- Workflows -----

// reminderWorkflow sends a reminder after a delay.
var reminderWorkflow = romancy.DefineWorkflow("reminder_workflow",
	func(ctx *romancy.WorkflowContext, input ReminderInput) (ReminderResult, error) {
		log.Printf("[Workflow] Starting reminder workflow for user %s", input.UserID)
		log.Printf("[Workflow] Will wait %d seconds before sending reminder", input.DelaySeconds)

		result := ReminderResult{
			UserID:  input.UserID,
			Message: input.Message,
			Status:  "waiting",
		}

		// Sleep for the specified duration
		log.Printf("[Workflow] Sleeping for %d seconds...", input.DelaySeconds)
		if err := romancy.Sleep(ctx, time.Duration(input.DelaySeconds)*time.Second); err != nil {
			return result, err // SuspendSignal is returned as normal workflow suspension
		}

		log.Printf("[Workflow] Sleep completed, sending reminder...")

		// Send the reminder
		sentInfo, err := SendReminder.Execute(ctx, struct {
			UserID  string
			Message string
		}{
			UserID:  input.UserID,
			Message: input.Message,
		})
		if err != nil {
			result.Status = "send_failed"
			return result, fmt.Errorf("failed to send reminder: %w", err)
		}

		result.SentAt = sentInfo
		result.Status = "sent"

		log.Printf("[Workflow] Reminder sent successfully!")
		return result, nil
	},
)

// scheduledTaskWorkflow executes a task at a specific time.
var scheduledTaskWorkflow = romancy.DefineWorkflow("scheduled_task_workflow",
	func(ctx *romancy.WorkflowContext, input ScheduledTaskInput) (ScheduledTaskResult, error) {
		log.Printf("[Workflow] Scheduling task %s for %s", input.TaskID, input.ScheduledAt.Format(time.RFC3339))

		result := ScheduledTaskResult{
			TaskID: input.TaskID,
			Status: "scheduled",
		}

		// Sleep until the specified time using SleepUntil
		log.Printf("[Workflow] Sleeping until %s...", input.ScheduledAt.Format(time.RFC3339))
		if err := romancy.SleepUntil(ctx, input.ScheduledAt); err != nil {
			return result, err // SuspendSignal is returned as normal workflow suspension
		}

		log.Printf("[Workflow] Scheduled time reached, executing task...")

		// Execute the task
		execResult, err := ExecuteScheduledTask.Execute(ctx, input.TaskID)
		if err != nil {
			result.Status = "execution_failed"
			return result, fmt.Errorf("failed to execute task: %w", err)
		}

		result.ExecutedAt = time.Now().Format(time.RFC3339)
		result.Status = execResult

		log.Printf("[Workflow] Scheduled task completed!")
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
		serviceName = "romancy-timer"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(10*time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)), sdktrace.WithResource(res))
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
		dbPath = "timer_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbPath),
		romancy.WithWorkerID("timer-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register workflows
	romancy.RegisterWorkflow[ReminderInput, ReminderResult](app, reminderWorkflow)
	romancy.RegisterWorkflow[ScheduledTaskInput, ScheduledTaskResult](app, scheduledTaskWorkflow)

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}

	log.Println("==============================================")
	log.Println("Timer Workflow Example")
	log.Printf("Database: %s", dbPath)
	log.Println("==============================================")

	// Example 1: Reminder with relative delay (Sleep)
	log.Println("\n--- Example 1: Reminder Workflow (Sleep) ---")
	reminderInput := ReminderInput{
		UserID:       "user-001",
		Message:      "Don't forget your meeting!",
		DelaySeconds: 5, // 5 seconds delay
	}

	reminderInstanceID, err := romancy.StartWorkflow(ctx, app, reminderWorkflow, reminderInput)
	if err != nil {
		log.Printf("Failed to start reminder workflow: %v", err)
		return
	}
	log.Printf("Reminder workflow started: %s", reminderInstanceID)
	log.Printf("Will send reminder in %d seconds...", reminderInput.DelaySeconds)

	// Example 2: Scheduled task with absolute time (SleepUntil)
	log.Println("\n--- Example 2: Scheduled Task Workflow (SleepUntil) ---")
	scheduledTime := time.Now().Add(8 * time.Second) // 8 seconds from now
	taskInput := ScheduledTaskInput{
		TaskID:      "task-001",
		ScheduledAt: scheduledTime,
	}

	taskInstanceID, err := romancy.StartWorkflow(ctx, app, scheduledTaskWorkflow, taskInput)
	if err != nil {
		log.Printf("Failed to start scheduled task workflow: %v", err)
		return
	}
	log.Printf("Scheduled task workflow started: %s", taskInstanceID)
	log.Printf("Task scheduled for: %s", scheduledTime.Format(time.RFC3339))

	log.Println("\n==============================================")
	log.Println("Both workflows are now running.")
	log.Println("The reminder will be sent after 5 seconds.")
	log.Println("The scheduled task will execute after 8 seconds.")
	log.Println("")
	log.Printf("Check status: go run ./cmd/romancy/ get %s --db %s", reminderInstanceID, dbPath)
	log.Println("==============================================")

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in goroutine
	go func() {
		if err := app.ListenAndServe(":8081"); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Println("\nHTTP server running on :8081")
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
