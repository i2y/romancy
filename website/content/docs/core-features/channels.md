---
title: "Channels"
weight: 3
---

Romancy provides a powerful channel-based message passing system for communication between workflows. This enables patterns like pub/sub, work queues, and direct messaging between workflow instances.

## Overview

Channels allow workflows to:

- **Subscribe** to named channels with different delivery modes
- **Receive** messages asynchronously (workflow suspends while waiting)
- **Publish** messages to all subscribers
- **Send directly** to specific workflow instances

## Delivery Modes

Romancy supports two delivery modes:

| Mode | Description | Use Case |
|------|-------------|----------|
| `ModeBroadcast` | All subscribers receive every message | Notifications, events |
| `ModeCompeting` | Each message goes to exactly one subscriber | Work queues, load balancing |

## Basic Usage

### Subscribe to a Channel

```go
import "github.com/i2y/romancy"

var myWorkflow = romancy.DefineWorkflow("my_workflow",
    func(ctx *romancy.WorkflowContext, input MyInput) (MyResult, error) {
        // Subscribe to receive all messages (broadcast mode)
        if err := romancy.Subscribe(ctx, "notifications", romancy.ModeBroadcast); err != nil {
            return MyResult{}, err
        }

        // Or subscribe in competing mode (work queue)
        if err := romancy.Subscribe(ctx, "tasks", romancy.ModeCompeting); err != nil {
            return MyResult{}, err
        }

        // ... rest of workflow
    },
)
```

### Receive Messages

```go
// Define message type
type ChatMessage struct {
    From    string    `json:"from"`
    Content string    `json:"content"`
    Time    time.Time `json:"time"`
}

var chatWorkflow = romancy.DefineWorkflow("chat_workflow",
    func(ctx *romancy.WorkflowContext, input ChatInput) (ChatResult, error) {
        // Subscribe to channel
        if err := romancy.Subscribe(ctx, "chat", romancy.ModeBroadcast); err != nil {
            return ChatResult{}, err
        }

        // Wait for a message (workflow suspends here)
        msg, err := romancy.Receive[ChatMessage](ctx, "chat")
        if err != nil {
            return ChatResult{}, err
        }

        fmt.Printf("Received: %s from %s\n", msg.Data.Content, msg.Data.From)
        return ChatResult{LastMessage: msg.Data.Content}, nil
    },
)
```

### Receive with Timeout

```go
// Wait for message with timeout
msg, err := romancy.Receive[ChatMessage](ctx, "chat",
    romancy.WithReceiveTimeout(30*time.Second),
)
if err != nil {
    // Check for timeout
    var timeoutErr *romancy.ChannelMessageTimeoutError
    if errors.As(err, &timeoutErr) {
        fmt.Println("No message received within timeout")
        return ChatResult{Status: "timeout"}, nil
    }
    return ChatResult{}, err
}
```

### Publish Messages

```go
var publisherWorkflow = romancy.DefineWorkflow("publisher",
    func(ctx *romancy.WorkflowContext, input PublishInput) (PublishResult, error) {
        message := ChatMessage{
            From:    "system",
            Content: input.Message,
            Time:    time.Now(),
        }

        // Publish to all subscribers
        if err := romancy.Publish(ctx, "chat", message); err != nil {
            return PublishResult{}, err
        }

        return PublishResult{Status: "published"}, nil
    },
)
```

### Publish with Metadata

```go
// Attach metadata to messages
err := romancy.Publish(ctx, "events", eventData,
    romancy.WithMetadata(map[string]any{
        "priority": "high",
        "source":   "order-service",
    }),
)
```

### Send to Specific Instance

```go
// Send directly to a specific workflow instance
targetInstanceID := "wf-12345"
err := romancy.SendTo(ctx, targetInstanceID, "private-channel", message)
```

## Unsubscribe

```go
// Remove subscription when no longer needed
if err := romancy.Unsubscribe(ctx, "notifications"); err != nil {
    return MyResult{}, err
}
```

## Transactional Message Passing

When `Publish` or `SendTo` is called within an activity (inside a database transaction), Romancy ensures transactional consistency:

| Scenario | Behavior |
|----------|----------|
| Inside TX → Commit | Message saved, delivery after commit |
| Inside TX → Rollback | Message not saved, no delivery |
| Outside TX | Message saved, immediate delivery |

This guarantees that:
- Messages are only delivered if the transaction commits successfully
- Rollbacks prevent any message delivery (consistency)
- No duplicate messages or lost messages

### Example: Transactional Publish

```go
var orderActivity = romancy.DefineActivity("process_order",
    func(ctx context.Context, order Order) (OrderResult, error) {
        // This runs inside a transaction by default
        wfCtx := romancy.GetWorkflowContext(ctx)

        // Database operations...
        session := wfCtx.Session()
        _, err := session.ExecContext(ctx,
            "INSERT INTO orders (id, status) VALUES (?, ?)",
            order.ID, "processing",
        )
        if err != nil {
            return OrderResult{}, err // Rollback - no message sent
        }

        // Message is queued but NOT delivered yet
        if err := romancy.Publish(wfCtx, "order-events", OrderEvent{
            OrderID: order.ID,
            Status:  "processing",
        }); err != nil {
            return OrderResult{}, err // Rollback - no message sent
        }

        return OrderResult{Status: "ok"}, nil
        // Commit happens here - message delivered after commit
    },
)
```

## ReceivedMessage Structure

When receiving a message, you get a `ReceivedMessage[T]` struct:

```go
type ReceivedMessage[T any] struct {
    ID               int64          // Message ID
    ChannelName      string         // Channel name
    Data             T              // Your message data
    Metadata         map[string]any // Optional metadata
    SenderInstanceID string         // Sender's instance ID (if sent via SendTo)
    CreatedAt        time.Time      // When message was created
}
```

## Complete Example

Here's a complete example showing consumer and producer workflows:

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"
    "time"

    "github.com/i2y/romancy"
)

// Message types
type TaskMessage struct {
    TaskID  string `json:"task_id"`
    Payload string `json:"payload"`
}

type TaskResult struct {
    TaskID string `json:"task_id"`
    Status string `json:"status"`
}

// Consumer workflow - processes tasks from queue
var taskConsumer = romancy.DefineWorkflow("task_consumer",
    func(ctx *romancy.WorkflowContext, input struct{}) ([]TaskResult, error) {
        var results []TaskResult

        // Subscribe in competing mode (work queue)
        if err := romancy.Subscribe(ctx, "tasks", romancy.ModeCompeting); err != nil {
            return nil, err
        }

        // Process up to 10 tasks
        for i := 0; i < 10; i++ {
            msg, err := romancy.Receive[TaskMessage](ctx, "tasks",
                romancy.WithReceiveTimeout(5*time.Second),
            )
            if err != nil {
                var timeoutErr *romancy.ChannelMessageTimeoutError
                if errors.As(err, &timeoutErr) {
                    break // No more tasks
                }
                return nil, err
            }

            log.Printf("Processing task: %s", msg.Data.TaskID)
            results = append(results, TaskResult{
                TaskID: msg.Data.TaskID,
                Status: "completed",
            })
        }

        return results, nil
    },
)

// Producer workflow - creates tasks
var taskProducer = romancy.DefineWorkflow("task_producer",
    func(ctx *romancy.WorkflowContext, tasks []TaskMessage) (int, error) {
        for _, task := range tasks {
            if err := romancy.Publish(ctx, "tasks", task); err != nil {
                return 0, err
            }
        }
        return len(tasks), nil
    },
)

func main() {
    app := romancy.NewApp(
        romancy.WithDatabase("channels.db"),
        romancy.WithWorkerID("worker-1"),
    )

    ctx := context.Background()
    if err := app.Start(ctx); err != nil {
        log.Fatal(err)
    }
    defer app.Shutdown(ctx)

    // Start a consumer
    consumerID, _ := romancy.StartWorkflow(ctx, app, taskConsumer, struct{}{})
    fmt.Printf("Started consumer: %s\n", consumerID)

    // Start a producer with tasks
    tasks := []TaskMessage{
        {TaskID: "task-1", Payload: "data1"},
        {TaskID: "task-2", Payload: "data2"},
        {TaskID: "task-3", Payload: "data3"},
    }
    producerID, _ := romancy.StartWorkflow(ctx, app, taskProducer, tasks)
    fmt.Printf("Started producer: %s\n", producerID)

    // Wait for processing...
    time.Sleep(10 * time.Second)
}
```

## Use Cases

### Fan-Out (Broadcast)

Send notifications to all interested workflows:

```go
// All subscribers receive the notification
romancy.Subscribe(ctx, "system-alerts", romancy.ModeBroadcast)
romancy.Publish(ctx, "system-alerts", AlertMessage{...})
```

### Work Queue (Competing)

Distribute work across multiple workers:

```go
// Only one subscriber receives each task
romancy.Subscribe(ctx, "work-queue", romancy.ModeCompeting)
msg, _ := romancy.Receive[WorkItem](ctx, "work-queue")
```

### Request-Response

Direct communication between specific workflows:

```go
// Workflow A sends request
romancy.SendTo(ctx, targetInstanceID, "requests", request)

// Workflow B receives and responds
msg, _ := romancy.Receive[Request](ctx, "requests")
romancy.SendTo(ctx, msg.SenderInstanceID, "responses", response)
```

## See Also

- **[Event Waiting](/docs/core-features/events/wait-event)**: Wait for external CloudEvents
- **[Workflows & Activities](/docs/core-features/workflows-activities)**: Core workflow concepts
- **[Transactional Outbox](/docs/core-features/transactional-outbox)**: Reliable event publishing
