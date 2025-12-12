// Package main demonstrates event-driven workflow patterns.
//
// This example shows:
// - WithEventHandler for automatic workflow start on CloudEvents
// - Event-driven microservice patterns
// - Multi-step event processing
//
// Usage:
//
//	go run ./examples/event_handler/
//
// Then send events:
//
//	curl -X POST http://localhost:8082/ \
//	  -H "Content-Type: application/json" \
//	  -d '{
//	    "specversion": "1.0",
//	    "type": "user.signup",
//	    "source": "auth-service",
//	    "id": "evt-123",
//	    "data": {"user_id": "user-001", "email": "alice@example.com", "name": "Alice"}
//	  }'
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

// UserSignupEvent represents a user signup event.
type UserSignupEvent struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

// UserSignupResult is the result of processing a user signup.
type UserSignupResult struct {
	UserID           string `json:"user_id"`
	WelcomeEmailSent bool   `json:"welcome_email_sent"`
	ProfileCreated   bool   `json:"profile_created"`
	AnalyticsTracked bool   `json:"analytics_tracked"`
	Status           string `json:"status"`
}

// OrderCreatedEvent represents an order created event.
type OrderCreatedEvent struct {
	OrderID    string  `json:"order_id"`
	CustomerID string  `json:"customer_id"`
	Amount     float64 `json:"amount"`
	Items      []struct {
		ProductID string `json:"product_id"`
		Quantity  int    `json:"quantity"`
	} `json:"items"`
}

// OrderProcessingResult is the result of processing an order.
type OrderProcessingResult struct {
	OrderID          string `json:"order_id"`
	InventoryChecked bool   `json:"inventory_checked"`
	PaymentProcessed bool   `json:"payment_processed"`
	NotificationSent bool   `json:"notification_sent"`
	Status           string `json:"status"`
}

// ----- Activities -----

// SendWelcomeEmail sends a welcome email to a new user.
var SendWelcomeEmail = romancy.DefineActivity(
	"send_welcome_email",
	func(ctx context.Context, input struct {
		Email string
		Name  string
	}) (bool, error) {
		log.Printf("[Activity] Sending welcome email to %s <%s>", input.Name, input.Email)
		time.Sleep(100 * time.Millisecond) // Simulate sending email
		log.Printf("[Activity] Welcome email sent successfully")
		return true, nil
	},
)

// CreateUserProfile creates a user profile in the database.
var CreateUserProfile = romancy.DefineActivity(
	"create_user_profile",
	func(ctx context.Context, input struct {
		UserID string
		Name   string
		Email  string
	}) (bool, error) {
		log.Printf("[Activity] Creating profile for user %s", input.UserID)
		time.Sleep(100 * time.Millisecond) // Simulate database operation
		log.Printf("[Activity] Profile created successfully")
		return true, nil
	},
)

// TrackUserSignup tracks the signup event for analytics.
var TrackUserSignup = romancy.DefineActivity(
	"track_user_signup",
	func(ctx context.Context, userID string) (bool, error) {
		log.Printf("[Activity] Tracking signup analytics for user %s", userID)
		time.Sleep(50 * time.Millisecond) // Simulate analytics call
		log.Printf("[Activity] Analytics tracked successfully")
		return true, nil
	},
)

// CheckInventory checks inventory availability.
var CheckInventory = romancy.DefineActivity(
	"check_inventory",
	func(ctx context.Context, orderID string) (bool, error) {
		log.Printf("[Activity] Checking inventory for order %s", orderID)
		time.Sleep(100 * time.Millisecond) // Simulate inventory check
		log.Printf("[Activity] Inventory available")
		return true, nil
	},
)

// ProcessOrderPayment processes payment for an order.
var ProcessOrderPayment = romancy.DefineActivity(
	"process_order_payment",
	func(ctx context.Context, input struct {
		OrderID    string
		CustomerID string
		Amount     float64
	}) (bool, error) {
		log.Printf("[Activity] Processing payment of $%.2f for order %s", input.Amount, input.OrderID)
		time.Sleep(150 * time.Millisecond) // Simulate payment processing
		log.Printf("[Activity] Payment processed successfully")
		return true, nil
	},
)

// SendOrderNotification sends an order confirmation notification.
var SendOrderNotification = romancy.DefineActivity(
	"send_order_notification",
	func(ctx context.Context, input struct {
		OrderID    string
		CustomerID string
	}) (bool, error) {
		log.Printf("[Activity] Sending order confirmation for order %s to customer %s",
			input.OrderID, input.CustomerID)
		time.Sleep(50 * time.Millisecond) // Simulate notification
		log.Printf("[Activity] Notification sent successfully")
		return true, nil
	},
)

// ----- Workflows -----

// UserSignupWorkflow handles new user signups.
// It is triggered automatically when a "user.signup" CloudEvent is received.
type UserSignupWorkflow struct{}

func (w *UserSignupWorkflow) Name() string { return "user.signup" } // Matches CloudEvent type

func (w *UserSignupWorkflow) Execute(ctx *romancy.WorkflowContext, input UserSignupEvent) (UserSignupResult, error) {
	log.Printf("[Workflow] Processing user signup: %s (%s)", input.Name, input.Email)

	result := UserSignupResult{
		UserID: input.UserID,
		Status: "processing",
	}

	// Step 1: Send welcome email
	log.Printf("[Workflow] Step 1: Sending welcome email...")
	emailSent, err := SendWelcomeEmail.Execute(ctx, struct {
		Email string
		Name  string
	}{
		Email: input.Email,
		Name:  input.Name,
	})
	if err != nil {
		result.Status = "email_failed"
		return result, fmt.Errorf("failed to send welcome email: %w", err)
	}
	result.WelcomeEmailSent = emailSent

	// Step 2: Create user profile
	log.Printf("[Workflow] Step 2: Creating user profile...")
	profileCreated, err := CreateUserProfile.Execute(ctx, struct {
		UserID string
		Name   string
		Email  string
	}{
		UserID: input.UserID,
		Name:   input.Name,
		Email:  input.Email,
	})
	if err != nil {
		result.Status = "profile_creation_failed"
		return result, fmt.Errorf("failed to create profile: %w", err)
	}
	result.ProfileCreated = profileCreated

	// Step 3: Track analytics
	log.Printf("[Workflow] Step 3: Tracking analytics...")
	analyticsTracked, err := TrackUserSignup.Execute(ctx, input.UserID)
	if err != nil {
		// Non-critical, continue even if analytics fails
		log.Printf("[Workflow] Analytics tracking failed (non-critical): %v", err)
	}
	result.AnalyticsTracked = analyticsTracked

	result.Status = "completed"
	log.Printf("[Workflow] User signup processing completed for %s", input.UserID)
	return result, nil
}

// OrderCreatedWorkflow handles new order processing.
// It is triggered automatically when an "order.created" CloudEvent is received.
type OrderCreatedWorkflow struct{}

func (w *OrderCreatedWorkflow) Name() string { return "order.created" } // Matches CloudEvent type

func (w *OrderCreatedWorkflow) Execute(ctx *romancy.WorkflowContext, input OrderCreatedEvent) (OrderProcessingResult, error) {
	log.Printf("[Workflow] Processing new order: %s (Customer: %s, Amount: $%.2f)",
		input.OrderID, input.CustomerID, input.Amount)

	result := OrderProcessingResult{
		OrderID: input.OrderID,
		Status:  "processing",
	}

	// Step 1: Check inventory
	log.Printf("[Workflow] Step 1: Checking inventory...")
	inventoryOK, err := CheckInventory.Execute(ctx, input.OrderID)
	if err != nil {
		result.Status = "inventory_check_failed"
		return result, fmt.Errorf("inventory check failed: %w", err)
	}
	result.InventoryChecked = inventoryOK

	// Step 2: Process payment
	log.Printf("[Workflow] Step 2: Processing payment...")
	paymentOK, err := ProcessOrderPayment.Execute(ctx, struct {
		OrderID    string
		CustomerID string
		Amount     float64
	}{
		OrderID:    input.OrderID,
		CustomerID: input.CustomerID,
		Amount:     input.Amount,
	})
	if err != nil {
		result.Status = "payment_failed"
		return result, fmt.Errorf("payment failed: %w", err)
	}
	result.PaymentProcessed = paymentOK

	// Step 3: Send notification
	log.Printf("[Workflow] Step 3: Sending notification...")
	notificationSent, err := SendOrderNotification.Execute(ctx, struct {
		OrderID    string
		CustomerID string
	}{
		OrderID:    input.OrderID,
		CustomerID: input.CustomerID,
	})
	if err != nil {
		// Non-critical
		log.Printf("[Workflow] Notification failed (non-critical): %v", err)
	}
	result.NotificationSent = notificationSent

	result.Status = "completed"
	log.Printf("[Workflow] Order processing completed for %s", input.OrderID)
	return result, nil
}

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-event-handler"
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
		dbPath = "event_handler_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbPath),
		romancy.WithWorkerID("event-handler-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register workflows with event handler option
	// These workflows will be automatically started when matching CloudEvents are received
	romancy.RegisterWorkflow[UserSignupEvent, UserSignupResult](
		app, &UserSignupWorkflow{}, romancy.WithEventHandler(true),
	)
	romancy.RegisterWorkflow[OrderCreatedEvent, OrderProcessingResult](
		app, &OrderCreatedWorkflow{}, romancy.WithEventHandler(true),
	)

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}

	log.Println("==============================================")
	log.Println("Event Handler Workflow Example")
	log.Printf("Database: %s", dbPath)
	log.Println("==============================================")
	log.Println("")
	log.Println("This server automatically starts workflows when CloudEvents are received.")
	log.Println("")
	log.Println("Registered Event Handlers:")
	log.Println("  - user.signup   -> UserSignupWorkflow")
	log.Println("  - order.created -> OrderCreatedWorkflow")
	log.Println("")
	log.Println("Example curl commands:")
	log.Println("")
	log.Println("1. User Signup Event:")
	fmt.Println(`curl -X POST http://localhost:8082/ \`)
	fmt.Println(`     -H "Content-Type: application/json" \`)
	fmt.Println(`     -d '{`)
	fmt.Println(`       "specversion": "1.0",`)
	fmt.Println(`       "type": "user.signup",`)
	fmt.Println(`       "source": "auth-service",`)
	fmt.Println(`       "id": "evt-001",`)
	fmt.Println(`       "data": {"user_id": "user-001", "email": "alice@example.com", "name": "Alice"}`)
	fmt.Println(`     }'`)
	log.Println("")
	log.Println("2. Order Created Event:")
	fmt.Println(`curl -X POST http://localhost:8082/ \`)
	fmt.Println(`     -H "Content-Type: application/json" \`)
	fmt.Println(`     -d '{`)
	fmt.Println(`       "specversion": "1.0",`)
	fmt.Println(`       "type": "order.created",`)
	fmt.Println(`       "source": "order-service",`)
	fmt.Println(`       "id": "evt-002",`)
	fmt.Println(`       "data": {"order_id": "ORD-001", "customer_id": "cust-001", "amount": 99.99, "items": [{"product_id": "P001", "quantity": 2}]}`)
	fmt.Println(`     }'`)
	log.Println("")
	log.Println("==============================================")

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in goroutine
	go func() {
		if err := app.ListenAndServe(":8082"); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Println("HTTP server running on :8082")
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
