// Package main demonstrates a simple order processing workflow using Romancy.
//
// This example shows:
// - Workflow and Activity definitions
// - Compensation functions (Saga pattern)
// - Event waiting (payment confirmation)
// - Type-safe event handling
//
// Usage:
//
//	go run ./examples/demo/
//
// Then use the CLI to interact:
//
//	go run ./cmd/romancy/ get <instance_id>
//	go run ./cmd/romancy/ event <instance_id> payment.completed '{"transaction_id":"TX-123"}'
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

// OrderInput is the input for the order workflow.
type OrderInput struct {
	OrderID    string  `json:"order_id"`
	CustomerID string  `json:"customer_id"`
	Amount     float64 `json:"amount"`
}

// OrderResult is the output of the order workflow.
type OrderResult struct {
	OrderID       string `json:"order_id"`
	Status        string `json:"status"`
	ReservationID string `json:"reservation_id,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	ShipmentID    string `json:"shipment_id,omitempty"`
}

// PaymentCompleted is the event data for payment completion.
type PaymentCompleted struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount,omitempty"`
}

// ----- Activities -----

// ReserveInventory reserves inventory for an order.
var ReserveInventory = romancy.DefineActivity(
	"reserve_inventory",
	func(ctx context.Context, orderID string) (string, error) {
		log.Printf("[Activity] Reserving inventory for order %s", orderID)
		// Simulate some work
		time.Sleep(100 * time.Millisecond)
		reservationID := fmt.Sprintf("RES-%s-%d", orderID, time.Now().UnixNano()%10000)
		log.Printf("[Activity] Inventory reserved: %s", reservationID)
		return reservationID, nil
	},
	romancy.WithCompensation[string, string](func(ctx context.Context, orderID string) error {
		log.Printf("[Compensation] Releasing inventory for order %s", orderID)
		// In a real system, this would release the reserved inventory
		return nil
	}),
)

// ProcessShipment processes the shipment for an order.
var ProcessShipment = romancy.DefineActivity(
	"process_shipment",
	func(ctx context.Context, orderID string) (string, error) {
		log.Printf("[Activity] Processing shipment for order %s", orderID)
		time.Sleep(100 * time.Millisecond)
		shipmentID := fmt.Sprintf("SHIP-%s-%d", orderID, time.Now().UnixNano()%10000)
		log.Printf("[Activity] Shipment processed: %s", shipmentID)
		return shipmentID, nil
	},
)

// ----- Workflow -----

// orderWorkflow processes an order through the complete lifecycle.
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderResult, error) {
		result := OrderResult{
			OrderID: input.OrderID,
			Status:  "processing",
		}

		// Step 1: Reserve inventory
		log.Printf("[Workflow] Step 1: Reserving inventory for order %s", input.OrderID)
		reservationID, err := ReserveInventory.Execute(ctx, input.OrderID)
		if err != nil {
			result.Status = "failed"
			return result, fmt.Errorf("failed to reserve inventory: %w", err)
		}
		result.ReservationID = reservationID

		// Step 2: Wait for payment confirmation
		log.Printf("[Workflow] Step 2: Waiting for payment confirmation...")
		log.Printf("[Workflow] Send payment event with: romancy event %s payment.completed '{\"transaction_id\":\"TX-123\"}'", ctx.InstanceID())

		event, err := romancy.WaitEvent[PaymentCompleted](ctx, "payment.completed", romancy.WithEventTimeout(5*time.Minute))
		if err != nil {
			// If waiting for event, the engine will suspend the workflow
			// and resume it when the event arrives
			result.Status = "waiting_for_payment"
			return result, err
		}
		result.TransactionID = event.Data.TransactionID
		log.Printf("[Workflow] Payment received: %s", event.Data.TransactionID)

		// Step 3: Process shipment
		log.Printf("[Workflow] Step 3: Processing shipment...")
		shipmentID, err := ProcessShipment.Execute(ctx, input.OrderID)
		if err != nil {
			result.Status = "shipment_failed"
			return result, fmt.Errorf("failed to process shipment: %w", err)
		}
		result.ShipmentID = shipmentID

		result.Status = "completed"
		log.Printf("[Workflow] Order %s completed successfully!", input.OrderID)
		return result, nil
	},
)

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil // No tracing configured
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-demo"
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
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
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
		dbPath = "romancy_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbPath),
		romancy.WithWorkerID("demo-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register the workflow
	romancy.RegisterWorkflow[OrderInput, OrderResult](app, orderWorkflow)

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}

	log.Println("==============================================")
	log.Println("Romancy Demo Application Started")
	log.Printf("Database: %s", dbPath)
	log.Println("==============================================")

	// Start an example workflow
	orderInput := OrderInput{
		OrderID:    fmt.Sprintf("ORD-%d", time.Now().Unix()),
		CustomerID: "CUST-001",
		Amount:     99.99,
	}

	instanceID, err := romancy.StartWorkflow(ctx, app, orderWorkflow, orderInput)
	if err != nil {
		log.Printf("Failed to start workflow: %v", err)
		return
	}

	log.Println("")
	log.Printf("✓ Workflow started with instance ID: %s", instanceID)
	log.Println("")
	log.Println("Commands to interact:")
	log.Printf("  Get status:    go run ./cmd/romancy/ get %s --db %s", instanceID, dbPath)
	log.Printf("  Send payment:  go run ./cmd/romancy/ event %s payment.completed '{\"transaction_id\":\"TX-123\"}' --db %s", instanceID, dbPath)
	log.Println("")
	log.Println("HTTP endpoints:")
	log.Println("  Health:  GET  http://localhost:8080/health/live")
	log.Println("  Events:  POST http://localhost:8080/ (CloudEvents)")
	log.Println("  Cancel:  POST http://localhost:8080/cancel/{instance_id}")
	log.Println("")
	log.Println("Starting HTTP server on :8080...")
	log.Println("Press Ctrl+C to stop")

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in goroutine
	go func() {
		if err := app.ListenAndServe(":8080"); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

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
