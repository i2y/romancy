---
title: "Recur Pattern"
weight: 4
---

Romancy provides an Erlang-style tail recursion pattern called `Recur` for long-running workflows. This pattern prevents unbounded history growth by archiving old history and continuing with fresh state.

## The Problem

In durable execution frameworks, every activity result is stored in the workflow history. For long-running workflows that process many items over time, this history can grow unboundedly:

```go
// Problem: History grows with each iteration
var longRunningWorkflow = romancy.DefineWorkflow("processor",
    func(ctx *romancy.WorkflowContext, input ProcessInput) (ProcessResult, error) {
        for {
            // Each iteration adds to history
            item, _ := romancy.Receive[Item](ctx, "items")
            processItem.Execute(ctx, item)  // History grows!

            // After 1000 iterations, history has 2000+ entries
        }
    },
)
```

## The Solution: Recur

`Recur` solves this by:

1. Archiving the current workflow's history
2. Cleaning up all subscriptions (events, timers, channels, groups)
3. Marking the current instance as "recurred"
4. Creating a new instance that continues from the current state

```go
var longRunningWorkflow = romancy.DefineWorkflow("processor",
    func(ctx *romancy.WorkflowContext, input ProcessInput) (ProcessResult, error) {
        // Process a batch of items
        for i := 0; i < 100; i++ {
            item, err := romancy.Receive[Item](ctx, "items",
                romancy.WithReceiveTimeout(5*time.Second),
            )
            if err != nil {
                break // No more items
            }
            processItem.Execute(ctx, item)
            input.ProcessedCount++
        }

        // Continue with fresh history (tail recursion)
        if input.ProcessedCount < input.MaxItems {
            return romancy.Recur(ctx, input) // Archives history, continues
        }

        return ProcessResult{Total: input.ProcessedCount}, nil
    },
)
```

## How Recur Works

```
┌─────────────────────────────────────────────────────────────┐
│ Workflow Instance: wf-001                                   │
├─────────────────────────────────────────────────────────────┤
│ History (100 entries)                                       │
│ ├── activity_1: result_1                                    │
│ ├── activity_2: result_2                                    │
│ └── ... (growing)                                           │
└─────────────────────────────────────────────────────────────┘
                            │
                            │ Recur(ctx, newInput)
                            ▼
┌─────────────────────────────────────────────────────────────┐
│ 1. Archive history to workflow_history_archive              │
│ 2. Clean up subscriptions (channels, events, timers)        │
│ 3. Mark wf-001 as status="recurred"                         │
│ 4. Create new instance wf-002 with continued_from="wf-001"  │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│ Workflow Instance: wf-002                                   │
│ continued_from: wf-001                                      │
├─────────────────────────────────────────────────────────────┤
│ History (fresh - 0 entries)                                 │
│ └── (starts accumulating again)                             │
└─────────────────────────────────────────────────────────────┘
```

## Basic Usage

```go
package main

import (
    "github.com/i2y/romancy"
)

// Input includes state that persists across recurs
type CounterInput struct {
    Count    int `json:"count"`
    MaxCount int `json:"max_count"`
}

type CounterResult struct {
    FinalCount int `json:"final_count"`
}

var counterWorkflow = romancy.DefineWorkflow("counter",
    func(ctx *romancy.WorkflowContext, input CounterInput) (CounterResult, error) {
        // Do some work
        for i := 0; i < 10; i++ {
            input.Count++
            // ... activities that add to history
        }

        // Check if we should continue
        if input.Count < input.MaxCount {
            // Tail recursion: archive history and continue
            return romancy.Recur(ctx, input)
        }

        // Done - return final result
        return CounterResult{FinalCount: input.Count}, nil
    },
)
```

## Tracking Workflow Lineage

Each recurred workflow tracks its origin via `continued_from`:

```go
// Get workflow instance to see lineage
instance, _ := app.GetInstance(ctx, instanceID)
if instance.ContinuedFrom != "" {
    fmt.Printf("This workflow continued from: %s\n", instance.ContinuedFrom)
}
```

You can trace the full history:

```
wf-003 (current, running)
  └── continued_from: wf-002 (recurred)
        └── continued_from: wf-001 (recurred)
              └── continued_from: "" (original)
```

## Use Cases

### 1. Batch Processing

Process large datasets in manageable chunks:

```go
type BatchInput struct {
    Offset      int   `json:"offset"`
    BatchSize   int   `json:"batch_size"`
    TotalCount  int   `json:"total_count"`
    ProcessedIDs []string `json:"processed_ids"`
}

var batchProcessor = romancy.DefineWorkflow("batch_processor",
    func(ctx *romancy.WorkflowContext, input BatchInput) (BatchResult, error) {
        // Process one batch
        items := fetchItems(input.Offset, input.BatchSize)
        for _, item := range items {
            processItem.Execute(ctx, item)
            input.ProcessedIDs = append(input.ProcessedIDs, item.ID)
        }

        // Move to next batch
        input.Offset += input.BatchSize

        if input.Offset < input.TotalCount {
            return romancy.Recur(ctx, input) // Continue with next batch
        }

        return BatchResult{
            Processed: len(input.ProcessedIDs),
        }, nil
    },
)
```

### 2. Long-Running Daemons

Workflows that run indefinitely:

```go
type DaemonInput struct {
    Iteration int `json:"iteration"`
}

var daemonWorkflow = romancy.DefineWorkflow("daemon",
    func(ctx *romancy.WorkflowContext, input DaemonInput) (DaemonResult, error) {
        // Subscribe to events
        romancy.Subscribe(ctx, "commands", romancy.ModeBroadcast)

        // Process events for a while
        for i := 0; i < 100; i++ {
            msg, err := romancy.Receive[Command](ctx, "commands",
                romancy.WithReceiveTimeout(1*time.Minute),
            )
            if err != nil {
                continue // Timeout - check again
            }

            if msg.Data.Type == "shutdown" {
                return DaemonResult{Status: "shutdown"}, nil
            }

            handleCommand.Execute(ctx, msg.Data)
        }

        // Recur to prevent history buildup
        input.Iteration++
        return romancy.Recur(ctx, input)
    },
)
```

### 3. Periodic Tasks

Tasks that repeat on a schedule:

```go
type ScheduledInput struct {
    RunCount int       `json:"run_count"`
    LastRun  time.Time `json:"last_run"`
}

var scheduledTask = romancy.DefineWorkflow("scheduled_task",
    func(ctx *romancy.WorkflowContext, input ScheduledInput) (ScheduledResult, error) {
        // Do the scheduled work
        performTask.Execute(ctx, input)
        input.RunCount++
        input.LastRun = time.Now()

        // Wait until next run
        romancy.Sleep(ctx, 1*time.Hour)

        // Recur to keep history bounded
        return romancy.Recur(ctx, input)
    },
)
```

## Best Practices

### 1. Choose Appropriate Batch Sizes

```go
// Too small: Frequent recurs, more overhead
for i := 0; i < 10; i++ { ... }
return romancy.Recur(ctx, input) // Every 10 items

// Too large: History grows too much before recur
for i := 0; i < 10000; i++ { ... }
return romancy.Recur(ctx, input) // History has 10000 entries

// Good balance: Recur every ~100-1000 operations
for i := 0; i < 100; i++ { ... }
return romancy.Recur(ctx, input)
```

### 2. Preserve Important State in Input

```go
type ProcessInput struct {
    // State that must survive recur
    ProcessedCount int      `json:"processed_count"`
    FailedItems    []string `json:"failed_items"`
    StartedAt      time.Time `json:"started_at"`

    // Configuration
    BatchSize int `json:"batch_size"`
}
```

### 3. Handle Termination Conditions

```go
var workflow = romancy.DefineWorkflow("bounded_processor",
    func(ctx *romancy.WorkflowContext, input ProcessInput) (ProcessResult, error) {
        // Process batch...

        // Multiple termination conditions
        if input.ProcessedCount >= input.MaxItems {
            return ProcessResult{Status: "completed"}, nil
        }
        if input.ErrorCount > input.MaxErrors {
            return ProcessResult{Status: "too_many_errors"}, nil
        }
        if time.Since(input.StartedAt) > 24*time.Hour {
            return ProcessResult{Status: "timeout"}, nil
        }

        return romancy.Recur(ctx, input)
    },
)
```

## Performance Considerations

| Aspect | Without Recur | With Recur |
|--------|---------------|------------|
| History Size | Grows unboundedly | Bounded per iteration |
| Replay Time | Increases over time | Constant |
| Memory Usage | Increases | Constant |
| Database Size | History table grows | Archived, can be cleaned |

## Archived History

When `Recur` is called, history is moved to `workflow_history_archive` table. This data can be:

- Kept for audit/debugging purposes
- Periodically cleaned up via background jobs
- Queried for historical analysis

```sql
-- Query archived history
SELECT * FROM workflow_history_archive
WHERE instance_id = 'wf-001'
ORDER BY sequence_number;
```

## See Also

- **[Deterministic Replay](/docs/core-features/durable-execution/replay)**: How workflow history works
- **[Channels](/docs/core-features/channels)**: Message passing between workflows
- **[Workflows & Activities](/docs/core-features/workflows-activities)**: Core concepts
