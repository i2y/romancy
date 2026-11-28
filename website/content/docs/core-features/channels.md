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

Romancy supports three delivery modes:

| Mode | Description | Use Case |
|------|-------------|----------|
| `ModeBroadcast` | All subscribers receive every message | Notifications, events |
| `ModeCompeting` | Each message goes to exactly one subscriber | Work queues, load balancing |
| `ModeDirect` | Receives messages sent via SendTo to this instance | Point-to-point, request-response |

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

        return MyResult{Status: "subscribed"}, nil
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

fmt.Println("received:", msg.Data)
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
if err := romancy.Publish(ctx, "events", eventData,
    romancy.WithMetadata(map[string]any{
        "priority": "high",
        "source":   "order-service",
    }),
); err != nil {
    return MyResult{}, err
}
```

### Send to Specific Instance (SendTo)

`SendTo` sends a message directly to a specific workflow instance. Use `ModeDirect` on the receiver side for easy setup.

```go
// === Receiver Workflow ===
// Subscribe with ModeDirect to receive SendTo messages
// This automatically subscribes to "notifications:instanceID"
if err := romancy.Subscribe(ctx, "notifications", romancy.ModeDirect); err != nil {
    return MyResult{}, err
}

// Receive works normally - ModeDirect handles the channel name internally
msg, err := romancy.Receive[MyMessage](ctx, "notifications")
if err != nil {
    return MyResult{}, err
}
fmt.Println("received:", msg.Data)

// === Sender Workflow ===
// Send directly to a specific workflow instance
targetInstanceID := "wf-12345"
if err := romancy.SendTo(ctx, targetInstanceID, "notifications", message); err != nil {
    return MyResult{}, err
}
```

This design matches Erlang/Elixir's mailbox semantics where each process has its own mailbox identified by its PID. Like a mailbox, the direct channel keeps what is sent to it: a `ModeDirect` subscription also receives the messages sent to the instance before it subscribed.

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

## Replay Safety

Called from workflow code, `Publish` and `SendTo` are recorded steps. The message and a history record of the publish commit together, and when the workflow is replayed after a suspension or a crash, the call returns the recorded outcome instead of publishing again. A refusal on a sealed channel (`ErrChannelForbidden`) is recorded the same way, so a replay takes the same branch even if the grants changed in the meantime.

The step first proves that the run still holds the instance lock. A run that has lost its lease to another worker publishes nothing and gets `ErrLockNotHeld`.

Inside an activity, `Publish` and `SendTo` run directly, in the activity's transaction (see above); there the activity is the unit that is recorded and replayed.

## Delivery Guarantees

- **Broadcast**: a subscription starts after the latest message on the channel, and the subscriber then sees every later message once.
- **Direct**: a subscription to the instance's own direct channel (`ModeDirect`) starts at the first message sent to it, so nothing sent before the subscription is lost.
- **Competing**: each message is consumed at most once. Claiming the message, recording the receive in history and deleting the message commit in one transaction. A message is handed only to a subscriber that is waiting for it, so it cannot be lost on an instance that is busy elsewhere.
- **Stranded claims**: if a consumer dies after claiming a message, the claim is taken over once it is 300 seconds old, or as soon as the consumer is completed, failed or cancelled.
- **No missed wakeups**: registering a wait re-checks for pending messages in the same transaction, so a message published while the workflow suspends still wakes it.

## Sealed Channels

A channel is open until a capability grant exists for it. Once `IssueCapability` has issued a `channel_pub` grant for a channel, publishing to it requires a token. The same applies to `channel_sub` and receiving:

```go
token, _, err := romancy.IssueCapability(ctx, "channel_pub", "orders")
if err != nil {
    return MyResult{}, err
}

// Without the token, Publish and SendTo on "orders" fail with ErrChannelForbidden
if err := romancy.Publish(ctx, "orders", order, romancy.WithPublishCapability(token)); err != nil {
    return MyResult{}, err
}

// A channel sealed for channel_sub requires the token on Receive (and ReadChannelSince)
msg, err := romancy.Receive[Order](ctx, "orders", romancy.WithReceiveCapability(subToken))
if err != nil {
    return MyResult{}, err
}
fmt.Println("received order:", msg.Data)
```

The seal also covers the direct channels derived from the name: `SendTo(ctx, target, "orders", ...)` and a `Publish` to the physical name `"orders:<instanceID>"` need the same token.

- **Expiry does not unseal.** `IssueCapability(ctx, kind, target, romancy.WithCapabilityTTL(time.Hour))` issues a grant that stops validating after an hour, but the channel stays sealed: the cleanup of expired grants always keeps a channel's newest grant. Only revoking the last grant re-opens a channel.
- **Only the issuer revokes.** `RevokeCapability` from another instance fails with `ErrNotOwner`; an unknown capability ID is a no-op.
- **CloudEvents need the token too.** An event posted to the CloudEvents endpoint for a channel sealed for `channel_pub` must carry the token in the `capabilitytoken` extension attribute (binary mode: the `ce-capabilitytoken` header). Without a valid token the endpoint answers `403 Forbidden` and publishes nothing. The attribute is not stored with the message.
- **Tokens and history.** Without a capability key, the issued token is recorded in the issuing workflow's history (a replay returns it from there). With `romancy.WithCapabilityKey(key)`, or the `DURAX_CAPABILITY_KEY` environment variable, the secret is derived from the capability ID and history holds no token. Give every worker the same key and do not change it while instances that issued capabilities are running.

## Direct Channels Are Private

Whatever the seal state, an instance cannot subscribe to, receive from or read another instance's direct channel: `Subscribe`, `Receive`, `ReadChannelSince` and `ReadChannelAfter` on `"orders:<other instance ID>"` fail with `ErrChannelForbidden`. Sending to it (`SendTo`, or `Publish` to the physical name) stays allowed.

## Reading a Channel as a Log

`ReadChannelSince` and `ReadChannelAfter` read a channel's messages without subscribing, as a recorded step:

```go
// After a message ID read earlier; ErrCursorNotFound once that message is gone
msgs, err := romancy.ReadChannelSince(ctx, "events", lastMessageID)

// After the numeric ID of the last message read; keeps working after it is deleted
msgs, err = romancy.ReadChannelAfter(ctx, "events", lastID)
```

Both need a `channel_sub` token on a sealed channel (`romancy.WithReceiveCapability(token)`). Pass `""` or `0` to read from the beginning.

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

    romancy.RegisterWorkflow(app, taskConsumer)
    romancy.RegisterWorkflow(app, taskProducer)

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
if err := romancy.Publish(ctx, "system-alerts", AlertMessage{Text: "deploy finished"}); err != nil {
    return MyResult{}, err
}
```

### Work Queue (Competing)

Distribute work across multiple workers:

```go
// Only one subscriber receives each task
romancy.Subscribe(ctx, "work-queue", romancy.ModeCompeting)
msg, _ := romancy.Receive[WorkItem](ctx, "work-queue")
fmt.Println("received task:", msg.Data)
```

### Direct Messaging (SendTo)

Point-to-point communication with specific workflow instances:

```go
// === Receiver Workflow ===
// Subscribe with ModeDirect to receive SendTo messages
romancy.Subscribe(ctx, "messages", romancy.ModeDirect)

// Receive works with the original channel name
msg, _ := romancy.Receive[Message](ctx, "messages")
fmt.Printf("Received from %s: %s\n", msg.Data.From, msg.Data.Content)

// === Sender Workflow ===
// Send directly to a specific instance
romancy.SendTo(ctx, targetInstanceID, "messages", Message{
    From:    ctx.InstanceID(),
    Content: "Hello!",
})
```

## See Also

- **[Event Waiting](/docs/core-features/events/wait-event)**: Wait for external CloudEvents
- **[Workflows & Activities](/docs/core-features/workflows-activities)**: Core workflow concepts
- **[Transactional Outbox](/docs/core-features/transactional-outbox)**: Reliable event publishing
