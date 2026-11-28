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

A workflow runs only on apps that registered it. Register it before `app.Start`; an instance started on an app that did not register its workflow stays `running` until a worker that registered it picks it up.

```go
// Register the workflow with the app (before app.Start)
romancy.RegisterWorkflow(app, myWorkflow)

// Start a workflow programmatically
instanceID, err := romancy.StartWorkflow(ctx, app, myWorkflow, MyInput{
	Param1: "hello",
	Param2: 42,
})
if err != nil {
	panic(err)
}
fmt.Println("started", instanceID)
```

### CloudEvents Auto-Registration (Opt-in)

By default, workflows are NOT automatically registered as CloudEvents handlers (security).

To enable auto-registration:

```go
type OrderResult struct {
	Status  string `json:"status"`
	OrderID string `json:"order_id"`
}

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderResult, error) {
		// This workflow will automatically handle CloudEvents
		// with type="order_workflow"
		return OrderResult{Status: "completed", OrderID: orderID}, nil
	},
)

// Enable CloudEvents auto-registration when registering the workflow.
func registerOrderWorkflow(app *romancy.App) {
	romancy.RegisterWorkflow(app, orderWorkflow, romancy.WithEventHandler(true))
}
```

**How it works:**

1. CloudEvent arrives with `type="order_workflow"`
2. Romancy extracts `data` from the event
3. Workflow starts with `data` as parameters
4. Workflow instance ID is returned

An event with the `romancyinstanceid` extension attribute is for that instance only: it starts no workflow.

**Security note:** Only use `WithEventHandler()` for workflows you want publicly accessible via CloudEvents.

## DefineActivity

`DefineActivity` creates an activity that performs business logic.

### Basic Usage

```go
type EmailInput struct {
	Email   string `json:"email"`
	Subject string `json:"subject"`
}

type EmailResult struct {
	Sent      bool   `json:"sent"`
	MessageID string `json:"message_id"`
}

var sendEmail = romancy.DefineActivity("send_email",
	func(ctx context.Context, input EmailInput) (EmailResult, error) {
		// Call external service
		response, err := emailService.Send(input.Email, input.Subject)
		if err != nil {
			return EmailResult{}, err
		}
		return EmailResult{
			Sent:      true,
			MessageID: response.ID,
		}, nil
	},
)
```

### Automatic Transactions

Activities are automatically transactional:

```go
type OrderResult struct {
	OrderID string `json:"order_id"`
}

var createOrder = romancy.DefineActivity("create_order",
	func(ctx context.Context, orderID string) (OrderResult, error) {
		// All operations in a single transaction:
		// 1. Activity execution
		// 2. History recording
		// 3. Event publishing (if using transactional outbox)
		return OrderResult{OrderID: orderID}, nil
	},
)
```

### Custom Database Operations

For atomic operations with your own database tables, use the session from WorkflowContext:

```go
var createOrderWithDB = romancy.DefineActivity("create_order_with_db",
	func(ctx context.Context, orderID string) (OrderResult, error) {
		// Access workflow context for session
		wfCtx := romancy.GetWorkflowContext(ctx)
		session := wfCtx.Session()

		// Your database operations run in the activity transaction.
		if _, err := session.ExecContext(ctx, "INSERT INTO orders (order_id) VALUES (?)", orderID); err != nil {
			return OrderResult{}, err
		}

		// Romancy automatically commits (or rolls back on error)
		return OrderResult{OrderID: orderID}, nil
	},
)
```

### Calling an Activity from Another Activity

An activity called inside another activity's body is part of the outer activity. Call it with the context the outer function receives (`romancy.GetWorkflowContext(ctx)`); its function then runs directly, in the outer attempt's transaction:

```go
var reserveItem = romancy.DefineActivity("reserve_item",
	func(ctx context.Context, itemID string) (string, error) {
		session := romancy.GetWorkflowContext(ctx).Session()
		if _, err := session.ExecContext(ctx, "INSERT INTO reservations (item_id) VALUES (?)", itemID); err != nil {
			return "", err
		}
		return itemID, nil
	},
)

var reserveOrder = romancy.DefineActivity("reserve_order",
	func(ctx context.Context, itemIDs []string) ([]string, error) {
		wfCtx := romancy.GetWorkflowContext(ctx)
		reserved := make([]string, 0, len(itemIDs))
		for _, itemID := range itemIDs {
			// Runs as part of reserve_order, in its transaction
			id, err := reserveItem.Execute(wfCtx, itemID)
			if err != nil {
				return nil, err
			}
			reserved = append(reserved, id)
		}
		return reserved, nil
	},
)
```

- The nested call takes no activity ID and is not recorded in history; `WithActivityID` on it is ignored. Only `reserve_order` is recorded, and a replay returns its result without running its body.
- The nested call's writes commit or roll back with the outer activity. `WithTransactional` on the nested activity has no effect: it uses the outer activity's transaction, or none when the outer activity is not transactional.
- An error from the nested call is an error of the outer attempt. The outer activity's retry policy decides whether the whole attempt runs again. The nested activity's own retry policy, timeout and compensation do not apply, and no activity hooks run for it.
- An activity called from a compensation function runs the same way.

To get two steps that are recorded and retried separately, call both activities from the workflow.

Before v0.7.3 the nested call took an ID from the workflow's counter and committed in a transaction of its own. A replay does not run the outer activity's body, so the IDs of later calls shifted, and a later call of the same activity from the workflow could receive the nested call's result. History that already holds such a record keeps it.

## Retry Policies

Activities automatically retry on failure with exponential backoff. This provides resilience against transient failures like network timeouts or temporary service unavailability.

### Default Retry Behavior

By default (`retry.DefaultPolicy()`), an activity makes up to **3 attempts** with exponential backoff:

```go
type PaymentResult struct {
	TransactionID string `json:"transaction_id"`
}

var callPaymentAPI = romancy.DefineActivity("call_payment_api",
	func(ctx context.Context, amount float64) (PaymentResult, error) {
		// Default: 3 attempts with exponential backoff
		response, err := paymentService.Charge(amount)
		if err != nil {
			return PaymentResult{}, err
		}
		return PaymentResult{TransactionID: response.ID}, nil
	},
)
```

**Default schedule:**

- Attempt 1: Immediate execution
- Attempt 2: after about 100 ms
- Attempt 3: after about 200 ms

Each delay varies by ±50%. If all 3 attempts fail, a retry exhausted error is returned.

### Custom Retry Policies

Configure retry behavior per activity using `WithRetryPolicy`:

```go
type PaymentStatusResult struct {
	Status string `json:"status"`
}

var criticalPayment = romancy.DefineActivity("critical_payment",
	func(ctx context.Context, orderID string) (PaymentStatusResult, error) {
		response, err := paymentService.Process(orderID)
		if err != nil {
			return PaymentStatusResult{}, err
		}
		return PaymentStatusResult{Status: response.Status}, nil
	},
	romancy.WithRetryPolicy[string, PaymentStatusResult](&retry.Policy{
		MaxAttempts:     10,                      // More attempts for critical operations
		InitialInterval: 500 * time.Millisecond, // Faster initial retry
		Multiplier:      1.5,                    // Slower exponential growth
		MaxInterval:     30 * time.Second,       // Cap delay at 30 seconds
	}),
)
```

**retry.Policy fields:**

| Field | Type | When zero | Description |
|-------|------|-----------|-------------|
| `MaxAttempts` | `int` | No limit | Maximum attempts, including the first |
| `InitialInterval` | `time.Duration` | Retry at once | Delay before the first retry |
| `Multiplier` | `float64` | `2.0` | Exponential backoff multiplier |
| `MaxInterval` | `time.Duration` | No maximum | Maximum retry delay |
| `RandomizationFactor` | `float64` | No jitter | `0.5` varies each delay by ±50% |
| `NonRetryableErrors` | `[]error` | Every error is retried | Errors (matched with `errors.Is`) that end the retries |

### Shared Retry Policy

Romancy does not have an application-level retry default. Define a shared policy and pass it to each activity:

```go
// One policy for the activities that call the payment provider
var paymentRetry = &retry.Policy{
	MaxAttempts:     10,
	InitialInterval: 2 * time.Second,
	MaxInterval:     2 * time.Minute,
}

var chargeCard = romancy.DefineActivity("charge_card",
	func(ctx context.Context, orderID string) (ProcessResult, error) {
		return ProcessResult{Processed: true}, nil
	},
	romancy.WithRetryPolicy[string, ProcessResult](paymentRetry),
)
```

**Policy resolution order:**

1. The activity's own policy (`WithRetryPolicy`)
2. Otherwise `retry.DefaultPolicy()` (3 attempts)

### Non-Retryable Errors

Use `TerminalError` for errors that should **never** be retried:

```go
type ValidationResult struct {
	OrderID string `json:"order_id"`
	Valid   bool   `json:"valid"`
}

var validateOrder = romancy.DefineActivity("validate_order",
	func(ctx context.Context, orderID string) (ValidationResult, error) {
		order, err := db.GetOrder(orderID)
		if err != nil {
			return ValidationResult{}, err
		}

		if order == nil {
			// Don't retry - order doesn't exist
			return ValidationResult{}, romancy.NewTerminalError(fmt.Errorf("order %s not found", orderID))
		}

		if order.Status == "cancelled" {
			// Business rule violation - don't retry
			return ValidationResult{}, romancy.NewTerminalError(fmt.Errorf("order %s is cancelled", orderID))
		}

		return ValidationResult{OrderID: orderID, Valid: true}, nil
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

type ProcessResult struct {
	Processed bool `json:"processed"`
}

// Default policy (3 attempts, exponential backoff)
var defaultActivity = romancy.DefineActivity("default_activity",
	func(ctx context.Context, data string) (ProcessResult, error) {
		return ProcessResult{Processed: true}, nil
	},
	romancy.WithRetryPolicy[string, ProcessResult](retry.DefaultPolicy()),
)

// No retries - fail immediately
var noRetryActivity = romancy.DefineActivity("no_retry_activity",
	func(ctx context.Context, data string) (ProcessResult, error) {
		return ProcessResult{Processed: true}, nil
	},
	romancy.WithRetryPolicy[string, ProcessResult](retry.NoRetry()),
)

// Fixed interval retries
var fixedRetryActivity = romancy.DefineActivity("fixed_retry_activity",
	func(ctx context.Context, data string) (ProcessResult, error) {
		return ProcessResult{Processed: true}, nil
	},
	romancy.WithRetryPolicy[string, ProcessResult](retry.Fixed(5, 2*time.Second)), // 5 attempts, 2s interval
)

// Exponential backoff
var exponentialActivity = romancy.DefineActivity("exponential_activity",
	func(ctx context.Context, data string) (ProcessResult, error) {
		return ProcessResult{Processed: true}, nil
	},
	romancy.WithRetryPolicy[string, ProcessResult](retry.Exponential(10, 100*time.Millisecond, 30*time.Second, 2.0)),
)
```

## Activity IDs and Deterministic Replay

Romancy automatically assigns IDs to activities for deterministic replay after crashes. Understanding when to use manual IDs vs. auto-generated IDs is important.

### Auto-Generated IDs (Default - Recommended)

For **sequential execution**, Romancy automatically generates IDs in the format `"{function_name}:{counter}"`:

```go
type WorkflowResult struct {
	Status string `json:"status"`
}

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (WorkflowResult, error) {
		// Auto-generated IDs: "validate:1", "process:1", "notify:1"
		_, _ = validate.Execute(ctx, orderID)    // "validate:1"
		_, _ = process.Execute(ctx, orderID)     // "process:1"
		_, _ = notify.Execute(ctx, orderID)      // "notify:1"
		return WorkflowResult{Status: "completed"}, nil
	},
)
```

**How it works:**

- First call to `validate.Execute()` → `"validate:1"`
- Second call to `validate.Execute()` → `"validate:2"`
- First call to `process.Execute()` → `"process:1"`

**Even with conditional branches**, auto-generation works correctly:

```go
type CreditCheckResult struct {
	Score int `json:"score"`
}

type LoanResult struct {
	Status      string `json:"status"`
	ApplicantID string `json:"applicant_id"`
}

var loanApproval = romancy.DefineWorkflow("loan_approval",
	func(ctx *romancy.WorkflowContext, applicantID string) (LoanResult, error) {
		creditResult, _ := checkCredit.Execute(ctx, applicantID) // "check_credit:1"

		if creditResult.Score >= 700 {
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
type FetchResult struct {
	URL     string `json:"url"`
	Content string `json:"content"`
}

type ConcurrentResult struct {
	Results []FetchResult `json:"results"`
}

var concurrentWorkflow = romancy.DefineWorkflow("concurrent_workflow",
	func(ctx *romancy.WorkflowContext, urls []string) (ConcurrentResult, error) {
		// Manual IDs required for concurrent execution
		var wg sync.WaitGroup
		results := make([]FetchResult, len(urls))
		errs := make([]error, len(urls))

		for i, url := range urls {
			wg.Add(1)
			go func(idx int, u string) {
				defer wg.Done()
				// An explicit ID that no auto-generated one ("fetch_data:1", ...) can take
				results[idx], errs[idx] = fetchData.Execute(ctx, u,
					romancy.WithActivityID(fmt.Sprintf("fetch_data:url-%d", idx+1)))
			}(i, url)
		}

		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return ConcurrentResult{}, err
			}
		}
		return ConcurrentResult{Results: results}, nil
	},
)
```

**When manual IDs are required:**

- Goroutine-based concurrent execution
- Any scenario where execution order is non-deterministic

**Choosing manual IDs:** auto-generated IDs are `{activity name}:{n}`, and a manual ID does not advance the count. A manual `fetch_data:1` would therefore also be the ID of the next `fetchData.Execute(ctx, url)` without one, which would return the earlier call's result without running. Use IDs an auto-generated one cannot be, such as `fetch_data:url-1`, unique within the instance. Romancy logs a warning (`activity ID used twice in one run`) when a run uses an ID twice. Run only activities from goroutines: call `Sleep`, `WaitEvent`, `Receive`, `Publish` and the other workflow APIs from the workflow function itself (see [Deterministic Replay](/docs/core-features/durable-execution/replay)).

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
type OnboardingResult struct {
	Status string `json:"status"`
}

type AccountResult struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
}

var userOnboarding = romancy.DefineWorkflow("user_onboarding",
	func(ctx *romancy.WorkflowContext, userID string) (OnboardingResult, error) {
		// Orchestration logic
		account, _ := createAccount.Execute(ctx, userID)
		_, _ = sendWelcomeEmail.Execute(ctx, account.Email)
		_, _ = setupPreferences.Execute(ctx, userID)
		return OnboardingResult{Status: "completed"}, nil
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
	func(ctx context.Context, userID string) (AccountResult, error) {
		// Business logic
		account, err := db.CreateUser(userID)
		if err != nil {
			return AccountResult{}, err
		}
		return AccountResult{
			AccountID: account.ID,
			Email:     account.Email,
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

type DatabaseRecordResult struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

type WelcomeEmailResult struct {
	Sent  bool   `json:"sent"`
	Email string `json:"email"`
}

type ProfileSettings struct {
	Theme         string `json:"theme"`
	Notifications bool   `json:"notifications"`
}

type UserProfileResult struct {
	ProfileID string          `json:"profile_id"`
	Settings  ProfileSettings `json:"settings"`
}

// Activities
var createDatabaseRecord = romancy.DefineActivity("create_database_record",
	func(ctx context.Context, input UserInput) (DatabaseRecordResult, error) {
		fmt.Printf("Creating user %s in database\n", input.UserID)
		// Simulate database write
		return DatabaseRecordResult{
			AccountID: fmt.Sprintf("ACC-%s", input.UserID),
			Email:     input.Email,
			Name:      input.Name,
		}, nil
	},
)

type WelcomeEmailInput struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

var sendWelcomeEmail = romancy.DefineActivity("send_welcome_email",
	func(ctx context.Context, input WelcomeEmailInput) (WelcomeEmailResult, error) {
		fmt.Printf("Sending welcome email to %s\n", input.Email)
		// Simulate email service
		return WelcomeEmailResult{Sent: true, Email: input.Email}, nil
	},
)

type CreateProfileInput struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
}

var createUserProfile = romancy.DefineActivity("create_user_profile",
	func(ctx context.Context, input CreateProfileInput) (UserProfileResult, error) {
		fmt.Printf("Creating profile for %s\n", input.AccountID)
		// Simulate profile creation
		return UserProfileResult{
			ProfileID: fmt.Sprintf("PROF-%s", input.AccountID),
			Settings:  ProfileSettings{Theme: "light", Notifications: true},
		}, nil
	},
)

// Workflow
var userRegistrationWorkflow = romancy.DefineWorkflow("user_registration",
	func(ctx *romancy.WorkflowContext, input UserInput) (UserResult, error) {
		// Step 1: Database record
		account, err := createDatabaseRecord.Execute(ctx, input)
		if err != nil {
			return UserResult{}, err
		}

		// Step 2: Welcome email
		_, err = sendWelcomeEmail.Execute(ctx, WelcomeEmailInput{Email: account.Email, Name: account.Name})
		if err != nil {
			return UserResult{}, err
		}

		// Step 3: User profile
		_, err = createUserProfile.Execute(ctx, CreateProfileInput{AccountID: account.AccountID, Name: account.Name})
		if err != nil {
			return UserResult{}, err
		}

		return UserResult{
			UserID:    input.UserID,
			AccountID: account.AccountID,
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

	romancy.RegisterWorkflow(app, userRegistrationWorkflow)

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
type OrderProcessResult struct {
	Status string `json:"status"`
}

type InventoryResult struct {
	Total float64 `json:"total"`
}

var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderProcessResult, error) {
		inventory, err := reserveInventory.Execute(ctx, orderID)
		if err != nil {
			return OrderProcessResult{}, err
		}
		if _, err := processPayment.Execute(ctx, inventory.Total); err != nil {
			return OrderProcessResult{}, err
		}
		if _, err := shipOrder.Execute(ctx, orderID); err != nil {
			return OrderProcessResult{}, err
		}
		return OrderProcessResult{Status: "completed"}, nil
	},
)
```

❌ **Bad:**

```go
var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderProcessResult, error) {
		// Don't put business logic in workflows!
		inventoryData, _ := db.QueryContext(ctx.Context(), "SELECT quantity FROM inventory") // ❌
		if calculateTotal(inventoryData) > 0 {                                                // ❌
			externalAPI.Call("reserve") // ❌
		}
		return OrderProcessResult{Status: "completed"}, nil
	},
)
```

### 2. Activities Should Be Focused

✅ **Good:**

```go
type SendEmailResult struct {
	Sent bool `json:"sent"`
}

var sendEmail = romancy.DefineActivity("send_email",
	func(ctx context.Context, input EmailInput) (SendEmailResult, error) {
		// Single responsibility: send email
		_, err := emailService.Send(input.Email, input.Subject)
		if err != nil {
			return SendEmailResult{}, err
		}
		return SendEmailResult{Sent: true}, nil
	},
)
```

❌ **Bad:**

```go
var sendEmailAndUpdateDBAndLog = romancy.DefineActivity("send_email_and_update_db_and_log",
	func(ctx context.Context, input EmailInput) (SendEmailResult, error) {
		// Too many responsibilities!
		emailService.Send(input.Email, input.Subject)
		db.ExecContext(ctx, "UPDATE users SET welcomed = true")
		logger.Println("sent")
		// Break this into 3 separate activities!
		return SendEmailResult{Sent: true}, nil
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

type OrderWorkflowResult struct {
	Status  string `json:"status"`
	OrderID string `json:"order_id"`
}

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderWorkflowResult, error) {
		// Type-safe, structured input and output
		return OrderWorkflowResult{Status: "completed", OrderID: input.OrderID}, nil
	},
)
```

❌ **Bad:**

```go
// Romancy workflows take one input value. Use a struct rather than
// multiple positional parameters; positional parameters are not supported.
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, input OrderInput) (OrderWorkflowResult, error) {
		return OrderWorkflowResult{Status: "completed", OrderID: input.OrderID}, nil
	},
)
```

## Managing Workflow Instances

Romancy provides several methods for querying and managing workflow instances.

### Get a Specific Instance

```go
instance, err := app.GetInstance(ctx, "wf_abc123")
var notFound *romancy.WorkflowNotFoundError
if errors.As(err, &notFound) {
    // Instance not found
} else if err != nil {
    // Handle error
} else {
    fmt.Println(instance.Status, instance.WorkflowName)
}
```

### List Instances with Filters

```go
instances, err := app.FindInstancesWithOptions(ctx, romancy.ListInstancesOptions{
    WorkflowNameFilter: "order",               // Partial match
    StatusFilter:       romancy.StatusRunning, // Exact match
    InstanceIDFilter:   "order_",              // Partial match
    Limit:              50,                    // Page size (default: 50)
})
if err != nil {
    // Handle error
}
for _, inst := range instances {
    fmt.Println(inst.InstanceID, inst.Status)
}
```

To page through more instances, list them from the storage, which returns a token for the next page:

```go
page, err := app.Storage().ListInstances(ctx, romancy.ListInstancesOptions{Limit: 50})
for err == nil {
    for _, inst := range page.Instances {
        fmt.Println(inst.InstanceID, inst.Status)
    }
    if !page.HasMore {
        break
    }
    page, err = app.Storage().ListInstances(ctx, romancy.ListInstancesOptions{
        Limit:     50,
        PageToken: page.NextPageToken,
    })
}
if err != nil {
    // Handle error
}
```

From a shell, the CLI lists instances the same way: `romancy list --db workflow.db --status running`, with `--page-token` for the next page.

### Find Instances by Input Data

Query workflow instances by values in their input data:

```go
// Find orders for a specific customer
instances, err := app.FindInstances(ctx, map[string]any{
    "customer_id": "cust_123",
})

// Find orders with specific status in input
instances, err = app.FindInstances(ctx, map[string]any{
    "order.status": "pending",
    "order.priority": "high",
})
if err != nil {
    panic(err)
}
fmt.Println(len(instances))
```

#### JSON Path Syntax

Input filters support dot-notation for nested fields:

| Pattern | Matches |
|---------|---------|
| `"customer_id"` | Top-level field |
| `"order.id"` | Nested field `{"order": {"id": ...}}` |
| `"items.0.sku"` | Array access (if supported by DB) |

#### Full Options

Combine input filters with the other filters:

```go
instances, err := app.FindInstancesWithOptions(ctx, romancy.ListInstancesOptions{
    InputFilters: map[string]any{
        "customer.id": "cust_123",
        "order.type":  "subscription",
    },
    StatusFilter:       romancy.StatusRunning, // Optional: filter by status
    WorkflowNameFilter: "order",               // Optional: filter by workflow name
    Limit:              100,
})
if err != nil {
    // Handle error
}
for _, inst := range instances {
    fmt.Println(inst.InstanceID, inst.Status)
}
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
