---
title: "MCP Integration"
weight: 1
---

Romancy provides seamless integration with the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/), allowing AI assistants like Claude to interact with your durable workflows as long-running tools.

## Overview

MCP is a standardized protocol for AI tool integration. Romancy's MCP integration automatically converts your durable workflows into MCP-compliant tools that:

- **Start workflows** and return instance IDs immediately
- **Check workflow status** to monitor progress with completed activity count and suggested poll interval
- **Retrieve results** when workflows complete
- **Cancel workflows** if running or waiting, with automatic compensation execution

This enables AI assistants to work with long-running processes that may take minutes, hours, or even days to complete, with full control over the workflow lifecycle.

## Quick Start

### 1. Create an MCP Server

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/mcp"
)

// OrderInput is the input for the order workflow.
type OrderInput struct {
	OrderID string  `json:"order_id" jsonschema:"The unique order ID"`
	Amount  float64 `json:"amount" jsonschema:"The total order amount"`
}

// OrderResult is the output of the order workflow.
type OrderResult struct {
	OrderID      string `json:"order_id"`
	Status       string `json:"status"`
	Confirmation string `json:"confirmation"`
}

// Activities

var validateOrder = romancy.DefineActivity("validate_order",
	func(ctx context.Context, input OrderInput) (bool, error) {
		return input.Amount > 0, nil
	},
)

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string) (string, error) {
		return "CONF-" + orderID, nil
	},
)

// Workflow - implements romancy.Workflow interface

type OrderWorkflow struct{}

func (w *OrderWorkflow) Name() string { return "order_workflow" }

func (w *OrderWorkflow) Execute(ctx *romancy.WorkflowContext, input OrderInput) (OrderResult, error) {
	// Step 1: Validate
	valid, err := validateOrder.Execute(ctx, input)
	if err != nil || !valid {
		return OrderResult{Status: "invalid"}, err
	}

	// Step 2: Process payment
	confirmation, err := processPayment.Execute(ctx, input.OrderID)
	if err != nil {
		return OrderResult{Status: "payment_failed"}, err
	}

	return OrderResult{
		OrderID:      input.OrderID,
		Status:       "completed",
		Confirmation: confirmation,
	}, nil
}

func main() {
	ctx := context.Background()

	// Create Romancy app first
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("mcp-worker"),
	)

	// Create MCP server wrapping the Romancy app
	server := mcp.NewServer(app,
		mcp.WithServerName("order-service"),
		mcp.WithServerVersion("1.0.0"),
	)

	// Register workflow - auto-generates 4 MCP tools
	mcp.RegisterWorkflow[OrderInput, OrderResult](server, &OrderWorkflow{},
		mcp.WithDescription("Process customer orders"),
	)

	// Initialize (starts the Romancy app)
	if err := server.Initialize(ctx); err != nil {
		log.Fatal(err)
	}
	defer server.Shutdown(ctx)

	// Run on stdio transport (for Claude Desktop)
	if err := server.RunStdio(ctx); err != nil {
		log.Fatal(err)
	}
}
```

### 2. Configure Claude Desktop

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS):

```json
{
  "mcpServers": {
    "order-service": {
      "command": "./order-service",
      "args": []
    }
  }
}
```

## Auto-Generated Tools

Each registered workflow automatically generates **four MCP tools**:

### 1. Start Tool: `{workflow}_start`

Starts a new workflow instance and returns immediately.

```
Tool Name: order_workflow_start
Input: {"order_id": "ORD-123", "amount": 99.99}
Output: "Workflow started. Instance ID: abc123..."
```

### 2. Status Tool: `{workflow}_status`

Checks the current status of a workflow instance.

```
Tool Name: order_workflow_status
Input: {"instance_id": "abc123..."}
Output: "Status: running, Completed Activities: 1, Suggested Poll Interval: 5000ms"
```

### 3. Result Tool: `{workflow}_result`

Gets the final result of a completed workflow.

```
Tool Name: order_workflow_result
Input: {"instance_id": "abc123..."}
Output: {"order_id": "ORD-123", "status": "completed", "confirmation": "CONF-ORD-123"}
```

### 4. Cancel Tool: `{workflow}_cancel`

Cancels a running workflow and triggers compensation.

```
Tool Name: order_workflow_cancel
Input: {"instance_id": "abc123..."}
Output: "Workflow cancelled. Compensation executed."
```

## Workflow Interface

Workflows registered with MCP must implement the `romancy.Workflow` interface:

```go
type Workflow[I, O any] interface {
	Name() string
	Execute(ctx *WorkflowContext, input I) (O, error)
}
```

Example:

```go
type MyWorkflow struct{}

func (w *MyWorkflow) Name() string { return "my_workflow" }

func (w *MyWorkflow) Execute(ctx *romancy.WorkflowContext, input MyInput) (MyOutput, error) {
	// Workflow logic here
	return MyOutput{}, nil
}
```

## Server Options

### WithServerName

Sets the MCP server name (shown to clients).

```go
mcp.WithServerName("order-service")
```

### WithServerVersion

Sets the MCP server version.

```go
mcp.WithServerVersion("1.0.0")
```

### WithTransportMode

Sets the transport mode (stdio or HTTP).

```go
mcp.WithTransportMode(mcp.TransportStdio) // default
mcp.WithTransportMode(mcp.TransportHTTP)
```

## Workflow Tool Options

### WithDescription

Sets the main description for the workflow tools.

```go
mcp.RegisterWorkflow[I, O](server, workflow,
	mcp.WithDescription("Process customer orders with payment"),
)
```

### WithNamePrefix

Sets a custom prefix for the generated tool names.

```go
mcp.RegisterWorkflow[I, O](server, workflow,
	mcp.WithNamePrefix("custom_prefix"), // generates custom_prefix_start, etc.
)
```

## HTTP Transport

For REST API access, use the HTTP handler:

```go
func main() {
	app := romancy.NewApp(romancy.WithDatabase("orders.db"))
	server := mcp.NewServer(app,
		mcp.WithServerName("order-service"),
		mcp.WithTransportMode(mcp.TransportHTTP),
	)

	mcp.RegisterWorkflow[OrderInput, OrderResult](server, &OrderWorkflow{})

	ctx := context.Background()
	if err := server.Initialize(ctx); err != nil {
		log.Fatal(err)
	}
	defer server.Shutdown(ctx)

	// Mount MCP handler on your HTTP server
	http.Handle("/mcp", server.Handler())
	http.ListenAndServe(":8080", nil)
}
```

## Architecture

```
┌─────────────────┐
│    MCP Client   │
│  (Claude, etc)  │
└────────┬────────┘
         │ JSON-RPC 2.0
         │ (stdio or HTTP)
         ▼
┌─────────────────┐
│   MCP Server    │
│   ┌─────────┐   │
│   │ MCP SDK │   │  ← Protocol Handler
│   └─────────┘   │
│   ┌─────────┐   │
│   │ Romancy │   │  ← Durable Execution
│   │   App   │   │
│   └─────────┘   │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│    Database     │
│ (SQLite/PG/MySQL)│
└─────────────────┘
```

## JSON Schema Support

Use the `jsonschema` struct tag to provide descriptions for MCP tool parameters:

```go
type OrderInput struct {
	OrderID string  `json:"order_id" jsonschema:"The unique order identifier"`
	Amount  float64 `json:"amount" jsonschema:"Total amount in dollars"`
	Items   []Item  `json:"items" jsonschema:"List of items in the order"`
}
```

These descriptions appear in the MCP tool schema, helping AI assistants understand the expected input format.

## Related Documentation

- [Workflows and Activities](/docs/core-features/workflows-activities)
- [Saga Pattern](/docs/core-features/saga-compensation)
- [MCP Protocol Specification](https://modelcontextprotocol.io/)
