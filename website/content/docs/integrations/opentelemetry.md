---
title: "OpenTelemetry Integration"
weight: 2
---

Romancy provides official integration with [OpenTelemetry](https://opentelemetry.io/), enabling distributed tracing and optional metrics for your durable workflows.

## Overview

OpenTelemetry is an industry-standard observability framework. Romancy's OpenTelemetry integration provides:

- **Distributed Tracing**: Workflow and activity spans with parent-child relationships
- **Optional Metrics**: Counters for workflow/activity execution, histograms for duration
- **W3C Trace Context**: Propagate traces across service boundaries via CloudEvents
- **Automatic Context Inheritance**: Inherit from HTTP middleware or CloudEvents headers

## Installation

Install the OpenTelemetry integration package:

```bash
go get github.com/i2y/romancy/integrations/opentelemetry
go get go.opentelemetry.io/otel
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
	otelromancy "github.com/i2y/romancy/integrations/opentelemetry"
	"go.opentelemetry.io/otel"
)

// Activities

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		return map[string]any{"reserved": true}, nil
	},
)

// Workflow

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		_, err := reserveInventory.Execute(ctx, orderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "completed"}, nil
	},
)

func main() {
	// Create OpenTelemetry hooks (console exporter for development)
	hooks := otelromancy.NewHooks(
		otelromancy.WithServiceName("order-service"),
		otelromancy.WithConsoleExporter(), // For development
		otelromancy.WithMetrics(false),
	)
	defer hooks.Shutdown(context.Background())

	// Or with OTLP exporter for production (Jaeger, Tempo, etc.)
	// hooks := otelromancy.NewHooks(
	//     otelromancy.WithServiceName("order-service"),
	//     otelromancy.WithOTLPEndpoint("http://localhost:4317"),
	//     otelromancy.WithMetrics(true),
	// )

	// Create Romancy app with hooks
	app := romancy.NewApp(
		romancy.WithDatabase("workflow.db"),
		romancy.WithWorkerID("worker-1"),
		romancy.WithHooks(hooks),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		panic(err)
	}
	defer app.Shutdown(ctx)

	// Start workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, orderWorkflow, "ORD-123")
	if err != nil {
		panic(err)
	}

	fmt.Printf("Workflow started: %s\n", instanceID)
}
```

## Span Hierarchy

Romancy creates a hierarchical span structure:

```
workflow:order_workflow (parent)
├── activity:reserve_inventory:1 (child)
├── activity:process_payment:1 (child)
└── activity:ship_order:1 (child)
```

## Span Attributes

**Workflow Spans**:

- `romancy.workflow.instance_id`
- `romancy.workflow.name`
- `romancy.workflow.cancelled` (when cancelled)

**Activity Spans**:

- `romancy.activity.id` (e.g., "reserve_inventory:1")
- `romancy.activity.name`
- `romancy.activity.is_replaying`
- `romancy.activity.cache_hit`

## Metrics (Optional)

When `WithMetrics(true)`:

| Metric | Type | Description |
|--------|------|-------------|
| `romancy.workflow.started` | Counter | Workflows started |
| `romancy.workflow.completed` | Counter | Workflows completed |
| `romancy.workflow.failed` | Counter | Workflows failed |
| `romancy.workflow.duration` | Histogram | Workflow execution time |
| `romancy.activity.executed` | Counter | Activities executed |
| `romancy.activity.cache_hit` | Counter | Activity cache hits |
| `romancy.activity.duration` | Histogram | Activity execution time |

## Trace Context Propagation

### Automatic Context Inheritance

OpenTelemetry hooks automatically inherit trace context from multiple sources, with the following priority:

1. **Explicit trace context in input data** (highest priority)

   - Extracted from CloudEvents extension attributes
   - Useful for cross-service trace propagation

2. **Current active span** (e.g., from HTTP middleware)

   - Automatically detected using `otel.GetTracerProvider()`
   - Works with OpenTelemetry instrumentation middleware

3. **New root span** (if no parent context is found)

### CloudEvents Integration

Inject trace context when sending events:

```go
package main

import (
	"context"

	"github.com/i2y/romancy"
	otelromancy "github.com/i2y/romancy/integrations/opentelemetry"
)

func sendEventWithTrace(ctx *romancy.WorkflowContext, hooks *otelromancy.Hooks) error {
	eventData := map[string]any{
		"order_id": "ORD-123",
	}

	// Inject trace context
	eventData = otelromancy.InjectTraceContext(hooks, ctx.InstanceID(), eventData)

	return romancy.SendEventTransactional(ctx,
		"order.shipped",
		"orders",
		eventData,
	)
}
```

When a CloudEvent contains W3C Trace Context extension attributes (`traceparent`, `tracestate`), they are automatically extracted and used as the parent context:

```bash
# CloudEvent with trace context
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/json" \
  -H "ce-specversion: 1.0" \
  -H "ce-type: order.created" \
  -H "ce-source: external-service" \
  -H "ce-id: event-123" \
  -H "ce-traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01" \
  -d '{"order_id": "ORD-123"}'
```

### HTTP Middleware

OpenTelemetry hooks automatically inherit from the current active span:

```go
package main

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	// Middleware creates parent span for each HTTP request
	handler := otelhttp.NewHandler(yourHandler, "server")

	// Workflow spans automatically inherit from the request span
	http.ListenAndServe(":8001", handler)
}
```

### Existing TracerProvider Reuse

If a TracerProvider is already configured (e.g., by HTTP middleware or your application), OpenTelemetry hooks will reuse it instead of creating a new one:

```go
package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	otelromancy "github.com/i2y/romancy/integrations/opentelemetry"
)

func main() {
	// Configure your own provider
	provider := trace.NewTracerProvider(
		trace.WithResource(myResource),
	)
	otel.SetTracerProvider(provider)

	// OpenTelemetry hooks will use the existing provider
	hooks := otelromancy.NewHooks(
		otelromancy.WithServiceName("my-service"),
	)
	// No new provider is created!
}
```

## Complete Example

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/i2y/romancy"
	otelromancy "github.com/i2y/romancy/integrations/opentelemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Activities

var validateOrder = romancy.DefineActivity("validate_order",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		time.Sleep(100 * time.Millisecond)
		return map[string]any{"valid": true}, nil
	},
)

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, amount float64) (map[string]any, error) {
		time.Sleep(500 * time.Millisecond)
		return map[string]any{"transaction_id": "TXN-123"}, nil
	},
)

// Workflow

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string, amount float64) (map[string]any, error) {
		_, err := validateOrder.Execute(ctx, orderID)
		if err != nil {
			return nil, err
		}

		paymentResult, err := processPayment.Execute(ctx, amount)
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"status":         "completed",
			"transaction_id": paymentResult["transaction_id"],
		}, nil
	},
)

func main() {
	// Create OpenTelemetry hooks
	hooks := otelromancy.NewHooks(
		otelromancy.WithServiceName("order-service"),
		otelromancy.WithOTLPEndpoint("http://localhost:4317"),
		otelromancy.WithMetrics(true),
	)
	defer hooks.Shutdown(context.Background())

	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("worker-1"),
		romancy.WithHooks(hooks),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// HTTP handler with OpenTelemetry middleware
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instanceID, err := romancy.StartWorkflow(r.Context(), app, orderWorkflow, OrderInput{OrderID: "ORD-123", Amount: 99.99})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "Workflow started: %s\n", instanceID)
	})

	// Wrap with OpenTelemetry HTTP middleware
	wrappedHandler := otelhttp.NewHandler(handler, "order-endpoint")

	fmt.Println("Server running on http://localhost:8001")
	fmt.Println("Traces exported to http://localhost:4317")
	log.Fatal(http.ListenAndServe(":8001", wrappedHandler))
}
```

## Configuration Options

```go
hooks := otelromancy.NewHooks(
	// Service name (required)
	otelromancy.WithServiceName("my-service"),

	// OTLP endpoint (production)
	otelromancy.WithOTLPEndpoint("http://localhost:4317"),

	// Console exporter (development)
	otelromancy.WithConsoleExporter(),

	// Enable/disable metrics
	otelromancy.WithMetrics(true),

	// Custom resource attributes
	otelromancy.WithResourceAttributes(map[string]string{
		"environment": "production",
		"version":     "1.0.0",
	}),
)
```

## Related Documentation

- [Lifecycle Hooks](/docs/core-features/hooks) - Detailed hooks documentation
- [OpenTelemetry Documentation](https://opentelemetry.io/docs/)
- [OpenTelemetry Go SDK](https://opentelemetry.io/docs/languages/go/)
