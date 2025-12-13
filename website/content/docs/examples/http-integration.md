---
title: "HTTP Integration"
weight: 5
---

This example demonstrates how to integrate Romancy with a Go HTTP server for building workflow-powered APIs.

## What This Example Shows

- ✅ HTTP server setup with proper lifecycle management
- ✅ REST API endpoints for workflow invocation
- ✅ CloudEvents integration for event-driven workflows
- ✅ Direct workflow invocation patterns
- ✅ Background task management

## Basic Setup

### Define Your Workflow

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/i2y/romancy"
)

// OrderInput represents the order request
type OrderInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

func (o OrderInput) Validate() error {
	if o.OrderID == "" {
		return fmt.Errorf("order_id is required")
	}
	if o.Amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	return nil
}

// OrderResult represents the order result
type OrderResult struct {
	OrderID   string `json:"order_id"`
	Status    string `json:"status"`
	ProcessedAt string `json:"processed_at"`
}

// Activity result types

type ValidationResult struct {
	Valid bool `json:"valid"`
}

type PaymentResult struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
}

// Activities

var validateOrder = romancy.DefineActivity("validate_order",
	func(ctx context.Context, order OrderInput) (ValidationResult, error) {
		if err := order.Validate(); err != nil {
			return ValidationResult{}, err
		}
		return ValidationResult{Valid: true}, nil
	},
)

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string, amount float64) (PaymentResult, error) {
		fmt.Printf("Processing payment of $%.2f for order %s\n", amount, orderID)
		return PaymentResult{
			TransactionID: fmt.Sprintf("TXN-%s", orderID),
			Status:        "completed",
		}, nil
	},
)

// Workflow

var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		// Validate order
		_, err := validateOrder.Execute(ctx, order)
		if err != nil {
			return OrderResult{}, err
		}

		// Process payment
		_, err = processPayment.Execute(ctx, order.OrderID, order.Amount)
		if err != nil {
			return OrderResult{}, err
		}

		return OrderResult{
			OrderID:     order.OrderID,
			Status:      "completed",
			ProcessedAt: time.Now().Format(time.RFC3339),
		}, nil
	},
)
```

### Create HTTP Server

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/i2y/romancy"
)

func main() {
	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("http-server"),
	)

	ctx := context.Background()

	// Initialize the app
	if err := app.Initialize(ctx); err != nil {
		log.Fatal(err)
	}

	// Create HTTP handlers
	mux := http.NewServeMux()

	// Order creation endpoint
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var order OrderInput
		if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Start workflow (non-blocking)
		instanceID, err := processOrder.Start(r.Context(), app, order)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"instance_id": instanceID,
			"status":      "accepted",
		})
	})

	// Get workflow status endpoint
	mux.HandleFunc("GET /orders/{instance_id}", func(w http.ResponseWriter, r *http.Request) {
		instanceID := r.PathValue("instance_id")

		instance, err := app.GetInstance(r.Context(), instanceID)
		if err != nil {
			http.Error(w, "Instance not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(instance)
	})

	// Create server
	server := &http.Server{
		Addr:    ":8001",
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		fmt.Println("\nShutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server.Shutdown(shutdownCtx)
		app.Shutdown(shutdownCtx)
	}()

	fmt.Println("Server running on http://localhost:8001")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
```

## CloudEvents Integration

### Mount CloudEvents Handler

Romancy provides a CloudEvents handler that can be mounted on your HTTP server:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/i2y/romancy"
)

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("events.db"),
		romancy.WithWorkerID("cloudevents-server"),
	)

	ctx := context.Background()
	if err := app.Initialize(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	mux := http.NewServeMux()

	// Your REST endpoints
	mux.HandleFunc("POST /orders", handleCreateOrder(app))
	mux.HandleFunc("GET /orders/{id}", handleGetOrder(app))

	// Mount CloudEvents handler at root
	mux.HandleFunc("POST /", app.HandleCloudEvent)

	fmt.Println("Server running on http://localhost:8001")
	log.Fatal(http.ListenAndServe(":8001", mux))
}

func handleCreateOrder(app *romancy.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var order OrderInput
		if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		instanceID, err := processOrder.Start(r.Context(), app, order)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"instance_id": instanceID,
		})
	}
}

func handleGetOrder(app *romancy.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		instanceID := r.PathValue("id")
		instance, err := app.GetInstance(r.Context(), instanceID)
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(instance)
	}
}
```

### Event-Driven Workflow

Create a workflow that waits for external events:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/i2y/romancy"
)

type PaymentStartResult struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
}

type PaymentWorkflowResult struct {
	Status        string  `json:"status"`
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

type PaymentCompleted struct {
	TransactionID string  `json:"transaction_id"`
	Amount        float64 `json:"amount"`
}

var startPayment = romancy.DefineActivity("start_payment",
	func(ctx context.Context, orderID string) (PaymentStartResult, error) {
		fmt.Printf("Starting payment for order %s\n", orderID)
		return PaymentStartResult{
			PaymentID: fmt.Sprintf("PAY-%s", orderID),
			Status:    "pending",
		}, nil
	},
)

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (PaymentWorkflowResult, error) {
		// Start payment process
		_, err := startPayment.Execute(ctx, orderID)
		if err != nil {
			return PaymentWorkflowResult{}, err
		}

		// Wait for payment confirmation event (with timeout)
		event, err := romancy.WaitEvent(ctx, "payment.completed",
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return PaymentWorkflowResult{}, fmt.Errorf("payment timeout: %w", err)
		}

		// Parse event data
		var payment PaymentCompleted
		if err := romancy.DecodeEventData(event.Data, &payment); err != nil {
			return PaymentWorkflowResult{}, err
		}

		return PaymentWorkflowResult{
			Status:        "completed",
			TransactionID: payment.TransactionID,
			Amount:        payment.Amount,
		}, nil
	},
)
```

Send a CloudEvent to resume the workflow:

```bash
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123",
    "data": {
      "order_id": "ORD-123",
      "transaction_id": "TXN-456",
      "amount": 99.99
    }
  }'
```

## Direct vs Event-Driven Invocation

### Direct Invocation (REST API)

Best for synchronous request-response patterns:

```go
// Client sends HTTP request
// POST /orders {"order_id": "123", "amount": 99.99}

// Handler starts workflow directly
instanceID, err := processOrder.Start(r.Context(), app, order)

// Return instance ID immediately (non-blocking)
// {"instance_id": "wf-abc123", "status": "accepted"}

// Client polls for result
// GET /orders/wf-abc123
```

### Event-Driven Invocation (CloudEvents)

Best for event-driven and async patterns:

```go
// External service sends CloudEvent
// POST / with CloudEvent payload

// Romancy handles the event automatically:
// 1. If event starts a new workflow → start workflow
// 2. If event resumes a waiting workflow → resume it

// Workflow waits for events
event, err := romancy.WaitEvent(ctx, "payment.completed")
```

## Database Configuration

### SQLite (Development)

```go
app := romancy.NewApp(
	romancy.WithDatabase("workflow.db"),
	romancy.WithWorkerID("worker-1"),
)
```

### PostgreSQL (Production)

```go
app := romancy.NewApp(
	romancy.WithDatabase("postgres://user:pass@localhost:5432/workflows"),
	romancy.WithWorkerID("worker-1"),
)
```

### MySQL

```go
app := romancy.NewApp(
	romancy.WithDatabase("mysql://user:pass@localhost:3306/workflows"),
	romancy.WithWorkerID("worker-1"),
)
```

## Best Practices

### 1. Return Instance ID Immediately

Don't wait for workflow completion in HTTP handlers:

```go
// ✅ Good: Non-blocking, returns immediately
mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
	instanceID, err := workflow.Start(r.Context(), app, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"instance_id": instanceID,
	})
})

// ❌ Bad: Blocks until workflow completes
mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
	result, err := workflow.StartAndWait(r.Context(), app, input) // Don't do this!
	// ... HTTP timeout risk
})
```

### 2. Use PostgreSQL for Production

```go
// ✅ Production: PostgreSQL with connection pool
app := romancy.NewApp(
	romancy.WithDatabase("postgres://user:pass@localhost:5432/workflows"),
	romancy.WithWorkerID(hostname()),
)

// ⚠️ Development only: SQLite
app := romancy.NewApp(
	romancy.WithDatabase("dev.db"),
	romancy.WithWorkerID("dev-worker"),
)
```

### 3. Proper Lifecycle Management

```go
func main() {
	app := romancy.NewApp(...)
	ctx := context.Background()

	// Initialize before serving
	if err := app.Initialize(ctx); err != nil {
		log.Fatal(err)
	}

	// Shutdown on termination
	defer app.Shutdown(ctx)

	// Or use signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		app.Shutdown(ctx)
		os.Exit(0)
	}()

	log.Fatal(http.ListenAndServe(":8001", mux))
}
```

### 4. Use Struct-Based Input/Output

```go
// ✅ Good: Type-safe structs
type OrderInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		// Type-safe access
		return OrderResult{...}, nil
	},
)

// ❌ Avoid: map[string]any for complex data
var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, data map[string]any) (map[string]any, error) {
		// Type assertions required
		return nil, nil
	},
)
```

## Running the Example

### Create Complete Example

Create a file named `http_server.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/i2y/romancy"
)

// Input/Output types

type OrderInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

type OrderResult struct {
	OrderID     string `json:"order_id"`
	Status      string `json:"status"`
	ProcessedAt string `json:"processed_at"`
}

// Activity result types

type ValidationResult struct {
	Valid bool `json:"valid"`
}

type PaymentResult struct {
	TransactionID string `json:"transaction_id"`
}

// Activities

var validateOrder = romancy.DefineActivity("validate_order",
	func(ctx context.Context, order OrderInput) (ValidationResult, error) {
		if order.OrderID == "" {
			return ValidationResult{}, fmt.Errorf("order_id required")
		}
		return ValidationResult{Valid: true}, nil
	},
)

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string, amount float64) (PaymentResult, error) {
		fmt.Printf("Processing $%.2f for %s\n", amount, orderID)
		return PaymentResult{TransactionID: fmt.Sprintf("TXN-%s", orderID)}, nil
	},
)

// Workflow

var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		_, err := validateOrder.Execute(ctx, order)
		if err != nil {
			return OrderResult{}, err
		}

		_, err = processPayment.Execute(ctx, order.OrderID, order.Amount)
		if err != nil {
			return OrderResult{}, err
		}

		return OrderResult{
			OrderID:     order.OrderID,
			Status:      "completed",
			ProcessedAt: time.Now().Format(time.RFC3339),
		}, nil
	},
)

func main() {
	fmt.Println("============================================================")
	fmt.Println("Romancy Framework - HTTP Server Integration Example")
	fmt.Println("============================================================")
	fmt.Println()

	app := romancy.NewApp(
		romancy.WithDatabase("http_demo.db"),
		romancy.WithWorkerID("http-server"),
	)

	ctx := context.Background()

	if err := app.Initialize(ctx); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// Create order
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var order OrderInput
		if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		instanceID, err := processOrder.Start(r.Context(), app, order)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"instance_id": instanceID,
			"status":      "accepted",
		})
	})

	// Get order status
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		instance, err := app.GetInstance(r.Context(), id)
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(instance)
	})

	// CloudEvents handler
	mux.HandleFunc("POST /", app.HandleCloudEvent)

	// Graceful shutdown
	server := &http.Server{Addr: ":8001", Handler: mux}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		fmt.Println("\nShutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server.Shutdown(shutdownCtx)
		app.Shutdown(shutdownCtx)
	}()

	fmt.Println(">>> Server running on http://localhost:8001")
	fmt.Println(">>> Endpoints:")
	fmt.Println("    POST /orders        - Create order")
	fmt.Println("    GET  /orders/{id}   - Get order status")
	fmt.Println("    POST /              - CloudEvents endpoint")
	fmt.Println()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
```

### Run the Server

```bash
# Initialize Go module
go mod init http-example
go get github.com/i2y/romancy

# Run the server
go run http_server.go
```

### Test the API

```bash
# Create an order
curl -X POST http://localhost:8001/orders \
  -H "Content-Type: application/json" \
  -d '{"order_id": "ORD-123", "amount": 99.99}'

# Response: {"instance_id": "wf-xxx", "status": "accepted"}

# Get order status
curl http://localhost:8001/orders/wf-xxx
```

## What You Learned

- ✅ **HTTP Server Setup** with proper lifecycle management
- ✅ **REST Endpoints** for workflow invocation
- ✅ **CloudEvents Integration** for event-driven workflows
- ✅ **Direct vs Event-Driven** invocation patterns
- ✅ **Best Practices** for production deployments

## Next Steps

- **[Simple Workflow](/docs/examples/simple)**: Basic workflow example
- **[Event Waiting](/docs/examples/events)**: Deep dive into event-driven workflows
- **[CloudEvents HTTP Binding](/docs/core-features/events/cloudevents-http-binding)**: CloudEvents specification compliance
