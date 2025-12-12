---
title: "Simple Workflow"
weight: 1
---

This example demonstrates the basics of creating a workflow with activities in Romancy.

## What This Example Shows

- ✅ Defining activities with `DefineActivity`
- ✅ Defining a workflow with `DefineWorkflow`
- ✅ Starting a workflow
- ✅ Basic workflow execution

## Code Overview

### Define Activities

```go
package main

import (
	"context"
	"fmt"

	"github.com/i2y/romancy"
)

// greetUser activity that greets a user
var greetUser = romancy.DefineActivity("greet_user",
	func(ctx context.Context, name string) (map[string]any, error) {
		fmt.Printf("[Activity] Greeting user: %s\n", name)
		return map[string]any{
			"message": fmt.Sprintf("Hello, %s!", name),
		}, nil
	},
)

// processData activity that processes some data
var processData = romancy.DefineActivity("process_data",
	func(ctx context.Context, data string) (map[string]any, error) {
		fmt.Printf("[Activity] Processing data: %s\n", data)
		processed := strings.ToUpper(data)
		return map[string]any{
			"processed": processed,
			"length":    len(processed),
		}, nil
	},
)

// finalize activity that finalizes the workflow
var finalize = romancy.DefineActivity("finalize",
	func(ctx context.Context, result map[string]any) (map[string]any, error) {
		fmt.Printf("[Activity] Finalizing with result: %v\n", result)
		return map[string]any{
			"status":       "completed",
			"final_result": result,
		}, nil
	},
)
```

### Define Workflow

```go
package main

import (
	"fmt"

	"github.com/i2y/romancy"
)

// simpleWorkflow coordinates multiple activities
var simpleWorkflow = romancy.DefineWorkflow("simple_workflow",
	func(ctx *romancy.WorkflowContext, input SimpleInput) (map[string]any, error) {
		fmt.Printf("[Workflow] Starting simple_workflow for %s\n", input.Name)

		// Step 1: Greet the user
		greetingResult, err := greetUser.Execute(ctx, input.Name)
		if err != nil {
			return nil, err
		}
		fmt.Printf("[Workflow] Step 1 completed: %v\n", greetingResult)

		// Step 2: Process data
		processResult, err := processData.Execute(ctx, input.Data)
		if err != nil {
			return nil, err
		}
		fmt.Printf("[Workflow] Step 2 completed: %v\n", processResult)

		// Step 3: Finalize
		finalResult, err := finalize.Execute(ctx, map[string]any{
			"greeting":   greetingResult,
			"processing": processResult,
		})
		if err != nil {
			return nil, err
		}
		fmt.Printf("[Workflow] Step 3 completed: %v\n", finalResult)

		fmt.Println("[Workflow] Workflow completed successfully!")
		return finalResult, nil
	},
)

// SimpleInput defines the workflow input
type SimpleInput struct {
	Name string `json:"name"`
	Data string `json:"data"`
}
```

### Run the Workflow

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

func main() {
	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("demo.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	// Start the app
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start the workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, simpleWorkflow, SimpleInput{
		Name: "Alice",
		Data: "hello world from romancy",
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Workflow started with ID: %s\n", instanceID)
}
```

## Running the Example

Create a file named `simple_workflow.go` with the complete code (see [Complete Code](#complete-code) section below), then run:

```bash
# Initialize Go module
go mod init simple-example
go get github.com/i2y/romancy

# Run your workflow
go run simple_workflow.go
```

## Expected Output

```
============================================================
Romancy Framework - Simple Workflow Example
============================================================

>>> Starting workflow...

[Workflow] Starting simple_workflow for Alice
[Activity] Greeting user: Alice
[Workflow] Step 1 completed: map[message:Hello, Alice!]
[Activity] Processing data: hello world from romancy
[Workflow] Step 2 completed: map[processed:HELLO WORLD FROM ROMANCY length:24]
[Activity] Finalizing with result: {...}
[Workflow] Step 3 completed: map[status:completed final_result:map[...]]
[Workflow] Workflow completed successfully!

>>> Workflow started with instance ID: <instance_id>
```

## Complete Code

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/i2y/romancy"
)

// SimpleInput defines the workflow input
type SimpleInput struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

// Activities

var greetUser = romancy.DefineActivity("greet_user",
	func(ctx context.Context, name string) (map[string]any, error) {
		fmt.Printf("[Activity] Greeting user: %s\n", name)
		return map[string]any{
			"message": fmt.Sprintf("Hello, %s!", name),
		}, nil
	},
)

var processData = romancy.DefineActivity("process_data",
	func(ctx context.Context, data string) (map[string]any, error) {
		fmt.Printf("[Activity] Processing data: %s\n", data)
		processed := strings.ToUpper(data)
		return map[string]any{
			"processed": processed,
			"length":    len(processed),
		}, nil
	},
)

var finalize = romancy.DefineActivity("finalize",
	func(ctx context.Context, result map[string]any) (map[string]any, error) {
		fmt.Printf("[Activity] Finalizing with result: %v\n", result)
		return map[string]any{
			"status":       "completed",
			"final_result": result,
		}, nil
	},
)

// Workflow

var simpleWorkflow = romancy.DefineWorkflow("simple_workflow",
	func(ctx *romancy.WorkflowContext, input SimpleInput) (map[string]any, error) {
		fmt.Printf("[Workflow] Starting simple_workflow for %s\n", input.Name)

		// Step 1: Greet the user
		greetingResult, err := greetUser.Execute(ctx, input.Name)
		if err != nil {
			return nil, err
		}
		fmt.Printf("[Workflow] Step 1 completed: %v\n", greetingResult)

		// Step 2: Process data
		processResult, err := processData.Execute(ctx, input.Data)
		if err != nil {
			return nil, err
		}
		fmt.Printf("[Workflow] Step 2 completed: %v\n", processResult)

		// Step 3: Finalize
		finalResult, err := finalize.Execute(ctx, map[string]any{
			"greeting":   greetingResult,
			"processing": processResult,
		})
		if err != nil {
			return nil, err
		}
		fmt.Printf("[Workflow] Step 3 completed: %v\n", finalResult)

		fmt.Println("[Workflow] Workflow completed successfully!")
		return finalResult, nil
	},
)

func main() {
	fmt.Println("============================================================")
	fmt.Println("Romancy Framework - Simple Workflow Example")
	fmt.Println("============================================================")
	fmt.Println()

	// Create Romancy app
	app := romancy.NewApp(
		romancy.WithDatabase("demo.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	// Initialize the app
	if err := app.Initialize(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	fmt.Println(">>> Starting workflow...")
	fmt.Println()

	// Start the workflow
	instanceID, err := simpleWorkflow.Start(ctx, app, SimpleInput{
		Name: "Alice",
		Data: "hello world from romancy",
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println()
	fmt.Printf(">>> Workflow started with instance ID: %s\n", instanceID)
}
```

## Concurrent Execution with errgroup

For concurrent activity execution, use `errgroup`:

```go
import (
	"golang.org/x/sync/errgroup"
)

var concurrentWorkflow = romancy.DefineWorkflow("concurrent_workflow",
	func(ctx *romancy.WorkflowContext, items []string) (map[string]any, error) {
		// Concurrent execution with errgroup
		g, gctx := errgroup.WithContext(ctx.Context())
		results := make([]map[string]any, len(items))

		for i, item := range items {
			i, item := i, item // Capture loop variables
			g.Go(func() error {
				result, err := processItem.Execute(ctx, item,
					romancy.WithActivityID(fmt.Sprintf("process_item:%d", i+1)))
				if err != nil {
					return err
				}
				results[i] = result
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			return nil, err
		}

		return map[string]any{"results": results}, nil
	},
)
```

**Why explicit Activity IDs for concurrent execution?**

- Concurrent execution order is non-deterministic
- Romancy needs explicit IDs to match activities during replay
- Explicit IDs ensure deterministic replay even with concurrent execution

## What You Learned

- ✅ **Activities** perform business logic and are recorded in history
- ✅ **Workflows** orchestrate activities
- ✅ **WorkflowContext** (`ctx`) is automatically provided to workflows
- ✅ **App** manages the workflow engine and database
- ✅ **`romancy.StartWorkflow()`** begins workflow execution

## Next Steps

- **[E-commerce Example](/docs/examples/ecommerce)**: Learn about type-safe workflows
- **[Saga Pattern](/docs/examples/saga)**: Understand compensation and rollback
- **[Event Waiting](/docs/examples/events)**: Wait for external events in workflows
- **[Core Concepts](/docs/getting-started/concepts)**: Deep dive into workflows and activities
