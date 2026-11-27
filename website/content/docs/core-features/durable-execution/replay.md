---
title: "Deterministic Replay"
weight: 1
---

Romancy uses a **deterministic replay mechanism** to ensure workflows never lose progress. This document explains how workflows are resumed after interruption and how state is restored.

## Overview

Romancy's replay mechanism has three key characteristics:

1. **Completed activities are skipped**: Already-executed activities return cached results from history
2. **Workflow code runs fully**: Control flow and calculations between activities execute every time
3. **State restoration from history**: Workflow state is restored from the persisted execution history

## How Replay Works

### Activity Execution Flow

When an activity is called during replay:

1. **Activity ID Resolution**: Auto-generated (`function_name:counter`) or explicitly provided
2. **Cache Check**: If replaying, check if result is cached for this `activity_id`
3. **Return Cached**: If found, return cached result **without executing the function**
4. **Execute**: If not cached, run the function and record the result
5. **Error Handling**: When an activity gives up (a `TerminalError`, or its retry policy is exhausted), its failure is recorded, for transactional activities too; a replay returns the recorded failure without running the function (see [Failures are recorded](/docs/core-features/retry#failures-are-recorded))

### Activity ID Patterns

Activities are identified by unique IDs in the format `function_name:counter`.

**Sequential Execution (Auto-generated IDs):**

```go
// First call: auto-generates "reserve_inventory:1"
inventory, err := reserveInventory.Execute(ctx, orderID)
if err != nil {
    return nil, err
}

// Second call: auto-generates "reserve_inventory:2"
backupInventory, err := reserveInventory.Execute(ctx, backupOrderID)
if err != nil {
    return nil, err
}
```

**Conditional Execution (Auto-generated IDs):**

```go
// Execution order is deterministic, so auto-generated IDs work fine
if requiresApproval {
    result, err = approveOrder.Execute(ctx, orderID)  // Auto: "approve_order:1"
} else {
    result, err = autoApprove.Execute(ctx, orderID)   // Auto: "auto_approve:1"
}
```

**Loop Execution (Auto-generated IDs):**

```go
// Execution order is deterministic (same order every replay)
for _, item := range items {
    _, err := processItem.Execute(ctx, item)  // Auto: "process_item:1", "process_item:2", ...
    if err != nil {
        return nil, err
    }
}
```

**Concurrent Execution (Explicit IDs Required):**

```go
import "golang.org/x/sync/errgroup"

type ProcessResult struct {
    Status string `json:"status"`
    Data   string `json:"data"`
}

// The goroutines run in any order, so each activity gets an explicit ID
func processConcurrently(ctx *romancy.WorkflowContext, data string) ([3]ProcessResult, error) {
    var eg errgroup.Group
    var results [3]ProcessResult

    eg.Go(func() error {
        r, err := processA.Execute(ctx, data, romancy.WithActivityID("process_a:parallel"))
        results[0] = r
        return err
    })

    eg.Go(func() error {
        r, err := processB.Execute(ctx, data, romancy.WithActivityID("process_b:parallel"))
        results[1] = r
        return err
    })

    eg.Go(func() error {
        r, err := processC.Execute(ctx, data, romancy.WithActivityID("process_c:parallel"))
        results[2] = r
        return err
    })

    if err := eg.Wait(); err != nil {
        return [3]ProcessResult{}, err
    }
    return results, nil
}
```

- Give each activity call that runs concurrently its own ID with `romancy.WithActivityID`. Auto-generated IDs follow the order of the calls, which goroutines do not keep.
- Choose IDs that an auto-generated one can never be. Auto IDs are `{activity name}:{n}`, and an explicit ID does not advance the count: with `process_a:1` as the explicit ID, the next `processA.Execute(ctx, data)` without one would also be `process_a:1`, and would return this call's result without running. An ID is used once per instance, so a helper called more than once needs a part that differs per call, such as a batch number. Romancy logs a warning (`activity ID used twice in one run`) when a run uses an ID twice.
- Run only activities from the goroutines. Call `Sleep`, `WaitEvent`, `Receive`, `Publish` and the other workflow APIs from the workflow function itself: their history IDs also follow the order of the calls.
- Each activity runs in its own transaction. SQLite lets one transaction write at a time, so activities that write take turns there.

### How Replay Works Internally

When an activity is called:

1. **Resolve Activity ID**: Auto-generate or use explicit `activity_id` parameter
2. **Check Replay Mode**: If `ctx.IsReplaying()` is true, check cache
3. **Cache Lookup**: Look for cached result using `activity_id` as key
4. **Return or Execute**: Return cached result if found, otherwise execute function
5. **Record Result**: Save result to database with `activity_id` for future replay

### Example

```go
package main

import (
	"context"
	"github.com/i2y/romancy"
)

type ReservationResult struct {
	ReservationID string `json:"reservation_id"`
	Status        string `json:"status"`
}

type PaymentResult struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
}

type ShippingResult struct {
	TrackingNumber string `json:"tracking_number"`
}

type OrderWorkflowResult struct {
	Status string `json:"status"`
}

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string) (ReservationResult, error) {
		// Business logic here
		return ReservationResult{ReservationID: "R123", Status: "reserved"}, nil
	},
)

var processPayment = romancy.DefineActivity("process_payment",
	func(ctx context.Context, orderID string) (PaymentResult, error) {
		// Business logic here
		return PaymentResult{TransactionID: "T456", Status: "completed"}, nil
	},
)

var arrangeShipping = romancy.DefineActivity("arrange_shipping",
	func(ctx context.Context, orderID string) (ShippingResult, error) {
		// Business logic here
		return ShippingResult{TrackingNumber: "TRACK789"}, nil
	},
)

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderWorkflowResult, error) {
		// Activity IDs are auto-generated for sequential calls
		_, err := reserveInventory.Execute(ctx, orderID)
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		_, err = processPayment.Execute(ctx, orderID)
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		_, err = arrangeShipping.Execute(ctx, orderID)
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		return OrderWorkflowResult{Status: "completed"}, nil
	},
)
```

**If workflow crashed after processing payment, during replay:**

- `reserveInventory`: **Skipped** (cached result `{"reservation_id": "R123", ...}` returned)
- `processPayment`: **Skipped** (cached result `{"transaction_id": "T456", ...}` returned)
- `arrangeShipping`: **Executed** (no cache available, runs normally)

## What Gets Replayed

### ✅ Always Executed (Every Replay)

- Variable calculations and assignments
- Control flow (`if`, `for`, `switch` statements)
- Function calls (non-activity)
- Local variable operations
- Workflow function code from start to finish

### ❌ Never Re-executed (Cached)

- Completed activity business logic
- Activities that gave up: the recorded failure is returned again (see [Failures are recorded](/docs/core-features/retry#failures-are-recorded))
- Kernel primitives called from workflow code (`RegisterName`, `Monitor`, `IssueCapability`, `PostTask`, `ClaimTask`, `ReadChannelSince`, `Publish`, `SendTo`, ...): each runs once and its outcome, an error with a kernel code included, is returned from history

### Example

```go
type BalanceCheckResult struct {
	Sufficient bool    `json:"sufficient"`
	Balance    float64 `json:"balance"`
}

type TransactionResult struct {
	Amount float64 `json:"amount"`
	Fee    float64 `json:"fee"`
}

type ComplexWorkflowResult struct {
	Status string `json:"status"`
}

var complexWorkflow = romancy.DefineWorkflow("complex_workflow",
	func(ctx *romancy.WorkflowContext, amount float64) (ComplexWorkflowResult, error) {
		// This code executes every time (including replay)
		tax := amount * 0.1
		total := amount + tax
		fmt.Printf("Total calculated: %.2f\n", total)  // Prints on every replay!

		// Activity is skipped during replay (cached)
		result1, err := checkBalance.Execute(ctx, total)
		if err != nil {
			return ComplexWorkflowResult{}, err
		}

		// This if statement is evaluated every time
		if result1.Sufficient {
			// This activity is also skipped during replay
			result2, err := processTransaction.Execute(ctx, total)
			if err != nil {
				return ComplexWorkflowResult{}, err
			}

			// This calculation executes every time
			finalAmount := result2.Amount - result2.Fee

			// This activity is also skipped during replay
			_, err = sendReceipt.Execute(ctx, finalAmount)
			if err != nil {
				return ComplexWorkflowResult{}, err
			}
		} else {
			// This branch is also evaluated every time
			_, err := sendRejection.Execute(ctx, "Insufficient balance")
			if err != nil {
				return ComplexWorkflowResult{}, err
			}
		}

		return ComplexWorkflowResult{Status: "completed"}, nil
	},
)
```

**During replay:**

1. `tax`, `total` calculations execute every time
2. `fmt.Printf()` executes every time (may appear multiple times in logs)
3. `checkBalance` skipped, `result1` from cache
4. `if result1["sufficient"]` evaluated every time
5. `processTransaction` skipped, `result2` from cache
6. `finalAmount` calculation executes every time
7. `sendReceipt` skipped

## History and Caching

### Data Flow

```
First execution:
    Activity executes → Result saved to DB → Available for replay

Replay:
    Load history from DB → Populate cache → Return cached results
```

### What Gets Stored

Romancy persists all activity results to the `workflow_history` table:

| instance_id | activity_id | event_type | event_data |
|-------------|-------------|------------|------------|
| order-abc123 | reserve_inventory:1 | ActivityCompleted | `{"activity_name": "reserve_inventory", "result": {"reservation_id": "R123"}, "input": {...}}` |
| order-abc123 | process_payment:1 | ActivityCompleted | `{"activity_name": "process_payment", "result": {"transaction_id": "T456"}, "input": {...}}` |
| order-abc123 | wait_event_payment.completed:1 | EventReceived | `{"event_data": {...}}` |

**Event Types:**

- **ActivityCompleted**: Successful activity execution
- **ActivityFailed**: An activity gave up (terminal error or retries exhausted), transactional or not. Replay restores it as a `TerminalError`, a `RetryExhaustedError` or a kernel error where it was one, otherwise as a plain error with the message
- **EventReceived**: Event received via `WaitEvent()`
- **TimerExpired**: Timer expired via `Sleep()`

### How Cache Works

On replay, Romancy:

1. **Loads all history** from the database for this workflow instance
2. **Populates an in-memory cache** keyed by `activity_id`
3. **Returns cached results** without re-executing activities

**Example cache after loading history:**

```go
map[string]any{
    "reserve_inventory:1": map[string]any{"reservation_id": "R123", "status": "reserved"},
    "process_payment:1": map[string]any{"transaction_id": "T456", "status": "completed"},
}
```

This ensures workflows resume exactly where they left off, even after crashes.

### ReceivedEvent Reconstruction

Events received via `WaitEvent()` are automatically reconstructed from stored data, preserving CloudEvents metadata (type, source, time, etc.).

## Determinism Guarantees

### ✅ Best Practices

**1. Hide non-deterministic operations in activities:**

```go
var getCurrentTime = romancy.DefineActivity("get_current_time",
	func(ctx context.Context, _ struct{}) (string, error) {
		return time.Now().Format(time.RFC3339), nil
	},
)

type TimestampResult struct {
	Timestamp string `json:"timestamp"`
}

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, _ struct{}) (TimestampResult, error) {
		// Replay will use the same timestamp
		timestamp, err := getCurrentTime.Execute(ctx, struct{}{})
		if err != nil {
			return TimestampResult{}, err
		}
		return TimestampResult{Timestamp: timestamp}, nil
	},
)
```

**2. Random values should be activities:**

```go
var generateID = romancy.DefineActivity("generate_id",
	func(ctx context.Context, _ struct{}) (string, error) {
		return uuid.New().String(), nil
	},
)
```

**3. External API calls should be activities (recommended):**

```go
type APIData struct {
	ID     string `json:"id"`
	Value  string `json:"value"`
	Status string `json:"status"`
}

var callExternalAPI = romancy.DefineActivity("call_external_api",
	func(ctx context.Context, _ struct{}) (APIData, error) {
		resp, err := http.Get("https://api.example.com/data")
		if err != nil {
			return APIData{}, err
		}
		defer resp.Body.Close()

		var data APIData
		json.NewDecoder(resp.Body).Decode(&data)
		return data, nil
	},
)

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, _ struct{}) (APIData, error) {
		// Benefits of making it an activity:
		// - Not re-executed on replay (definitely from cache)
		// - Easy to test (can be mocked)
		// - Recorded in history
		// - Better performance (network cost reduced)
		data, err := callExternalAPI.Execute(ctx, struct{}{})
		if err != nil {
			return APIData{}, err
		}
		return data, nil
	},
)
```

### ❌ Anti-Patterns

```go
type SomeActivityResult struct {
	Status string `json:"status"`
}

var badWorkflow = romancy.DefineWorkflow("bad_workflow",
	func(ctx *romancy.WorkflowContext, _ struct{}) (SomeActivityResult, error) {
		// ❌ Direct time access in workflow (different on replay)
		timestamp := time.Now()
		// First run: 2025-01-01 10:00:00
		// Replay: 2025-01-01 10:05:00 ← Different!

		// ❌ Random value generation in workflow (different on replay)
		requestID := uuid.New().String()
		// First run: "abc-123"
		// Replay: "def-456" ← Different!

		// ❌ File write in workflow (duplicated on replay)
		f, _ := os.OpenFile("log.txt", os.O_APPEND|os.O_WRONLY, 0644)
		f.WriteString(fmt.Sprintf("Processing at %s\n", timestamp))
		f.Close()
		// Logs appended on every replay

		result, err := someActivity.Execute(ctx, SomeActivityInput{Timestamp: timestamp.String(), RequestID: requestID})
		return result, err
	},
)
```

**Rule of thumb:** When in doubt, make it an activity. There's minimal downside and significant benefits.

## When Replay Happens

### 1. Event Waiting Resume (`WaitEvent()`)

The most common case is when a workflow resumes after waiting for an external event.

```go
type StartPaymentResult struct {
	PaymentID string `json:"payment_id"`
}

type CompleteOrderResult struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

var paymentWorkflow = romancy.DefineWorkflow("payment_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (CompleteOrderResult, error) {
		// Step 1: Start payment
		_, err := startPayment.Execute(ctx, orderID)
		if err != nil {
			return CompleteOrderResult{}, err
		}

		// Step 2: Wait for payment completion event
		// Workflow pauses here (status="waiting_for_message")
		event, err := romancy.WaitEvent[map[string]any](ctx,
			"payment.completed",
			romancy.WithEventTimeout(5*time.Minute),
		)
		if err != nil {
			return CompleteOrderResult{}, err
		}

		// After event received, resume from here (replay happens)
		// Step 3: Complete order
		paymentID, _ := event.Data["payment_id"].(string)
		result, err := completeOrder.Execute(ctx, CompleteOrderInput{OrderID: orderID, PaymentID: paymentID})
		if err != nil {
			return CompleteOrderResult{}, err
		}

		return result, nil
	},
)
```

**Replay behavior:**

1. The event resumes the instance: a worker runs it again, with `ctx.IsReplaying()` true
2. The run loads the instance's history
3. Workflow function runs from start
4. `startPayment` - **Skipped** (cached result)
5. `WaitEvent()` - **Skipped** (cached event data)
6. `completeOrder` - **Executed** (new activity)

### 2. Explicit Resume Call

Workers resume interrupted workflows automatically; an admin endpoint only needs to inspect state:

```go
// Admin API endpoint: inspect a workflow instance.
// Recovery itself is automatic: when a worker restarts, Romancy resumes
// instances whose locks went stale, so no manual resume call is needed.
func InspectWorkflowHandler(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("instance_id")

	ctx := r.Context()
	instance, err := app.GetInstance(ctx, instanceID)
	var notFound *romancy.WorkflowNotFoundError
	if errors.As(err, &notFound) {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"status":      string(instance.Status),
		"instance_id": instanceID,
	})
}
```

### 3. Crash Recovery (Automatic)

A run holds its instance's lock: a 5-minute lease that the run renews every minute. When a worker crashes, its leases stop being renewed and expire. Recovery needs no code of yours:

- **Claiming expired leases**: every worker polls for `running` instances that are unlocked or whose lease expired (every second by default, `WithWorkflowResumptionInterval`, backing off while there are none), claims a batch of them and resumes them by replay
- **Stale lock cleanup**: the leader's cleanup tick (every 60 seconds by default, `WithStaleLockInterval`) also clears the expired locks of instances that wait for an event or a timer, so the event or the timer can resume them
- **Crash during compensation**: Romancy runs compensations while the instance is still `running`, so a worker that crashes in the middle of them leaves it `running`. The next run replays the workflow, which fails the same way again (the activity's failure is in the history), and runs only the compensations not yet recorded as executed

**Source hash verification (Safety mechanism):**

A worker resumes only instances whose code it has. Register each workflow with a hash of its source (the code, or the definition it was generated from):

```go
romancy.RegisterWorkflow(app, orderWorkflow, romancy.WithWorkflowSourceHash("order-v2"))
```

- An instance records the hash it was started with: the registered one, or the one given to `StartWorkflow` with `romancy.WithSourceHash`
- A worker claims only instances of workflows it registered, with the same hash; an instance started under another hash waits for a worker that has its version of the code
- A resume that reaches a worker with another hash returns `romancy.ErrSourceHashMismatch` and leaves the instance for another worker
- An empty hash on either side (the default) means no check

This prevents incompatible code from executing and ensures crash recovery is safe.

### 4. Deployment & Scale-Out

Romancy supports distributed execution, so workflows continue during deployment:

**Scenario:**

1. Worker A executing a workflow
2. Worker B newly deployed
3. Worker A shutdown
4. Waiting workflows are taken over by Worker B (resume via replay)

**Database-based exclusive control guarantee:**

Romancy's database-based exclusive control prevents multiple workers from executing the same workflow instance simultaneously: a run executes only while it holds the instance's lock (see [Exclusive Control Guarantees](#exclusive-control-guarantees)).

## Complete Replay Flow

### Initial Execution (Completed Steps 1-2, Crashed During Step 3)

```go
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderWorkflowResult, error) {
		// Activity 1 (auto-generated ID: "reserve_inventory:1")
		_, err := reserveInventory.Execute(ctx, orderID)
		// → DB saved: activity_id="reserve_inventory:1", result={"reservation_id": "R123"}
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		// Activity 2 (auto-generated ID: "process_payment:1")
		_, err = processPayment.Execute(ctx, orderID)
		// → DB saved: activity_id="process_payment:1", result={"transaction_id": "T456"}
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		// Activity 3: the worker crashes while it runs
		_, err = arrangeShipping.Execute(ctx, orderID)
		// → Nothing recorded for arrange_shipping:1
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		return OrderWorkflowResult{Status: "completed"}, nil
	},
)
```

**DB State:**

- `workflow_instances.status = "running"`
- `workflow_instances.current_activity_id = "process_payment:1"`
- `workflow_history` has 2 records

### Replay Execution (Resume)

Once the crashed worker's lease expires, a worker claims the instance and runs it again:

1. The worker takes the instance's lock and loads its history: `reserve_inventory:1` → `{"reservation_id": "R123"}`, `process_payment:1` → `{"transaction_id": "T456"}`
2. The workflow function runs from the start, with `ctx.IsReplaying()` true
3. `reserveInventory.Execute` finds `reserve_inventory:1` in the history and returns its result without running the activity
4. `processPayment.Execute` does the same for `process_payment:1`
5. `arrangeShipping.Execute` finds nothing for `arrange_shipping:1`, so the activity runs, and its result is recorded
6. The workflow returns, and the instance is marked `completed` with its output

## Safety Mechanisms

Romancy includes several safety mechanisms to ensure reliable execution:

### Source Hash Verification

Before auto-resuming crashed workflows, Romancy verifies workflow definition hasn't changed:

- Each workflow can be registered with a source hash (`WithWorkflowSourceHash`)
- Stored in database when workflow starts (`source_hash`)
- Compared with the worker's registered hash when it claims or resumes an instance
- Instances of another version are left for a worker that has that version (prevents unsafe execution)

This prevents:

- Resuming workflows with outdated logic
- Schema mismatches after deployment
- Data corruption from incompatible code changes

### Exclusive Control Guarantees

Romancy's database-based exclusive control prevents concurrent execution. Each run takes the instance's lock in the database, and the workflow engine does this for you:

- **5-minute lease**, renewed every minute while the run lasts (a crashed worker's lease expires)
- **Fenced writes**: every write of a run checks that the run still holds the lock, so a run that lost its lease cannot record anything
- **Worker ID tracking** (`locked_by` shows which worker holds the lock)
- **Stale lock cleanup** (automatic recovery after crashes)

### Transactional History Recording

All history recording is transactional:

- Activity completion + history save in single transaction
- Rollback on failure (ensures consistency)
- No orphaned history records
- Deterministic replay guaranteed

### Aborted Runs

A primitive records its outcome only when it returns a value or a kernel-defined error (`ErrActorNameTaken`, `ErrChannelForbidden`, `ErrNotOwner`, `ErrCursorNotFound`, ...). Any other error, such as a database outage, would make a replay take a branch the code never meant to take, so it is not recorded. It aborts the run instead:

- the failing primitive, and every primitive and activity after it in the same run, returns an error matching `romancy.ErrRunAborted`;
- the run ends without completing, failing or compensating the instance and releases its lock;
- the instance stays `running`, and a later run replays it and executes the primitive again.

A run that loses its lock, or whose worker shuts down, ends the same way: nothing is recorded and no compensation runs. Do not swallow `ErrRunAborted` in workflow code; return it.

### Compensating Workflow Recovery

Romancy runs a failed workflow's compensations while the instance is still `running`:

- A worker that crashes during compensation leaves the instance `running`, and its lease expires
- The next run replays the workflow, which fails again with the recorded error
- Only the compensations not yet recorded as executed run again
- Compensations complete even after multiple crashes, so make them idempotent

## Summary

Romancy's replay mechanism characteristics:

| Item | Behavior |
|------|----------|
| **Completed activities** | Skipped (result from cache) |
| **Workflow function code** | Runs from start every time |
| **Control flow (if/for/switch)** | Evaluated every time |
| **Variable calculations** | Executed every time |
| **State restoration** | Load history from DB → Populate memory cache |
| **Determinism guarantee** | Non-deterministic operations hidden in activities |

This mechanism ensures workflows can resume accurately after process crashes, deployments, or scale-outs.

## See Also

- **[Saga Pattern](/docs/core-features/saga-compensation)**: Automatic compensation on workflow failure
- **[Event Handling](/docs/core-features/events/wait-event)**: Wait for external events in workflows
