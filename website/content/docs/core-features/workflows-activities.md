---
title: "Workflows & Activities"
weight: 1
---

This guide covers the basics of creating workflows and activities in Romancy.

## DefineWorkflow

`DefineWorkflow` creates a workflow orchestrator function.

### Basic Usage

```go
package main

import (
	"github.com/i2y/romancy"
)

type MyInput struct {
	Param1 string `json:"param1"`
	Param2 int    `json:"param2"`
}

type MyResult struct {
	Result string `json:"result"`
	Param2 int    `json:"param2"`
}

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, input MyInput) (MyResult, error) {
		// Orchestration logic here
		result, err := someActivity.Execute(ctx, input.Param1)
		if err != nil {
			return MyResult{}, err
		}
		return MyResult{Result: result, Param2: input.Param2}, nil
	},
)
```

### Options

| Option | Description |
|--------|-------------|
| `romancy.WithEventHandler(true)` | Automatically registers as CloudEvents handler |

### Starting Workflows

```go
// Start a workflow programmatically
instanceID, err := romancy.StartWorkflow(ctx, app, myWorkflow, MyInput{
	Param1: "hello",
	Param2: 42,
})
```

### CloudEvents Auto-Registration (Opt-in)

By default, workflows are NOT automatically registered as CloudEvents handlers (security).

To enable auto-registration:

```go
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		// This workflow will automatically handle CloudEvents
		// with type="order_workflow"
		return map[string]any{"status": "completed"}, nil
	},
	romancy.WithEventHandler(true), // Enable CloudEvents auto-registration
)
```

**How it works:**

1. CloudEvent arrives with `type="order_workflow"`
2. Romancy extracts `data` from the event
3. Workflow starts with `data` as parameters
4. Workflow instance ID is returned

**Security note:** Only use `WithEventHandler()` for workflows you want publicly accessible via CloudEvents.

## DefineActivity

`DefineActivity` creates an activity that performs business logic.

### Basic Usage

```go
var sendEmail = romancy.DefineActivity("send_email",
	func(ctx context.Context, email, subject string) (map[string]any, error) {
		// Call external service
		response, err := emailService.Send(email, subject)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"sent":       true,
			"message_id": response.ID,
		}, nil
	},
)
```

### Automatic Transactions

Activities are automatically transactional:

```go
var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		// All operations in a single transaction:
		// 1. Activity execution
		// 2. History recording
		// 3. Event publishing (if using transactional outbox)
		return map[string]any{"order_id": orderID}, nil
	},
)
```

### Custom Database Operations

For atomic operations with your own database tables, use the session from WorkflowContext:

```go
var createOrderWithDB = romancy.DefineActivity("create_order_with_db",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		// Access workflow context for session
		wfCtx := romancy.GetWorkflowContext(ctx)
		session := wfCtx.Session()

		// Your database operations
		order := &Order{OrderID: orderID}
		if err := session.Create(order).Error; err != nil {
			return nil, err
		}

		// Romancy automatically commits (or rolls back on error)
		return map[string]any{"order_id": orderID}, nil
	},
)
```

## Retry Policies

Activities automatically retry on failure with exponential backoff. This provides resilience against transient failures like network timeouts or temporary service unavailability.

### Default Retry Behavior

By default, activities retry **5 times** with exponential backoff:

```go
var callPaymentAPI = romancy.DefineActivity("call_payment_api",
	func(ctx context.Context, amount float64) (map[string]any, error) {
		// Default: 5 attempts with exponential backoff
		response, err := paymentService.Charge(amount)
		if err != nil {
			return nil, err
		}
		return map[string]any{"transaction_id": response.ID}, nil
	},
)
```

**Default schedule:**

- Attempt 1: Immediate execution
- Attempt 2: 1 second delay
- Attempt 3: 2 seconds delay
- Attempt 4: 4 seconds delay
- Attempt 5: 8 seconds delay

If all 5 attempts fail, a retry exhausted error is returned.

### Custom Retry Policies

Configure retry behavior per activity using `WithRetryPolicy`:

```go
var criticalPayment = romancy.DefineActivity("critical_payment",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		response, err := paymentService.Process(orderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": response.Status}, nil
	},
	romancy.WithRetryPolicy(&retry.Policy{
		MaxAttempts:     10,                      // More attempts for critical operations
		InitialInterval: 500 * time.Millisecond, // Faster initial retry
		Multiplier:      1.5,                    // Slower exponential growth
		MaxInterval:     30 * time.Second,       // Cap delay at 30 seconds
	}),
)
```

**retry.Policy fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `MaxAttempts` | `int` | `5` | Maximum retry attempts (0 = infinite) |
| `InitialInterval` | `time.Duration` | `1s` | First retry delay |
| `Multiplier` | `float64` | `2.0` | Exponential backoff multiplier |
| `MaxInterval` | `time.Duration` | `60s` | Maximum retry delay |

### Application-Level Default Policy

Set a default retry policy for all activities in your application:

```go
app := romancy.NewApp(
	romancy.WithDatabase("postgres://localhost/workflows"),
	romancy.WithDefaultRetryPolicy(&retry.Policy{
		MaxAttempts:     10,
		InitialInterval: 2 * time.Second,
		MaxInterval:     2 * time.Minute,
	}),
)
```

**Policy resolution order:**

1. Activity-level policy (highest priority)
2. Application-level policy
3. Framework default (5 attempts)

### Non-Retryable Errors

Use `TerminalError` for errors that should **never** be retried:

```go
var validateOrder = romancy.DefineActivity("validate_order",
	func(ctx context.Context, orderID string) (map[string]any, error) {
		order, err := db.GetOrder(orderID)
		if err != nil {
			return nil, err
		}

		if order == nil {
			// Don't retry - order doesn't exist
			return nil, romancy.TerminalError(fmt.Errorf("order %s not found", orderID))
		}

		if order.Status == "cancelled" {
			// Business rule violation - don't retry
			return nil, romancy.TerminalError(fmt.Errorf("order %s is cancelled", orderID))
		}

		return map[string]any{"order_id": orderID, "valid": true}, nil
	},
)
```

**When to use `TerminalError`:**

- ✅ Validation failures (invalid input, malformed data)
- ✅ Business rule violations (insufficient funds, order cancelled)
- ✅ Permanent errors (resource not found, access denied)
- ❌ Transient errors (network timeout, service unavailable) - let these retry!

### Built-in Retry Policies

Romancy provides helper functions for common scenarios:

```go
import "github.com/i2y/romancy/retry"

// Default policy (5 attempts, exponential backoff)
var defaultActivity = romancy.DefineActivity("default_activity",
	func(ctx context.Context, data string) (map[string]any, error) {
		return nil, nil
	},
	romancy.WithRetryPolicy(retry.DefaultPolicy()),
)

// No retries - fail immediately
var noRetryActivity = romancy.DefineActivity("no_retry_activity",
	func(ctx context.Context, data string) (map[string]any, error) {
		return nil, nil
	},
	romancy.WithRetryPolicy(retry.NoRetry()),
)

// Fixed interval retries
var fixedRetryActivity = romancy.DefineActivity("fixed_retry_activity",
	func(ctx context.Context, data string) (map[string]any, error) {
		return nil, nil
	},
	romancy.WithRetryPolicy(retry.Fixed(5, 2*time.Second)), // 5 attempts, 2s interval
)

// Exponential backoff
var exponentialActivity = romancy.DefineActivity("exponential_activity",
	func(ctx context.Context, data string) (map[string]any, error) {
		return nil, nil
	},
	romancy.WithRetryPolicy(retry.Exponential(10, 100*time.Millisecond, 30*time.Second)),
)
```

## Activity IDs and Deterministic Replay

Romancy automatically assigns IDs to activities for deterministic replay after crashes. Understanding when to use manual IDs vs. auto-generated IDs is important.

### Auto-Generated IDs (Default - Recommended)

For **sequential execution**, Romancy automatically generates IDs in the format `"{function_name}:{counter}"`:

```go
var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		// Auto-generated IDs: "validate:1", "process:1", "notify:1"
		result1, _ := validate.Execute(ctx, orderID)    // "validate:1"
		result2, _ := process.Execute(ctx, orderID)     // "process:1"
		result3, _ := notify.Execute(ctx, orderID)      // "notify:1"
		return map[string]any{"status": "completed"}, nil
	},
)
```

**How it works:**

- First call to `validate.Execute()` → `"validate:1"`
- Second call to `validate.Execute()` → `"validate:2"`
- First call to `process.Execute()` → `"process:1"`

**Even with conditional branches**, auto-generation works correctly:

```go
var loanApproval = romancy.DefineWorkflow("loan_approval",
	func(ctx *romancy.WorkflowContext, applicantID string) (map[string]any, error) {
		creditResult, _ := checkCredit.Execute(ctx, applicantID) // "check_credit:1"
		creditScore := creditResult["score"].(int)

		if creditScore >= 700 {
			result, _ := approve.Execute(ctx, applicantID)  // "approve:1"
			return result, nil
		} else {
			result, _ := reject.Execute(ctx, applicantID)   // "reject:1"
			return result, nil
		}
	},
)
```

### Manual IDs (Required for Concurrent Execution)

Manual activity ID specification is **required ONLY** for concurrent execution using goroutines:

```go
var concurrentWorkflow = romancy.DefineWorkflow("concurrent_workflow",
	func(ctx *romancy.WorkflowContext, urls []string) (map[string]any, error) {
		// Manual IDs required for concurrent execution
		var wg sync.WaitGroup
		results := make([]map[string]any, len(urls))

		for i, url := range urls {
			wg.Add(1)
			go func(idx int, u string) {
				defer wg.Done()
				// Use WithActivityID for concurrent execution
				result, _ := fetchData.Execute(ctx, u,
					romancy.WithActivityID(fmt.Sprintf("fetch_data:%d", idx+1)))
				results[idx] = result
			}(i, url)
		}

		wg.Wait()
		return map[string]any{"results": results}, nil
	},
)
```

**When manual IDs are required:**

- Goroutine-based concurrent execution
- Any scenario where execution order is non-deterministic

### Best Practices

✅ **Do:** Rely on auto-generation for sequential execution
```go
result1, _ := activityOne.Execute(ctx, data)
result2, _ := activityTwo.Execute(ctx, data)
```

❌ **Don't:** Manually specify IDs for sequential execution
```go
// Unnecessary - adds noise
result1, _ := activityOne.Execute(ctx, data, romancy.WithActivityID("activity_one:1"))
result2, _ := activityTwo.Execute(ctx, data, romancy.WithActivityID("activity_two:1"))
```

## Workflow vs. Activity: When to Use Which?

### Use `DefineWorkflow` for:

- ✅ Orchestrating multiple steps
- ✅ Coordinating activities
- ✅ Defining business processes
- ✅ Decision logic (if/else, loops)

**Example:**

```go
var userOnboarding = romancy.DefineWorkflow("user_onboarding",
	func(ctx *romancy.WorkflowContext, userID string) (map[string]any, error) {
		// Orchestration logic
		account, _ := createAccount.Execute(ctx, userID)
		sendWelcomeEmail.Execute(ctx, account["email"].(string))
		setupPreferences.Execute(ctx, userID)
		return map[string]any{"status": "completed"}, nil
	},
)
```

### Use `DefineActivity` for:

- ✅ Database writes
- ✅ API calls
- ✅ File I/O
- ✅ External service calls
- ✅ Any side-effecting operation

**Example:**

```go
var createAccount = romancy.DefineActivity("create_account",
	func(ctx context.Context, userID string) (map[string]any, error) {
		// Business logic
		account, err := db.CreateUser(userID)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"account_id": account.ID,
			"email":      account.Email,
		}, nil
	},
)
```

## Complete Example

Here's a complete example showing workflows and activities together:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

// Data models
type UserInput struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

type UserResult struct {
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	Status    string `json:"status"`
}

// Activities
var createDatabaseRecord = romancy.DefineActivity("create_database_record",
	func(ctx context.Context, userID, email, name string) (map[string]any, error) {
		fmt.Printf("Creating user %s in database\n", userID)
		// Simulate database write
		return map[string]any{
			"account_id": fmt.Sprintf("ACC-%s", userID),
			"email":      email,
			"name":       name,
		}, nil
	},
)

var sendWelcomeEmail = romancy.DefineActivity("send_welcome_email",
	func(ctx context.Context, email, name string) (map[string]any, error) {
		fmt.Printf("Sending welcome email to %s\n", email)
		// Simulate email service
		return map[string]any{"sent": true, "email": email}, nil
	},
)

var createUserProfile = romancy.DefineActivity("create_user_profile",
	func(ctx context.Context, accountID, name string) (map[string]any, error) {
		fmt.Printf("Creating profile for %s\n", accountID)
		// Simulate profile creation
		return map[string]any{
			"profile_id": fmt.Sprintf("PROF-%s", accountID),
			"settings":   map[string]any{"theme": "light", "notifications": true},
		}, nil
	},
)

// Workflow
var userRegistrationWorkflow = romancy.DefineWorkflow("user_registration",
	func(ctx *romancy.WorkflowContext, input UserInput) (UserResult, error) {
		// Step 1: Database record
		account, err := createDatabaseRecord.Execute(ctx, input.UserID, input.Email, input.Name)
		if err != nil {
			return UserResult{}, err
		}

		// Step 2: Welcome email
		_, err = sendWelcomeEmail.Execute(ctx, account["email"].(string), account["name"].(string))
		if err != nil {
			return UserResult{}, err
		}

		// Step 3: User profile
		_, err = createUserProfile.Execute(ctx, account["account_id"].(string), account["name"].(string))
		if err != nil {
			return UserResult{}, err
		}

		return UserResult{
			UserID:    input.UserID,
			AccountID: account["account_id"].(string),
			Status:    "completed",
		}, nil
	},
)

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("users.db"),
		romancy.WithWorkerID("worker-1"),
	)

	ctx := context.Background()

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, userRegistrationWorkflow, UserInput{
		UserID: "user_123",
		Email:  "user@example.com",
		Name:   "John Doe",
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Workflow started: %s\n", instanceID)
}
```

## Best Practices

### 1. Keep Workflows Simple

✅ **Good:**

```go
var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		inventory, _ := reserveInventory.Execute(ctx, orderID)
		payment, _ := processPayment.Execute(ctx, inventory["total"].(float64))
		shipOrder.Execute(ctx, orderID)
		return map[string]any{"status": "completed"}, nil
	},
)
```

❌ **Bad:**

```go
var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, orderID string) (map[string]any, error) {
		// Don't put business logic in workflows!
		inventoryData := db.Query("SELECT ...")  // ❌
		total := calculateTotal(inventoryData)    // ❌
		externalAPI.Call(...)                     // ❌
		return map[string]any{"status": "completed"}, nil
	},
)
```

### 2. Activities Should Be Focused

✅ **Good:**

```go
var sendEmail = romancy.DefineActivity("send_email",
	func(ctx context.Context, email, subject string) (map[string]any, error) {
		// Single responsibility: send email
		response, err := emailService.Send(email, subject)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sent": true}, nil
	},
)
```

❌ **Bad:**

```go
var sendEmailAndUpdateDBAndLog = romancy.DefineActivity("send_email_and_update_db_and_log",
	func(ctx context.Context, ...) (map[string]any, error) {
		// Too many responsibilities!
		emailService.Send(...)
		db.Update(...)
		logger.Log(...)
		// Break this into 3 separate activities!
		return nil, nil
	},
)
```

### 3. Use Go Structs for Type Safety

✅ **Good:**

```go
type OrderInput struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input OrderInput) (map[string]any, error) {
		// Type-safe, structured input
		return nil, nil
	},
)
```

❌ **Bad:**

```go
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string, amount float64) (map[string]any, error) {
		// Multiple parameters - harder to extend, no validation
		return nil, nil
	},
)
```

## Managing Workflow Instances

Romancy provides several methods for querying and managing workflow instances.

### Get a Specific Instance

```go
instance, err := app.GetInstance(ctx, "wf_abc123")
if err != nil {
    // Handle error
}
if instance == nil {
    // Instance not found
}
fmt.Println(instance.Status, instance.WorkflowName)
```

### List Instances with Filters

```go
import "github.com/i2y/romancy/internal/storage"

result, err := app.ListInstances(ctx, storage.ListInstancesOptions{
    WorkflowNameFilter: "order",      // Partial match
    StatusFilter:       "running",    // Exact match
    InstanceIDFilter:   "order_",     // Partial match
    Limit:             50,            // Page size (default: 50)
})
if err != nil {
    // Handle error
}

for _, inst := range result.Instances {
    fmt.Println(inst.InstanceID, inst.Status)
}

// Paginate
if result.HasMore {
    nextResult, _ := app.ListInstances(ctx, storage.ListInstancesOptions{
        PageToken: result.NextPageToken,
    })
}
```

### Find Instances by Input Data

Query workflow instances by values in their input data:

```go
// Find orders for a specific customer
instances, err := app.FindInstances(ctx, map[string]any{
    "customer_id": "cust_123",
})

// Find orders with specific status in input
instances, err := app.FindInstances(ctx, map[string]any{
    "order.status": "pending",
    "order.priority": "high",
})
```

#### JSON Path Syntax

Input filters support dot-notation for nested fields:

| Pattern | Matches |
|---------|---------|
| `"customer_id"` | Top-level field |
| `"order.id"` | Nested field `{"order": {"id": ...}}` |
| `"items.0.sku"` | Array access (if supported by DB) |

#### Full Options

```go
result, err := app.FindInstancesWithOptions(ctx, storage.ListInstancesOptions{
    InputFilters: map[string]any{
        "customer.id": "cust_123",
        "order.type": "subscription",
    },
    StatusFilter:       storage.StatusRunning,  // Optional: filter by status
    WorkflowNameFilter: "order",                // Optional: filter by workflow name
    Limit:              100,
})
```

#### Supported Value Types

- **Strings**: Exact match
- **Numbers**: Numeric comparison
- **Booleans**: True/false
- **Null**: Null comparison

#### Database Support

| Database | JSON Path Support |
|----------|-------------------|
| SQLite | `json_extract()` |
| PostgreSQL | `#>>` operator |
| MySQL | `JSON_EXTRACT()` |

## Next Steps

- **[Durable Execution](/docs/core-features/durable-execution/replay)**: Learn how Romancy ensures workflows never lose progress
- **[Saga Pattern](/docs/core-features/saga-compensation)**: Automatic compensation on failure
- **[Event Handling](/docs/core-features/events/wait-event)**: Wait for external events in workflows
- **[Examples](/docs/examples/simple)**: See workflows and activities in action
