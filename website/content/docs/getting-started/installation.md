---
title: "Installation"
weight: 1
---

This guide will help you install Romancy and set up your development environment.

## Prerequisites

- **Go 1.24 or higher**

## Installing Romancy

Add Romancy to your Go project:

```bash
go get github.com/i2y/romancy
```

This installs Romancy with SQLite support, which is perfect for:

- Local development
- Testing
- Single-process deployments

**Important**: For multi-process or multi-pod deployments (K8s, Docker Compose with multiple replicas, etc.), you must use PostgreSQL. SQLite supports multiple goroutines within a single process, but its table-level locking makes it unsuitable for multi-process/multi-pod scenarios.

## Verifying Installation

Create a test file `main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

// Define an activity
var helloActivity = romancy.DefineActivity("hello",
	func(ctx context.Context, name string) (string, error) {
		return fmt.Sprintf("Hello, %s!", name), nil
	},
)

// Define a workflow
var helloWorkflow = romancy.DefineWorkflow("hello_workflow",
	func(ctx *romancy.WorkflowContext, name string) (map[string]any, error) {
		result, err := helloActivity.Execute(ctx, name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"message": result}, nil
	},
)

func main() {
	// Create app with in-memory SQLite
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	// Start the app before starting workflows
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, helloWorkflow, "World")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Workflow started: %s\n", instanceID)

	// Get result
	instance, err := app.Storage().GetInstance(ctx, instanceID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Result: %v\n", instance.OutputData)
}
```

Run the test:

```bash
go run main.go
```

Expected output:

```
Workflow started: <instance_id>
Result: map[message:Hello, World!]
```

## Database Setup

### Database Selection

| Database | Use Case | Multi-Worker Support | Production Ready |
|----------|----------|-------------------|------------------|
| **SQLite** | Development, testing, single-process apps | Single-process only | Limited |
| **PostgreSQL** | Production, multi-process/multi-pod systems | Yes | Yes (Recommended) |

### SQLite (Default)

No additional setup required! SQLite databases are created automatically:

```go
// File-based SQLite (single-process only)
app := romancy.NewApp(
	romancy.WithDatabase("workflow.db"),
)

// In-memory (testing only)
app := romancy.NewApp(
	romancy.WithDatabase(":memory:"),
)
```

**SQLite Considerations:**

**Supported scenarios:**

- Single-process deployments (even with multiple goroutines within that process)
- Development and testing environments
- Low-traffic single-server applications

**Not supported:**

- Multi-process deployments (Docker Compose with `replicas: 3`, multiple systemd services)
- Multi-pod deployments (Kubernetes with multiple replicas)
- High-concurrency production scenarios

**Performance limitations:**

- Table-level locking (not row-level like PostgreSQL)
- Concurrent writes are serialized, impacting throughput
- For production with multiple processes/pods, use PostgreSQL

### PostgreSQL

1. **Install PostgreSQL** (if not already installed)

2. **Create a database**:

```sql
CREATE DATABASE romancy_workflows;
```

3. **Configure connection**:

```go
app := romancy.NewApp(
	romancy.WithDatabase("postgres://user:password@localhost/romancy_workflows"),
)
```

## Next Steps

- **[Quick Start](/docs/getting-started/quick-start)**: Build your first workflow in 5 minutes
- **[Core Concepts](/docs/getting-started/concepts)**: Learn about workflows, activities, and durable execution
- **[Your First Workflow](/docs/getting-started/first-workflow)**: Step-by-step tutorial
