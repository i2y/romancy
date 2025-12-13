package romancy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/outbox"
)

// ReceivedEvent represents an event received by a workflow.
type ReceivedEvent[T any] struct {
	// CloudEvents metadata
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Source      string         `json:"source"`
	SpecVersion string         `json:"specversion"`
	Time        *time.Time     `json:"time,omitempty"`
	Extensions  map[string]any `json:"extensions,omitempty"`

	// Event data
	Data T `json:"data"`
}

// WaitEvent suspends the workflow until an event of the specified type is received.
// The event data will be deserialized into type T.
//
// Internally, this uses the channel messaging system. The event_type is used as
// the channel name, and the workflow subscribes in broadcast mode.
//
// During replay, if the event was already received, this returns immediately.
// Otherwise, the workflow is suspended until an event arrives on the channel.
//
// When the event arrives (published to the channel), the workflow will be resumed.
func WaitEvent[T any](ctx *WorkflowContext, eventType string, opts ...WaitEventOption) (*ReceivedEvent[T], error) {
	options := &waitEventOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Call OnEventWaitStart hook
	if h := ctx.Hooks(); h != nil {
		h.OnEventWaitStart(ctx.Context(), hooks.EventWaitStartInfo{
			InstanceID:   ctx.InstanceID(),
			WorkflowName: ctx.WorkflowName(),
			EventType:    eventType,
			Timeout:      options.timeout,
		})
	}

	// Subscribe to the event type channel (idempotent)
	if err := Subscribe(ctx, eventType, ModeBroadcast); err != nil {
		return nil, fmt.Errorf("failed to subscribe to event channel: %w", err)
	}

	// Convert WaitEventOption to ReceiveOption
	var receiveOpts []ReceiveOption
	if options.timeout != nil {
		receiveOpts = append(receiveOpts, WithReceiveTimeout(*options.timeout))
	}

	// Use Receive internally - the channel message contains CloudEvent data
	msg, err := Receive[map[string]any](ctx, eventType, receiveOpts...)
	if err != nil {
		return nil, err // SuspendSignal or other errors propagate
	}

	// Convert ReceivedMessage to ReceivedEvent
	return convertToReceivedEvent[T](msg)
}

// convertToReceivedEvent converts a ReceivedMessage containing CloudEvent data
// to a ReceivedEvent with proper typing.
func convertToReceivedEvent[T any](msg *ReceivedMessage[map[string]any]) (*ReceivedEvent[T], error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}

	event := &ReceivedEvent[T]{}

	// Extract CloudEvent metadata from the message data
	if id, ok := msg.Data["id"].(string); ok {
		event.ID = id
	}
	if typ, ok := msg.Data["type"].(string); ok {
		event.Type = typ
	}
	if source, ok := msg.Data["source"].(string); ok {
		event.Source = source
	}
	if specVersion, ok := msg.Data["specversion"].(string); ok {
		event.SpecVersion = specVersion
	}
	switch t := msg.Data["time"].(type) {
	case *time.Time:
		event.Time = t
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			event.Time = &parsed
		}
	}
	if extensions, ok := msg.Data["extensions"].(map[string]any); ok {
		event.Extensions = extensions
	}

	// Extract and deserialize the actual event data
	if data, ok := msg.Data["data"]; ok {
		// Re-serialize and deserialize to get proper type
		dataJSON, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize event data: %w", err)
		}
		var typedData T
		if err := json.Unmarshal(dataJSON, &typedData); err != nil {
			return nil, fmt.Errorf("failed to deserialize event data: %w", err)
		}
		event.Data = typedData
	}

	return event, nil
}

// WaitEventOption configures event waiting behavior.
type WaitEventOption func(*waitEventOptions)

type waitEventOptions struct {
	timeout *time.Duration
}

// WithEventTimeout sets a timeout for waiting for an event.
func WithEventTimeout(d time.Duration) WaitEventOption {
	return func(o *waitEventOptions) {
		o.timeout = &d
	}
}

// Sleep suspends the workflow for a specified duration.
// The workflow will resume after the duration has elapsed.
//
// During replay, if the timer already fired, this returns immediately.
// Otherwise, it returns a SuspendSignal that signals the engine to:
// 1. Register a timer subscription
// 2. Update workflow status to "waiting_for_timer"
// 3. Release the workflow lock
//
// When the timer expires, the workflow will be resumed.
func Sleep(ctx *WorkflowContext, duration time.Duration, opts ...SleepOption) error {
	options := &sleepOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Generate timer ID
	timerID := options.timerID
	if timerID == "" {
		timerID = ctx.GenerateActivityID("sleep")
	}

	// Check if result is cached (replay)
	if _, ok := ctx.GetCachedResult(timerID); ok {
		return nil
	}

	// Calculate expiry time (must be calculated on first execution only)
	expiresAt := time.Now().Add(duration)

	// Call OnTimerStart hook
	if h := ctx.Hooks(); h != nil {
		h.OnTimerStart(ctx.Context(), hooks.TimerStartInfo{
			InstanceID:   ctx.InstanceID(),
			WorkflowName: ctx.WorkflowName(),
			TimerID:      timerID,
			ExpiresAt:    expiresAt,
		})
	}

	// Record the timer ID for tracking
	ctx.RecordActivityID(timerID)

	// Return SuspendSignal that the replay engine will handle
	return NewTimerSuspend(ctx.InstanceID(), timerID, expiresAt, 0)
}

// SleepOption configures sleep behavior.
type SleepOption func(*sleepOptions)

type sleepOptions struct {
	timerID string
}

// WithSleepID sets a custom timer ID for the sleep.
// This is useful for identifying timers in logs and for deterministic replay.
func WithSleepID(id string) SleepOption {
	return func(o *sleepOptions) {
		o.timerID = id
	}
}

// SleepUntil suspends the workflow until a specific time.
func SleepUntil(ctx *WorkflowContext, t time.Time, opts ...SleepOption) error {
	duration := time.Until(t)
	if duration <= 0 {
		// Already past the target time
		return nil
	}
	return Sleep(ctx, duration, opts...)
}

// workflowContextAdapter adapts *WorkflowContext to outbox.WorkflowContext interface.
type workflowContextAdapter struct {
	ctx *WorkflowContext
}

func (a *workflowContextAdapter) Context() interface{ Done() <-chan struct{} } {
	return a.ctx.Context()
}

func (a *workflowContextAdapter) InstanceID() string {
	return a.ctx.InstanceID()
}

func (a *workflowContextAdapter) Storage() storage.Storage {
	return a.ctx.Storage()
}

// SendEvent sends an event through the transactional outbox.
// This is a convenience wrapper for outbox.SendEventTransactional.
//
// The event is stored in the database within the current transaction (if any)
// and will be delivered asynchronously by the outbox relayer.
//
// This ensures that the event is only sent if the activity/transaction commits,
// providing exactly-once delivery guarantees when combined with idempotent consumers.
//
// Example:
//
//	activity := romancy.DefineActivity("process_order", func(ctx context.Context, input OrderInput) (OrderResult, error) {
//	    wfCtx := romancy.GetWorkflowContext(ctx)
//	    if wfCtx == nil {
//	        return OrderResult{}, fmt.Errorf("workflow context not available")
//	    }
//	    err := romancy.SendEvent(wfCtx, "order.created", "order-service", map[string]any{
//	        "order_id": input.OrderID,
//	        "amount":   input.Amount,
//	    })
//	    if err != nil {
//	        return OrderResult{}, err
//	    }
//	    return OrderResult{Status: "created"}, nil
//	})
func SendEvent[T any](ctx *WorkflowContext, eventType, source string, data T) error {
	return outbox.SendEventTransactional(&workflowContextAdapter{ctx}, eventType, source, data)
}

// SendEventTransactional is an alias for outbox.SendEventTransactional.
// Use this when you need more control over event options.
//
// Example:
//
//	err := romancy.SendEventTransactional(wfCtx, "order.created", "order-service", orderData,
//	    outbox.WithEventID("custom-event-id"),
//	    outbox.WithContentType("application/json"),
//	)
func SendEventTransactional[T any](ctx *WorkflowContext, eventType, source string, data T, opts ...outbox.SendEventOption) error {
	return outbox.SendEventTransactional(&workflowContextAdapter{ctx}, eventType, source, data, opts...)
}
