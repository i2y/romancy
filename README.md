# Romancy

**Romancy** (from "Romance" - medieval chivalric tales) - Durable execution framework for Go

> Lightweight durable execution framework - no separate server required

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go 1.24+](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](https://go.dev/dl/)
[![GitHub](https://img.shields.io/badge/github-romancy-blue.svg)](https://github.com/i2y/romancy)

## Overview

Romancy is a lightweight durable execution framework for Go that runs as a **library** in your application - no separate workflow server required. It provides automatic crash recovery through deterministic replay, allowing **long-running workflows** to survive process restarts and failures without losing progress.

**Perfect for**: Order processing, distributed transactions (Saga pattern), and any workflow that must survive crashes.

Romancy is a Go port of [Edda](https://github.com/i2y/edda) (Python), providing the same core concepts and patterns in idiomatic Go.

## Key Features

- ✨ **Lightweight Library**: Runs in your application process - no separate server infrastructure
- 🔄 **Durable Execution**: Deterministic replay with workflow history for automatic crash recovery
- 🎯 **Workflow & Activity**: Clear separation between orchestration logic and business logic
- 🔁 **Saga Pattern**: Automatic compensation on failure
- 🌐 **Multi-worker Execution**: Run workflows safely across multiple servers or containers
- 📦 **Transactional Outbox**: Reliable event publishing with guaranteed delivery
- ☁️ **CloudEvents Support**: Native support for CloudEvents protocol
- ⏱️ **Event & Timer Waiting**: Free up worker resources while waiting for events or timers
- 🌍 **net/http Integration**: Use with any Go HTTP router (chi, gorilla/mux, standard library)
- 🔒 **Automatic Transactions**: Activities run in transactions by default with ctx.Session() access
- 🤖 **MCP Integration**: Expose workflows as MCP tools for AI assistants
- 📬 **Channel Messaging**: Broadcast and competing message delivery between workflows
- 🔄 **Recur Pattern**: Erlang-style tail recursion for long-running workflows
- 📡 **PostgreSQL LISTEN/NOTIFY**: Real-time event delivery without polling
- 🤖 **LLM Integration**: Durable LLM calls via bucephalus with automatic caching for replay

## Documentation

📚 **Full documentation**: https://i2y.github.io/romancy/

## Architecture

Romancy runs as a lightweight library in your applications, with all workflow state stored in a shared database:

```
┌─────────────────────────────────────────────────────────────────────┐
│                          Your Go Applications                        │
├──────────────────────┬──────────────────────┬──────────────────────┤
│   order-service-1    │   order-service-2    │   order-service-3    │
│   ┌──────────────┐   │   ┌──────────────┐   │   ┌──────────────┐   │
│   │   Romancy    │   │   │   Romancy    │   │   │   Romancy    │   │
│   │   Workflow   │   │   │   Workflow   │   │   │   Workflow   │   │
│   └──────────────┘   │   └──────────────┘   │   └──────────────┘   │
└──────────┬───────────┴──────────┬───────────┴──────────┬───────────┘
           │                      │                      │
           └──────────────────────┼──────────────────────┘
                                  │
                         ┌────────▼────────┐
                         │ Shared Database │
                         │ (SQLite/PG/MySQL)│
                         └─────────────────┘
```

**Key Points**:

- Multiple workers can run simultaneously across different pods/servers
- Each workflow instance runs on only one worker at a time (automatic coordination)
- `WaitEvent()` and `Sleep()` free up worker resources while waiting
- Automatic crash recovery with stale lock cleanup and workflow auto-resume

## Quick Start

```go
package main

import (
    "context"
    "log"
    "net/http"

    "github.com/i2y/romancy"
)

// Input/Output types
type OrderInput struct {
    OrderID string  `json:"order_id"`
    Amount  float64 `json:"amount"`
}

type OrderResult struct {
    OrderID string         `json:"order_id"`
    Payment map[string]any `json:"payment"`
}

// Define an activity
var processPayment = romancy.DefineActivity("process_payment",
    func(ctx context.Context, amount float64) (map[string]any, error) {
        log.Printf("Processing payment: $%.2f", amount)
        return map[string]any{"status": "paid", "amount": amount}, nil
    },
)

// Define a workflow
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
    func(ctx *romancy.WorkflowContext, input OrderInput) (OrderResult, error) {
        // Activity results are recorded in history
        result, err := processPayment.Execute(ctx, input.Amount)
        if err != nil {
            return OrderResult{}, err
        }
        return OrderResult{OrderID: input.OrderID, Payment: result}, nil
    },
)

func main() {
    ctx := context.Background()

    // Create app with SQLite storage
    app := romancy.NewApp(
        romancy.WithDatabase("workflow.db"),
        romancy.WithWorkerID("order-service"),
    )
    defer app.Shutdown(ctx)

    // Start the application
    if err := app.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Register and start workflow
    romancy.RegisterWorkflow[OrderInput, OrderResult](app, orderWorkflow)

    instanceID, err := romancy.StartWorkflow(ctx, app, orderWorkflow, OrderInput{
        OrderID: "ORD-123",
        Amount:  99.99,
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("Started workflow: %s", instanceID)

    // Serve CloudEvents endpoint
    http.Handle("/", app.Handler())
    log.Fatal(http.ListenAndServe(":8001", nil))
}
```

**What happens on crash?**

1. Activities already executed return cached results from history
2. Workflow resumes from the last checkpoint
3. No manual intervention required

## Installation

```bash
go get github.com/i2y/romancy
```

### Database Support

| Database | Use Case | Multi-Pod Support | Production Ready |
|----------|----------|-------------------|------------------|
| **SQLite** | Development, testing, single-process | ⚠️ Limited | ⚠️ Limited |
| **PostgreSQL** | Production, multi-process/multi-pod | ✅ Yes | ✅ Recommended |
| **MySQL** | Production, multi-process/multi-pod | ✅ Yes | ✅ Yes (8.0+) |

**Important**: For multi-process or multi-pod deployments (K8s, Docker Compose with multiple replicas), use PostgreSQL or MySQL.

### Database Migrations

Romancy uses [dbmate](https://github.com/amacneil/dbmate) for database schema management. The database schema is managed in the [durax-io/schema](https://github.com/durax-io/schema) repository, which is shared between Romancy (Go) and [Edda](https://github.com/i2y/edda) (Python) to ensure cross-framework compatibility.

**Running migrations with dbmate**:

```bash
# Install dbmate
brew install dbmate  # macOS
# or: go install github.com/amacneil/dbmate@latest

# Run migrations (SQLite)
dbmate --url "sqlite:workflow.db" --migrations-dir schema/db/migrations/sqlite up

# Run migrations (PostgreSQL)
dbmate --url "postgres://user:pass@localhost/db?sslmode=disable" --migrations-dir schema/db/migrations/postgres up

# Run migrations (MySQL)
dbmate --url "mysql://user:pass@localhost/db" --migrations-dir schema/db/migrations/mysql up

# Check current version
dbmate --url "sqlite:workflow.db" --migrations-dir schema/db/migrations/sqlite status
```

**Note**: The `WithAutoMigrate()` option is deprecated. Migrations should be managed externally using dbmate before starting the application.

## Core Concepts

### Workflows and Activities

**Activity**: A unit of work that performs business logic. Activity results are recorded in history.

**Workflow**: Orchestration logic that coordinates activities. Workflows can be replayed from history after crashes.

```go
// Input/Output types
type EmailInput struct {
    Email   string `json:"email"`
    Message string `json:"message"`
}

type SignupResult struct {
    Status string `json:"status"`
}

// Define an activity with DefineActivity
var sendEmail = romancy.DefineActivity("send_email",
    func(ctx context.Context, input EmailInput) (bool, error) {
        // Business logic - this will be recorded
        log.Printf("Sending email to %s", input.Email)
        return true, nil
    },
)

// Define a workflow with DefineWorkflow
var userSignup = romancy.DefineWorkflow("user_signup",
    func(ctx *romancy.WorkflowContext, email string) (SignupResult, error) {
        // Orchestration logic
        _, err := sendEmail.Execute(ctx, EmailInput{Email: email, Message: "Welcome!"})
        if err != nil {
            return SignupResult{}, err
        }
        return SignupResult{Status: "completed"}, nil
    },
)
```

### Durable Execution

Romancy ensures workflow progress is never lost through **deterministic replay**:

1. **Activity results are recorded** in a history table
2. **On crash recovery**, workflows resume from the last checkpoint
3. **Already-executed activities** return cached results from history
4. **New activities** continue from where the workflow left off

**Key guarantees**:
- Activities execute **exactly once** (results cached in history)
- Workflows can survive **arbitrary crashes**
- No manual checkpoint management required

### Compensation (Saga Pattern)

When a workflow fails, Romancy can execute compensation functions for already-executed activities in reverse order:

```go
// Define activity with inline compensation
var reserveInventory = romancy.DefineActivity("reserve_inventory",
    func(ctx context.Context, itemID string) (string, error) {
        log.Printf("Reserved %s", itemID)
        return "reservation-123", nil
    },
    romancy.WithCompensation[string, string](func(ctx context.Context, itemID string) error {
        log.Printf("Cancelled reservation for %s", itemID)
        return nil
    }),
)
```

### Multi-worker Execution

Multiple workers can safely process workflows using database-based exclusive control:

```go
app := romancy.NewApp(
    romancy.WithDatabase("postgres://localhost/workflows"),  // Shared database
    romancy.WithWorkerID("order-service"),
)
```

**Features**:
- Each workflow instance runs on only one worker at a time
- Automatic stale lock cleanup (5-minute timeout)
- Crashed workflows automatically resume on any available worker

### Transactional Outbox

Activities are automatically transactional by default, ensuring atomicity between:
1. Activity execution (side effects)
2. History recording (replay data)
3. Event publishing (outbox table)

```go
// Default: activity runs in a transaction
var createOrder = romancy.DefineActivity("create_order",
    func(ctx context.Context, orderID string) (map[string]any, error) {
        // If this activity fails, both the history record and any
        // outbox events will be rolled back together
        log.Printf("Creating order: %s", orderID)
        return map[string]any{"order_id": orderID, "status": "created"}, nil
    },
)
```

**Opt-out of transactions** for external API calls:

```go
var callExternalAPI = romancy.DefineActivity("call_api",
    func(ctx context.Context, url string) (map[string]any, error) {
        // Not transactional - suitable for external calls
        resp, err := http.Get(url)
        // ...
    },
    romancy.WithTransactional(false),
)
```

### Event & Timer Waiting

Workflows can wait for external events or timers without consuming worker resources:

```go
// Event data type
type PaymentData struct {
    OrderID string `json:"order_id"`
    Status  string `json:"status"`
}

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
    func(ctx *romancy.WorkflowContext, orderID string) (PaymentData, error) {
        // Wait for payment completion event (process-releasing)
        event, err := romancy.WaitEvent[PaymentData](ctx, "payment.completed")
        if err != nil {
            return PaymentData{}, err
        }
        return event.Data, nil
    },
)

// With timeout
var paymentWithTimeout = romancy.DefineWorkflow("payment_with_timeout",
    func(ctx *romancy.WorkflowContext, orderID string) (PaymentData, error) {
        event, err := romancy.WaitEvent[PaymentData](ctx, "payment.completed",
            romancy.WithEventTimeout(5*time.Minute))
        if err != nil {
            return PaymentData{}, err
        }
        return event.Data, nil
    },
)
```

**Sleep() for time-based waiting**:

```go
var orderWithTimeout = romancy.DefineWorkflow("order_with_timeout",
    func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
        // Sleep for 60 seconds
        if err := romancy.Sleep(ctx, 60*time.Second); err != nil {
            return nil, err
        }
        // Continue after sleep completes
        return checkPayment.Execute(ctx, orderID)
    },
)
```

### Channel Messaging

Workflows can communicate with each other using durable channels with two delivery modes:

- **Broadcast**: All subscribers receive every message
- **Competing**: Each message goes to exactly one subscriber (work queue pattern)

```go
var workerWorkflow = romancy.DefineWorkflow("worker",
    func(ctx *romancy.WorkflowContext, workerID string) (map[string]any, error) {
        // Subscribe to a channel in competing mode (work queue)
        if err := romancy.Subscribe(ctx, "tasks", romancy.ModeCompeting); err != nil {
            return nil, err
        }

        // Wait for and receive a message
        msg, err := romancy.Receive[TaskData](ctx, "tasks",
            romancy.WithReceiveTimeout(60*time.Second))
        if err != nil {
            return nil, err
        }

        log.Printf("Worker %s received task: %v", workerID, msg.Data)
        return map[string]any{"task": msg.Data}, nil
    },
)

var dispatcherWorkflow = romancy.DefineWorkflow("dispatcher",
    func(ctx *romancy.WorkflowContext, _ struct{}) (map[string]any, error) {
        // Publish a task to the channel
        if err := romancy.Publish(ctx, "tasks", TaskData{ID: "task-1"}); err != nil {
            return nil, err
        }
        return map[string]any{"dispatched": true}, nil
    },
)
```

```go
// Send a direct message to a specific workflow instance
romancy.SendTo(ctx, targetInstanceID, "channel", data)

// Unsubscribe from a channel
romancy.Unsubscribe(ctx, "channel")

// Publish with metadata
romancy.Publish(ctx, "channel", data, romancy.WithMetadata(map[string]any{
    "priority": "high",
}))
```

### Recur Pattern

Long-running workflows can use Erlang-style tail recursion to prevent unbounded history growth:

```go
// For Recur, input and output types must be the same
type CounterState struct {
    Count      int  `json:"count"`
    Completed  bool `json:"completed"`
    FinalCount int  `json:"final_count,omitempty"`
}

var counterWorkflow = romancy.DefineWorkflow("counter",
    func(ctx *romancy.WorkflowContext, input CounterState) (CounterState, error) {
        newCount := input.Count + 1

        if newCount < 1000 {
            // Continue with new input (archives history, restarts workflow)
            return romancy.Recur(ctx, CounterState{Count: newCount})
        }

        // Complete when done
        return CounterState{Completed: true, FinalCount: newCount}, nil
    },
)
```

**Key benefits**:
- History is archived before each recur
- `continued_from` field tracks workflow lineage
- Prevents memory/storage issues with long-running workflows

## HTTP Integration

### Using with net/http

Romancy provides a `Handler()` method for integration with any Go HTTP router:

```go
// Standard library
http.Handle("/", app.Handler())

// With chi router
r := chi.NewRouter()
r.Mount("/workflows", app.Handler())

// With gorilla/mux
r := mux.NewRouter()
r.PathPrefix("/workflows").Handler(app.Handler())
```

### CloudEvents Support

Romancy accepts CloudEvents at the HTTP endpoint:

```bash
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "order.created",
    "source": "order-service",
    "id": "event-123",
    "data": {"order_id": "ORD-123", "amount": 99.99}
  }'
```

**HTTP Response Codes**:
- **202 Accepted**: Event accepted for asynchronous processing
- **400 Bad Request**: CloudEvents parsing/validation error (non-retryable)
- **500 Internal Server Error**: Internal error (retryable)

## MCP Integration

Romancy provides [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) integration, allowing you to expose workflows as MCP tools for AI assistants.

### Quick Start

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/mcp"
)

// Define workflow input/output types with jsonschema tags
type OrderInput struct {
	OrderID    string  `json:"order_id" jsonschema:"The unique order ID"`
	CustomerID string  `json:"customer_id" jsonschema:"The customer ID"`
	Amount     float64 `json:"amount" jsonschema:"The order amount in dollars"`
}

type OrderResult struct {
	OrderID      string `json:"order_id" jsonschema:"The order ID"`
	Status       string `json:"status" jsonschema:"Final order status"`
	Confirmation string `json:"confirmation" jsonschema:"Confirmation number"`
}

// Define workflow
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderResult, error) {
		// Workflow logic here
		return OrderResult{
			OrderID:      input.OrderID,
			Status:       "confirmed",
			Confirmation: "CONF-123",
		}, nil
	},
)

func main() {
	// Create Romancy app
	app := romancy.NewApp(romancy.WithDatabase("workflow.db"))

	// Create MCP server
	server := mcp.NewServer(app,
		mcp.WithServerName("order-service"),
		mcp.WithServerVersion("1.0.0"),
	)

	// Register workflow - auto-generates 4 MCP tools:
	// - order_workflow_start
	// - order_workflow_status
	// - order_workflow_result
	// - order_workflow_cancel
	mcp.RegisterWorkflow[OrderInput, OrderResult](server, orderWorkflow,
		mcp.WithDescription("Process customer orders"),
	)

	// Initialize and run
	ctx := context.Background()
	if err := server.Initialize(ctx); err != nil {
		log.Fatal(err)
	}
	defer server.Shutdown(ctx)

	// Run on stdio transport
	if err := server.RunStdio(ctx); err != nil {
		log.Fatal(err)
	}
}
```

### Auto-Generated Tools

When you register a workflow, Romancy automatically generates four MCP tools:

| Tool | Description |
|------|-------------|
| `{workflow}_start` | Start a new workflow instance with input parameters |
| `{workflow}_status` | Get the current status of a workflow instance |
| `{workflow}_result` | Get the result of a completed workflow |
| `{workflow}_cancel` | Cancel a running workflow instance |

## LLM Integration

Romancy provides durable LLM calls via the [bucephalus](https://github.com/i2y/bucephalus) library. All LLM calls are automatically cached as activities, so workflow replay returns cached results without re-invoking the LLM API.

### Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/llm"

	// Import the provider you want to use
	_ "github.com/i2y/bucephalus/anthropic"
)

type SummaryInput struct {
	Text string `json:"text"`
}

type SummaryResult struct {
	Summary string `json:"summary"`
}

var summarizeWorkflow = romancy.DefineWorkflow("summarize",
	func(ctx *romancy.WorkflowContext, input SummaryInput) (SummaryResult, error) {
		// LLM call is cached for replay
		response, err := llm.Call(ctx, input.Text,
			llm.WithSystemMessage("Summarize the following text concisely."),
		)
		if err != nil {
			return SummaryResult{}, err
		}
		return SummaryResult{Summary: response.Text}, nil
	},
)

func main() {
	ctx := context.Background()

	// Create app
	app := romancy.NewApp(romancy.WithDatabase("workflow.db"))

	// Set LLM defaults for the app
	llm.SetAppDefaults(app,
		llm.WithProvider("anthropic"),
		llm.WithModel("claude-sonnet-4-5-20250929"),
		llm.WithMaxTokens(1024),
	)

	romancy.RegisterWorkflow(app, summarizeWorkflow)

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	instanceID, _ := romancy.StartWorkflow(ctx, app, summarizeWorkflow, SummaryInput{
		Text: "Long article text here...",
	})
	fmt.Println("Started:", instanceID)
}
```

### Features

| Feature | Description |
|---------|-------------|
| `llm.Call()` | Ad-hoc LLM call with automatic caching |
| `llm.CallParse[T]()` | Parse response into a struct |
| `llm.CallMessages()` | Multi-turn conversation |
| `llm.DefineDurableCall()` | Reusable LLM definition |
| `llm.DurableAgent[T]` | Stateful multi-turn agent |
| `llm.SetAppDefaults()` | App-level default configuration |

### Supported Providers

Via bucephalus, Romancy supports:
- **Anthropic** (Claude models)
- **OpenAI** (GPT models)
- **Google Gemini**

## Development

### Running Tests

```bash
go test ./...
```

### Project Structure

```
romancy/
├── app.go           # Main App struct and HTTP handling
├── workflow.go      # Workflow definition and execution
├── activity.go      # Activity definition with transactions
├── context.go       # WorkflowContext for workflow execution
├── events.go        # Event and timer waiting
├── compensation.go  # Saga pattern compensation
├── hooks/           # Observability hooks
├── outbox/          # Transactional outbox
├── mcp/             # MCP (Model Context Protocol) integration
├── llm/             # LLM integration (via bucephalus)
└── internal/
    ├── storage/     # Database abstraction
    └── replay/      # Deterministic replay engine
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Related Projects

- **[Edda](https://github.com/i2y/edda)**: The original Python implementation
