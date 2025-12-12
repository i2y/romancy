// Package main demonstrates MCP (Model Context Protocol) integration with Romancy.
//
// This example shows how to expose workflows as MCP tools for AI assistants
// like Claude Desktop.
//
// The example creates an order processing workflow and exposes it via MCP with
// four auto-generated tools:
//   - order_workflow_start: Start a new order processing workflow
//   - order_workflow_status: Get the current status of an order workflow
//   - order_workflow_result: Get the result of a completed order workflow
//   - order_workflow_cancel: Cancel a running order workflow
//
// Usage:
//
//	# Run as MCP server (stdio transport for Claude Desktop)
//	go run ./examples/mcp/
//
//	# Configure in Claude Desktop settings:
//	# {
//	#   "mcpServers": {
//	#     "order-service": {
//	#       "command": "go",
//	#       "args": ["run", "./examples/mcp/"]
//	#     }
//	#   }
//	# }
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/hooks/otel"
	"github.com/i2y/romancy/mcp"
)

// ----- Types -----

// OrderInput is the input for the order workflow.
type OrderInput struct {
	OrderID    string  `json:"order_id" jsonschema:"The unique order ID"`
	CustomerID string  `json:"customer_id" jsonschema:"The customer ID placing the order"`
	Amount     float64 `json:"amount" jsonschema:"The total order amount in dollars"`
	Items      []Item  `json:"items" jsonschema:"List of items in the order"`
}

// Item represents a single order item.
type Item struct {
	ProductID string  `json:"product_id" jsonschema:"The product ID"`
	Quantity  int     `json:"quantity" jsonschema:"Quantity ordered"`
	Price     float64 `json:"price" jsonschema:"Unit price in dollars"`
}

// OrderResult is the output of the order workflow.
type OrderResult struct {
	OrderID      string  `json:"order_id" jsonschema:"The order ID"`
	Status       string  `json:"status" jsonschema:"Final order status (confirmed, shipped, etc.)"`
	Total        float64 `json:"total" jsonschema:"Total amount charged"`
	Confirmation string  `json:"confirmation" jsonschema:"Order confirmation number"`
	ShippingETA  string  `json:"shipping_eta" jsonschema:"Expected shipping date"`
}

// ----- Activities -----

// ValidateOrder validates the order input.
var ValidateOrder = romancy.DefineActivity(
	"validate_order",
	func(ctx context.Context, input OrderInput) (bool, error) {
		log.Printf("[Activity] Validating order %s", input.OrderID)
		time.Sleep(100 * time.Millisecond) // Simulate processing

		// Basic validation
		if input.Amount <= 0 {
			return false, fmt.Errorf("invalid order amount: %.2f", input.Amount)
		}
		if len(input.Items) == 0 {
			return false, fmt.Errorf("order has no items")
		}

		return true, nil
	},
)

// ProcessPayment processes the payment for an order.
var ProcessPayment = romancy.DefineActivity(
	"process_payment",
	func(ctx context.Context, input struct {
		OrderID string
		Amount  float64
	}) (string, error) {
		log.Printf("[Activity] Processing payment for order %s: $%.2f", input.OrderID, input.Amount)
		time.Sleep(150 * time.Millisecond) // Simulate payment processing

		// Generate confirmation number
		confirmation := fmt.Sprintf("CONF-%s-%d", input.OrderID, time.Now().UnixNano()%10000)
		return confirmation, nil
	},
)

// ScheduleShipping schedules the order for shipping.
var ScheduleShipping = romancy.DefineActivity(
	"schedule_shipping",
	func(ctx context.Context, orderID string) (string, error) {
		log.Printf("[Activity] Scheduling shipping for order %s", orderID)
		time.Sleep(100 * time.Millisecond) // Simulate scheduling

		// Calculate ETA (3-5 business days from now)
		eta := time.Now().AddDate(0, 0, 4).Format("2006-01-02")
		return eta, nil
	},
)

// ----- Workflow -----

// OrderWorkflow processes a customer order.
type OrderWorkflow struct{}

func (w *OrderWorkflow) Name() string { return "order_workflow" }

func (w *OrderWorkflow) Execute(ctx *romancy.WorkflowContext, input OrderInput) (OrderResult, error) {
	log.Printf("[Workflow] Processing order %s for customer %s", input.OrderID, input.CustomerID)

	result := OrderResult{
		OrderID: input.OrderID,
		Status:  "processing",
		Total:   input.Amount,
	}

	// Step 1: Validate the order
	log.Printf("[Workflow] Step 1: Validating order...")
	valid, err := ValidateOrder.Execute(ctx, input)
	if err != nil || !valid {
		result.Status = "validation_failed"
		return result, fmt.Errorf("order validation failed: %w", err)
	}

	// Step 2: Process payment
	log.Printf("[Workflow] Step 2: Processing payment...")
	confirmation, err := ProcessPayment.Execute(ctx, struct {
		OrderID string
		Amount  float64
	}{
		OrderID: input.OrderID,
		Amount:  input.Amount,
	})
	if err != nil {
		result.Status = "payment_failed"
		return result, fmt.Errorf("payment processing failed: %w", err)
	}
	result.Confirmation = confirmation

	// Step 3: Schedule shipping
	log.Printf("[Workflow] Step 3: Scheduling shipping...")
	eta, err := ScheduleShipping.Execute(ctx, input.OrderID)
	if err != nil {
		result.Status = "shipping_failed"
		return result, fmt.Errorf("shipping scheduling failed: %w", err)
	}
	result.ShippingETA = eta

	result.Status = "confirmed"
	log.Printf("[Workflow] Order %s completed! Confirmation: %s, ETA: %s",
		input.OrderID, result.Confirmation, result.ShippingETA)

	return result, nil
}

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil // No tracing configured
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-mcp"
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
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "mcp_demo.db"
	}

	// Create the Romancy application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbURL),
		romancy.WithWorkerID("mcp-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Create the MCP server
	server := mcp.NewServer(app,
		mcp.WithServerName("order-service"),
		mcp.WithServerVersion("1.0.0"),
	)

	// Register the order workflow
	// This auto-generates 4 MCP tools:
	// - order_workflow_start
	// - order_workflow_status
	// - order_workflow_result
	// - order_workflow_cancel
	mcp.RegisterWorkflow[OrderInput, OrderResult](server, &OrderWorkflow{},
		mcp.WithDescription("Process customer orders with payment and shipping"),
	)

	// Initialize the server (starts the Romancy app)
	if err := server.Initialize(ctx); err != nil {
		log.Printf("Failed to initialize server: %v", err)
		return
	}
	defer func() { _ = server.Shutdown(ctx) }()

	log.Println("==============================================")
	log.Println("Order Service MCP Server")
	log.Println("==============================================")
	log.Println("")
	log.Println("Available tools:")
	log.Println("  - order_workflow_start: Start a new order")
	log.Println("  - order_workflow_status: Get order status")
	log.Println("  - order_workflow_result: Get order result")
	log.Println("  - order_workflow_cancel: Cancel an order")
	log.Println("")
	log.Println("Running on stdio transport...")
	log.Println("==============================================")

	// Run the MCP server on stdio transport
	if err := server.RunStdio(ctx); err != nil {
		log.Printf("Server error: %v", err)
		return
	}
}
