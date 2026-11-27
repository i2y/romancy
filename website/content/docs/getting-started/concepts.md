---
title: "Core Concepts"
weight: 3
---

This guide introduces the fundamental concepts of Romancy: workflows, activities, durable execution, and the Saga pattern.

## Workflows vs. Activities

Romancy separates orchestration logic (**workflows**) from business logic (**activities**).

### Activities

**Activity**: A unit of work that performs business logic.

```go
type EmailInput struct {
	Email   string `json:"email"`
	Message string `json:"message"`
}

type EmailResult struct {
	Sent      bool   `json:"sent"`
	MessageID string `json:"message_id"`
}

var sendEmail = romancy.DefineActivity("send_email",
	func(ctx context.Context, input EmailInput) (EmailResult, error) {
		// Business logic - sends an actual email
		response, err := emailService.Send(input.Email, input.Message)
		if err != nil {
			return EmailResult{}, err
		}
		return EmailResult{Sent: true, MessageID: response.ID}, nil
	},
)
```

**Key characteristics:**

- Execute business logic (database writes, API calls, file I/O)
- Activity return values are automatically saved to history for deterministic replay
- On replay, return cached results from history (idempotent)
- Automatically transactional (by default)

### Workflows

**Workflow**: Orchestration logic that coordinates activities.

```go
type SignupInput struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type SignupResult struct {
	UserID string `json:"user_id"`
	Status string `json:"status"`
}

var userSignup = romancy.DefineWorkflow("user_signup",
	func(ctx *romancy.WorkflowContext, input SignupInput) (SignupResult, error) {
		// Step 1: Create user account
		user, err := createAccount.Execute(ctx, input)
		if err != nil {
			return SignupResult{}, err
		}

		// Step 2: Send welcome email
		_, err = sendEmail.Execute(ctx, EmailInput{
			Email:   input.Email,
			Message: fmt.Sprintf("Welcome, %s!", input.Name),
		})
		if err != nil {
			return SignupResult{}, err
		}

		// Step 3: Initialize user settings
		_, err = setupDefaultSettings.Execute(ctx, user.UserID)
		if err != nil {
			return SignupResult{}, err
		}

		return SignupResult{UserID: user.UserID, Status: "active"}, nil
	},
)
```

**Key characteristics:**

- Coordinate activities (orchestration, not business logic)
- Can be replayed from history after crashes
- Deterministic replay - workflow replays the same execution path using saved activity results
- Resume from the last checkpoint automatically

## Durable Execution

Romancy ensures workflow progress is never lost through **deterministic replay**.

### How It Works

```go
type ProcessOrderResult struct {
	OrderID       string `json:"order_id"`
	Status        string `json:"status"`
	ReservationID string `json:"reservation_id"`
	PaymentID     string `json:"payment_id"`
}

var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, orderID string) (ProcessOrderResult, error) {
		// Step 1: Reserve inventory
		reservation, err := reserveInventory.Execute(ctx, orderID) // Saved to history
		if err != nil {
			return ProcessOrderResult{}, err
		}

		// Process crashes here!

		// Step 2: Charge payment
		payment, err := chargePayment.Execute(ctx, orderID)
		if err != nil {
			return ProcessOrderResult{}, err
		}

		return ProcessOrderResult{
			OrderID:       orderID,
			Status:        "completed",
			ReservationID: reservation.ReservationID,
			PaymentID:     payment.PaymentID,
		}, nil
	},
)
```

**On crash recovery:**

1. **Workflow restarts** from the beginning
2. **Step 1 (reserveInventory)**: Returns cached result from history (does NOT execute again)
3. **Step 2 (chargePayment)**: Executes fresh (continues from checkpoint)

### Key Guarantees

- Activities execute **exactly once** (results cached in history)
- Workflows survive **arbitrary crashes** (process restarts, container failures, etc.)
- No manual checkpoint management required
- Deterministic replay - predictable behavior

## WorkflowContext

The `WorkflowContext` object provides access to workflow operations.

### Key Properties

| Property/Method | Description |
|----------------|-------------|
| `ctx.InstanceID()` | Workflow instance ID |
| `ctx.WorkflowName()` | Name of the workflow function |
| `ctx.IsReplaying()` | `true` if replaying from history |
| `ctx.Session()` | Access Romancy's managed database session |

## The Saga Pattern

When a workflow fails, Romancy automatically executes **compensation functions** for already-executed activities in **reverse order**.

This implements the [Saga pattern](https://microservices.io/patterns/data/saga.html) for distributed transaction rollback.

### Basic Compensation

```go
type ReservationResult struct {
	Reserved bool   `json:"reserved"`
	ItemID   string `json:"item_id"`
}

type OrderWorkflowResult struct {
	Status string `json:"status"`
}

// Activity with compensation
var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, itemID string) (ReservationResult, error) {
		log.Printf("Reserved %s", itemID)
		return ReservationResult{Reserved: true, ItemID: itemID}, nil
	},
	romancy.WithCompensation[string, ReservationResult](func(ctx context.Context, itemID string) error {
		log.Printf("Cancelled reservation for %s", itemID)
		return nil
	}),
)

type OrderItemsInput struct {
	OrderID string   `json:"order_id"`
	Items   []string `json:"items"`
}

// Workflow
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input OrderItemsInput) (OrderWorkflowResult, error) {
		for _, itemID := range input.Items {
			_, err := reserveInventory.Execute(ctx, itemID)
			if err != nil {
				return OrderWorkflowResult{}, err
			}
		}
		_, err := chargePayment.Execute(ctx, input.OrderID) // Fails in this example
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		return OrderWorkflowResult{Status: "completed"}, nil
	},
)
```

**Execution:**

```
Reserved item1
Reserved item2
chargePayment fails!
Cancelled reservation for item2  # Reverse order
Cancelled reservation for item1
```

### Compensation Rules

1. **Reverse Order**: Compensations run in **reverse order** of activity execution
2. **Already-Executed Only**: Only activities that **completed successfully** are compensated
3. **Automatic**: No manual trigger required - Romancy handles it

## AI Agent Workflows

Romancy is well-suited for orchestrating AI agent workflows that involve long-running tasks:

### Why Romancy for AI Agents?

- **Durable LLM Calls**: Long-running LLM inference with automatic retry on failure
- **Multi-Step Reasoning**: Coordinate multiple AI tasks (research -> analysis -> synthesis)
- **Tool Usage Workflows**: Orchestrate AI agents calling external tools/APIs with crash recovery
- **Compensation on Failure**: Automatically rollback AI agent actions when workflows fail

### Example: Research Agent Workflow

```go
type ResearchResult struct {
	Research string `json:"research"`
}

type AnalysisResult struct {
	Analysis string `json:"analysis"`
}

type ReportResult struct {
	Report string `json:"report"`
}

var researchTopic = romancy.DefineActivity("research_topic",
	func(ctx context.Context, topic string) (ResearchResult, error) {
		// Call LLM to research a topic (may take minutes)
		result, err := llmClient.Generate(fmt.Sprintf("Research: %s", topic))
		if err != nil {
			return ResearchResult{}, err
		}
		return ResearchResult{Research: result}, nil
	},
)

var analyzeResearch = romancy.DefineActivity("analyze_research",
	func(ctx context.Context, research string) (AnalysisResult, error) {
		// Analyze research results with another LLM call
		analysis, err := llmClient.Generate(fmt.Sprintf("Analyze: %s", research))
		if err != nil {
			return AnalysisResult{}, err
		}
		return AnalysisResult{Analysis: analysis}, nil
	},
)

var synthesizeReport = romancy.DefineActivity("synthesize_report",
	func(ctx context.Context, analysis string) (ReportResult, error) {
		// Create final report
		report, err := llmClient.Generate(fmt.Sprintf("Report: %s", analysis))
		if err != nil {
			return ReportResult{}, err
		}
		return ReportResult{Report: report}, nil
	},
)

var aiResearchWorkflow = romancy.DefineWorkflow("ai_research_workflow",
	func(ctx *romancy.WorkflowContext, topic string) (ReportResult, error) {
		// Step 1: Research (may take 2-3 minutes)
		research, err := researchTopic.Execute(ctx, topic)
		if err != nil {
			return ReportResult{}, err
		}

		// Step 2: Analyze (if crash happens here, Step 1 won't re-run)
		analysis, err := analyzeResearch.Execute(ctx, research.Research)
		if err != nil {
			return ReportResult{}, err
		}

		// Step 3: Synthesize
		report, err := synthesizeReport.Execute(ctx, analysis.Analysis)
		if err != nil {
			return ReportResult{}, err
		}

		return report, nil
	},
)
```

**Key benefits**:

- If the workflow crashes during Step 2, Step 1 (research) won't re-run - cached results are used
- Each LLM call is automatically retried on transient failures
- Workflow state is persisted, allowing multi-hour AI workflows to survive restarts
- Compensation functions can undo AI agent actions (e.g., delete created resources) on failure

## Distributed Execution

Multiple workers can safely process workflows.

### Multi-Worker Setup

```go
// Worker 1, Worker 2, Worker 3 (same code on different machines)
app := romancy.NewApp(
	romancy.WithDatabase("postgres://yourdbinstance/workflows"), // Shared database
	romancy.WithWorkerID("worker-1"),                            // Unique per worker
)
```

**Features:**

- **Exclusive Execution**: Only one worker can execute a workflow instance at a time
- **Stale Lock Cleanup**: Automatic cleanup of locks from crashed workers (5-minute timeout)
- **Automatic Resume**: Crashed workflows resume on any available worker

### How It Works

```
Worker 1: Tries to acquire lock for workflow instance A
         -> Lock acquired
         -> Executes workflow

Worker 2: Tries to acquire lock for same instance A
         -> Lock already held by Worker 1
         -> Skips, moves to next instance

Worker 1: Crashes during execution

Cleanup Task: Detects stale lock (5 minutes old)
             -> Releases lock
             -> Marks workflow for resume

Worker 3: Acquires lock for instance A
         -> Replays from history
         -> Completes workflow
```

## Type Safety with Go Generics

Romancy uses Go generics for type-safe workflows:

```go
// Type-safe input/output
type OrderInput struct {
	OrderID       string  `json:"order_id"`
	CustomerEmail string  `json:"customer_email"`
	Amount        float64 `json:"amount"`
}

type OrderResult struct {
	OrderID string  `json:"order_id"`
	Status  string  `json:"status"`
	Total   float64 `json:"total"`
}

// Type-safe activity
var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, input OrderInput) (OrderResult, error) {
		// Input is typed
		total := input.Amount * 1.1 // Add 10% tax

		return OrderResult{
			OrderID: input.OrderID,
			Status:  "completed",
			Total:   total,
		}, nil
	},
)
```

**Benefits:**

- Compile-time type checking
- IDE autocomplete
- No runtime type assertions needed

## Next Steps

Now that you understand the core concepts:

- **[Your First Workflow](/docs/getting-started/first-workflow)**: Build a complete workflow step-by-step
- **[Saga Pattern](/docs/core-features/saga-compensation)**: Deep dive into compensation
- **[Durable Execution](/docs/core-features/durable-execution/replay)**: Technical details of replay
- **[Examples](/docs/examples/simple)**: Real-world examples
