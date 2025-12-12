---
title: "Quick Start"
weight: 2
---

Get started with Romancy in 5 minutes! This guide will walk you through creating your first durable workflow.

## Prerequisites

Before starting, make sure you have Romancy installed:

```bash
go get github.com/i2y/romancy
```

If you haven't set up your Go environment, see the [Installation Guide](/docs/getting-started/installation).

## Step 1: Create a Simple Workflow

Create a new file `main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

// Input type for the workflow
type OnboardingInput struct {
	UserID string
	Email  string
}

// Define activities
var sendWelcomeEmail = romancy.DefineActivity("send_welcome_email",
	func(ctx context.Context, email string) (map[string]any, error) {
		fmt.Printf("Sending welcome email to %s\n", email)
		return map[string]any{"sent": true, "email": email}, nil
	},
)

var createUserProfile = romancy.DefineActivity("create_user_profile",
	func(ctx context.Context, input OnboardingInput) (map[string]any, error) {
		fmt.Printf("Creating profile for user %s\n", input.UserID)
		return map[string]any{"user_id": input.UserID, "email": input.Email, "created": true}, nil
	},
)

// Define workflow
var userOnboarding = romancy.DefineWorkflow("user_onboarding",
	func(ctx *romancy.WorkflowContext, input OnboardingInput) (map[string]any, error) {
		// Step 1: Create profile
		profile, err := createUserProfile.Execute(ctx, input)
		if err != nil {
			return nil, err
		}

		// Step 2: Send welcome email
		emailResult, err := sendWelcomeEmail.Execute(ctx, input.Email)
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"status":     "completed",
			"user_id":    profile["user_id"],
			"email_sent": emailResult["sent"],
		}, nil
	},
)

func main() {
	// Create Romancy app with SQLite database
	app := romancy.NewApp(
		romancy.WithDatabase("onboarding.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start the workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, userOnboarding,
		OnboardingInput{UserID: "user_123", Email: "newuser@example.com"})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Workflow started with ID: %s\n", instanceID)

	// Get workflow result
	instance, err := app.Storage().GetInstance(ctx, instanceID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Result: %v\n", instance.OutputData)
}
```

## Step 2: Run the Workflow

```bash
go run main.go
```

**Output:**

```
Creating profile for user user_123
Sending welcome email to newuser@example.com
Workflow started with ID: <instance_id>
Result: map[email_sent:true status:completed user_id:user_123]
```

## Step 3: Understanding Crash Recovery

Romancy's durable execution ensures workflows survive crashes through **deterministic replay**. When a workflow crashes:

1. **Activity results are saved** to the database before execution continues
2. **Workflow state is preserved** (current step, history, locks)
3. **Automatic recovery** detects and resumes stale workflows

### How Automatic Recovery Works

In production environments with long-running Romancy app instances:

- **Crash detection**: Romancy's background task checks for stale locks every 60 seconds
- **Auto-resume**: Crashed workflows are automatically resumed when their lock timeout expires
  - Both normal execution and rollback execution are automatically resumed
  - Default timeout: 5 minutes (300 seconds)
  - Workflows resume from their last checkpoint using deterministic replay
- **Deterministic replay**: Previously executed activities return cached results from history
- **Resume from checkpoint**: Only remaining activities execute fresh

### Workflows Waiting for Events or Timers

Workflows in special waiting states are handled differently:

- **Waiting for Events**: Resumed immediately when the awaited event arrives (not on a fixed schedule)
- **Waiting for Timers**: Checked every 10 seconds and resumed when the timer expires
- These workflows are **not** included in the 60-second crash recovery cycle

### Automatic Recovery Mechanisms

| Workflow State | Recovery Check Interval | When Resumed |
|----------------|------------------------|--------------|
| Normal execution or rollback | Every 60 seconds | When lock timeout expires (default: 5 min) |
| Waiting for event | Immediate (event-driven) | When event arrives |
| Waiting for timer | Every 10 seconds | When timer expires |

### Production Behavior

In production (use PostgreSQL for distributed systems):

```go
// For distributed systems (K8s, Docker Compose with multiple replicas)
// Use PostgreSQL (NOT SQLite)
app := romancy.NewApp(
	romancy.WithDatabase("postgres://user:password@localhost/workflows"),
	romancy.WithWorkerID("worker-1"),
)

// Background tasks automatically handle:
// - Stale lock cleanup
// - Workflow auto-resume
// - Deterministic replay
```

**Important**: For distributed execution (multiple worker pods/containers), you **must** use PostgreSQL. SQLite's single-writer limitation makes it unsuitable for multi-pod deployments.

**When a crash occurs:**

1. Worker process crashes mid-workflow
2. Lock remains in database (marks workflow as "in-progress")
3. After 5 minutes, another worker detects the stale lock
4. Workflow automatically resumes from last checkpoint
5. Previously executed activities skip (cached from history)
6. Remaining activities execute fresh

This is **deterministic replay** - Romancy's core feature for durable execution.

## Key Concepts Demonstrated

### Activities

```go
var sendWelcomeEmail = romancy.DefineActivity("send_welcome_email",
	func(ctx context.Context, email string) (map[string]any, error) {
		// Business logic here
		return map[string]any{"sent": true}, nil
	},
)
```

- Activities perform actual work (database writes, API calls, etc.)
- Activity results are **automatically saved in history**
- On replay, activities return cached results

### Workflows

```go
var userOnboarding = romancy.DefineWorkflow("user_onboarding",
	func(ctx *romancy.WorkflowContext, userID, email string) (map[string]any, error) {
		// Orchestration logic here
		result1, err := activity1.Execute(ctx, ...)
		result2, err := activity2.Execute(ctx, ...)
		return result, nil
	},
)
```

- Workflows orchestrate activities
- Workflows can be replayed after crashes
- Workflows resume from the last checkpoint

### WorkflowContext

```go
func(ctx *romancy.WorkflowContext, ...) (map[string]any, error) {
	// ctx provides workflow operations
	// Automatically manages history and replay
}
```

- `ctx` provides workflow operations
- Automatically manages history and replay

## Next Steps

Now that you've created your first workflow, learn more about:

- **[Core Concepts](/docs/getting-started/concepts)**: Deep dive into workflows, activities, and durable execution
- **[Your First Workflow](/docs/getting-started/first-workflow)**: Build a complete order processing workflow step-by-step
- **[Examples](/docs/examples/simple)**: See more real-world examples
- **[Saga Pattern](/docs/core-features/saga-compensation)**: Learn about compensation and rollback
