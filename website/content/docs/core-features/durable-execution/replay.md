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
5. **Error Handling**: Failed activities are recorded with full error details

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

**Concurrent Execution (Manual IDs Required):**

```go
// Execution order is non-deterministic, so manual IDs are required
import "golang.org/x/sync/errgroup"

type ProcessResult struct {
    Status string `json:"status"`
    Data   string `json:"data"`
}

var eg errgroup.Group
var results [3]ProcessResult

eg.Go(func() error {
    r, err := processA.Execute(ctx, data, romancy.WithActivityID("process_a:1"))
    results[0] = r
    return err
})

eg.Go(func() error {
    r, err := processB.Execute(ctx, data, romancy.WithActivityID("process_b:1"))
    results[1] = r
    return err
})

eg.Go(func() error {
    r, err := processC.Execute(ctx, data, romancy.WithActivityID("process_c:1"))
    results[2] = r
    return err
})

if err := eg.Wait(); err != nil {
    return ConcurrentResult{}, err
}
```

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
- **ActivityFailed**: Activity raised an error (includes error type and message)
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
	func(ctx context.Context) (string, error) {
		return time.Now().Format(time.RFC3339), nil
	},
)

type TimestampResult struct {
	Timestamp string `json:"timestamp"`
}

var myWorkflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext) (TimestampResult, error) {
		// Replay will use the same timestamp
		timestamp, err := getCurrentTime.Execute(ctx)
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
	func(ctx context.Context) (string, error) {
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
	func(ctx context.Context) (APIData, error) {
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
	func(ctx *romancy.WorkflowContext) (APIData, error) {
		// Benefits of making it an activity:
		// - Not re-executed on replay (definitely from cache)
		// - Easy to test (can be mocked)
		// - Recorded in history
		// - Better performance (network cost reduced)
		data, err := callExternalAPI.Execute(ctx)
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
	func(ctx *romancy.WorkflowContext) (SomeActivityResult, error) {
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

		result, err := someActivity.Execute(ctx, timestamp.String(), requestID)
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
		// Workflow pauses here (status="waiting_for_event")
		event, err := romancy.WaitEvent(ctx,
			romancy.WithEventType("payment.completed"),
			romancy.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return CompleteOrderResult{}, err
		}

		// After event received, resume from here (replay happens)
		// Step 3: Complete order
		result, err := completeOrder.Execute(ctx, orderID, event)
		if err != nil {
			return CompleteOrderResult{}, err
		}

		return result, nil
	},
)
```

**Replay behavior:**

1. `resume_workflow()` creates context with `ctx.IsReplaying()=true`
2. `loadHistory()` loads execution history
3. Workflow function runs from start
4. `startPayment` - **Skipped** (cached result)
5. `WaitEvent()` - **Skipped** (cached event data)
6. `completeOrder` - **Executed** (new activity)

### 2. Explicit Resume Call

Developers can manually resume workflows:

```go
// Admin API endpoint
func ResumeWorkflowHandler(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instance_id")

	ctx := context.Background()
	instance, err := app.Storage().GetInstance(ctx, instanceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	workflowName := instance["workflow_name"].(string)

	// Get corresponding workflow
	workflow := romancy.GetWorkflow(workflowName)

	// Start replay
	if err := workflow.Resume(ctx, app, instanceID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"status":      "resumed",
		"instance_id": instanceID,
	})
}
```

### 3. Crash Recovery (Automatic)

Romancy automatically recovers from crashes in two stages:

#### 3-1. Stale Lock Cleanup (Implemented)

When a worker process crashes, its locks become "stale." Romancy automatically cleans these up:

```go
func cleanupStaleLocksPeriodically(storage StorageProtocol, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		// Clean up stale locks (uses lock_expires_at column)
		workflowsToResume, err := storage.CleanupStaleLocks(context.Background())
		if err != nil {
			log.Printf("Failed to cleanup stale locks: %v", err)
			continue
		}

		if len(workflowsToResume) > 0 {
			log.Printf("Cleaned up %d stale locks", len(workflowsToResume))
		}
	}
}
```

This background task starts automatically when `romancy.App` launches.

**How it works:**

1. **Every 60 seconds**, check for stale locks
2. **Expired locks** are detected (based on `lock_expires_at` column set at lock acquisition)
3. Release those locks (`locked_by=NULL`)
4. Return list of workflows that need to be resumed

**Return value structure:**

```go
[]map[string]string{
    {
        "instance_id":   "...",
        "workflow_name": "...",
        "source_hash":   "...",  // Hash of workflow definition
        "status":        "...",  // "running" or "compensating"
    },
    // ...
}
```

The `status` field indicates whether the workflow was running normally (`"running"`) or executing compensations (`"compensating"`) when it crashed.

#### 3-2. Automatic Workflow Resume (Implemented)

After cleaning stale locks, Romancy automatically resumes workflows with `status="running"` or `status="compensating"`:

```go
func autoResumeStaleworkflowsPeriodically(storage StorageProtocol, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		ctx := context.Background()

		// Clean up stale locks and get workflows to resume
		workflowsToResume, err := storage.CleanupStaleLocks(ctx)
		if err != nil {
			log.Printf("Failed to cleanup stale locks: %v", err)
			continue
		}

		if len(workflowsToResume) > 0 {
			// Auto-resume workflows
			for _, wf := range workflowsToResume {
				instanceID := wf["instance_id"]
				workflowName := wf["workflow_name"]

				log.Printf("Auto-resuming: %s (%s)", workflowName, instanceID)

				workflow := romancy.GetWorkflow(workflowName)
				if err := workflow.Resume(ctx, app, instanceID); err != nil {
					log.Printf("Failed to resume %s: %v", instanceID, err)
				}
			}
		}
	}
}
```

**Special handling for different workflow states:**

1. **Running workflows** (`status="running"`):
   - Resume normally via `workflow.Resume()`
   - Full workflow function execution with replay

2. **Compensating workflows** (`status="compensating"`):
   - Resume via `workflow.ResumeCompensating()`
   - Only re-execute incomplete compensations (not the workflow function)
   - Ensures compensation transactions complete even after crashes

**Source hash verification (Safety mechanism):**

Before auto-resuming, Romancy verifies that the workflow definition hasn't changed:

```go
// Check if workflow definition matches current registry
currentHash := workflow.SourceHash()
storedHash := wf["source_hash"]

if currentHash != storedHash {
    // Workflow code has changed - skip auto-resume
    log.Printf("Source hash mismatch for %s", workflowName)
    continue
}
```

This prevents incompatible code from executing and ensures crash recovery is safe.

**Why this works:**

When a worker crashes, workflows with `status="running"` **always** hold a stale lock:

| Workflow Status | Lock Held | On Crash | Auto-Resume Strategy |
|----------------|-----------|----------|---------------------|
| `status="running"` | YES (inside `workflow_lock`) | Becomes stale | ✅ Normal resume |
| `status="compensating"` | YES (inside compensation execution) | Becomes stale | ✅ Compensation resume |
| `status="waiting_for_event"` | NO (after lock released) | No stale lock | ❌ Event-driven resume |
| `status="waiting_for_timer"` | NO (after lock released) | No stale lock | ❌ Timer-driven resume |
| `status="completed"` | NO | No stale lock | N/A |
| `status="failed"` | NO | No stale lock | N/A |
| `status="cancelled"` | NO | No stale lock | N/A |

Therefore, cleaning stale locks and resuming `status="running"` and `status="compensating"` workflows ensures **no resume leakage**.

### 4. Deployment & Scale-Out

Romancy supports distributed execution, so workflows continue during deployment:

**Scenario:**

1. Worker A executing a workflow
2. Worker B newly deployed
3. Worker A shutdown
4. Waiting workflows are taken over by Worker B (resume via replay)

**Database-based exclusive control guarantee:**

Romancy's database-based exclusive control prevents multiple workers from executing the same workflow instance simultaneously:

```go
err := app.WithWorkflowLock(ctx, instanceID, workerID, func() error {
    // Only execute while lock held
    wfCtx := romancy.NewWorkflowContext(instanceID, true, ...)
    wfCtx.LoadHistory()
    return workflowFunc(wfCtx, inputData)
})
```

## Complete Replay Flow

### Initial Execution (Completed Steps 1-2, Crashed at Step 3)

```go
var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderWorkflowResult, error) {
		// Activity 1 (auto-generated ID: "reserve_inventory:1")
		inventory, err := reserveInventory.Execute(ctx, orderID)
		// → DB saved: activity_id="reserve_inventory:1", result={"reservation_id": "R123"}
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		// Activity 2 (auto-generated ID: "process_payment:1")
		payment, err := processPayment.Execute(ctx, orderID)
		// → DB saved: activity_id="process_payment:1", result={"transaction_id": "T456"}
		if err != nil {
			return OrderWorkflowResult{}, err
		}

		// Activity 3: Error occurs (e.g., network error)
		shipping, err := arrangeShipping.Execute(ctx, orderID)
		// → Error, workflow interrupted
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

```go
// 1. workflow.Resume() called
err := orderWorkflow.Resume(ctx, app, instanceID)

// 2. Create WorkflowContext (isReplaying=true)
wfCtx := romancy.NewWorkflowContext(
    instanceID,
    true,  // Replay mode
    ...
)

// 3. Load history
wfCtx.LoadHistory()
// → historyCache = {"reserve_inventory:1": {...}, "process_payment:1": {...}}

// 4. Execute workflow function from start
result, err := orderWorkflow.Execute(wfCtx, orderID)

// 5. Activity: reserve_inventory:1
//    - ctx.IsReplaying() == true
//    - Cache has activity_id="reserve_inventory:1"
//    - Don't execute function, return {"reservation_id": "R123"} from cache

// 6. Activity: process_payment:1
//    - ctx.IsReplaying() == true
//    - Cache has activity_id="process_payment:1"
//    - Don't execute function, return {"transaction_id": "T456"} from cache

// 7. Activity: arrange_shipping:1
//    - ctx.IsReplaying() == true
//    - No cache for activity_id="arrange_shipping:1"
//    - Execute function (new processing)
//    - Save result to DB on success

// 8. Workflow complete
wfCtx.UpdateStatus("completed", result)
```

## Safety Mechanisms

Romancy includes several safety mechanisms to ensure reliable execution:

### Source Hash Verification

Before auto-resuming crashed workflows, Romancy verifies workflow definition hasn't changed:

- Each workflow has a source code hash (`source_hash`)
- Stored in database when workflow starts
- Compared with current registry during auto-resume
- Incompatible code is skipped (prevents unsafe execution)

This prevents:

- Resuming workflows with outdated logic
- Schema mismatches after deployment
- Data corruption from incompatible code changes

### Exclusive Control Guarantees

Romancy's database-based exclusive control prevents concurrent execution:

```go
err := app.WithWorkflowLock(ctx, instanceID, workerID,
    romancy.WithLockTimeout(5*time.Minute),
    func() error {
        // Only one worker can hold this lock
        // Other workers wait or skip
        return nil
    },
)
```

Features:
- **5-minute timeout** by default (prevents indefinite locks)
- **Worker ID tracking** (know which worker holds the lock)
- **Stale lock cleanup** (automatic recovery after crashes)

### Transactional History Recording

All history recording is transactional:

- Activity completion + history save in single transaction
- Rollback on failure (ensures consistency)
- No orphaned history records
- Deterministic replay guaranteed

### Compensating Workflow Recovery

Special handling for workflows that crash during compensation:

- `status="compensating"` detected during cleanup
- Only incomplete compensations are re-executed
- Workflow function is NOT re-executed
- Ensures compensation transactions complete even after multiple crashes

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
