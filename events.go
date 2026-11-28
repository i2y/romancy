package romancy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/outbox"
)

// ReceivedEvent represents an event received by a workflow.
type ReceivedEvent[T any] struct {
	// CloudEvents metadata
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Source      string     `json:"source"`
	SpecVersion string     `json:"specversion"`
	Time        *time.Time `json:"time,omitempty"`
	Subject     string     `json:"subject,omitempty"`
	// DataContentType is the media type of the data, if the event has one.
	// JSON data decodes into T. Text (text/*) is a string, and other data
	// is its bytes in base64: receive them with T = string or T = []byte.
	DataContentType string         `json:"datacontenttype,omitempty"`
	Extensions      map[string]any `json:"extensions,omitempty"`

	// Event data
	Data T `json:"data"`
}

// WaitEvent suspends the workflow until an event of the specified type is received.
// The event data will be deserialized into type T.
//
// Internally, this uses the channel messaging system. The event_type is used as
// the channel name, and the workflow subscribes in broadcast mode.
//
// WaitEvent also receives the events targeted at this instance (CloudEvents
// with the romancyinstanceid extension), which are stored on its own direct
// channel "{eventType}:{instanceID}" so that no other instance sees them. It
// takes the oldest pending event of the two channels, and its wait ends at
// the first event on either. A targeted event that arrived before WaitEvent
// is not lost: a direct channel keeps its messages until they are received.
// After Subscribe(ctx, eventType, ModeDirect), WaitEvent receives only the
// targeted events.
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

	// Convert WaitEventOption to ReceiveOption. The receive also reads the
	// instance's own direct channel, where its targeted events are stored.
	receiveOpts := []ReceiveOption{withTargetedEvents()}
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
	if subject, ok := msg.Data["subject"].(string); ok {
		event.Subject = subject
	}
	if contentType, ok := msg.Data["datacontenttype"].(string); ok {
		event.DataContentType = contentType
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
	return sleepUntil(ctx, func() time.Time { return time.Now().UTC().Add(duration) }, opts)
}

// sleepUntil suspends the workflow until the time deadline returns. The
// timer ID is taken before anything else, also when the deadline has
// already passed, so a replay generates the same IDs whatever the clock says
// by then; deadline is called on the first execution only.
func sleepUntil(ctx *WorkflowContext, deadline func() time.Time, opts []SleepOption) error {
	options := &sleepOptions{}
	for _, opt := range opts {
		opt(options)
	}
	if err := ctx.abortedErr(); err != nil {
		return err
	}

	// Generate timer ID
	timerID := options.timerID
	if timerID == "" {
		timerID = ctx.GenerateActivityID("sleep")
	} else {
		ctx.useExplicitID(timerID)
	}

	// Check if result is cached (replay)
	if _, ok := ctx.GetCachedResult(timerID); ok {
		return nil
	}

	// Calculate expiry time (must be calculated on first execution only).
	// A deadline that has passed returns at once; a future one is rounded up
	// to the second the storage can hold, so the timer never fires early.
	expiresAt := deadline().UTC()
	if !expiresAt.After(time.Now()) {
		return nil
	}
	expiresAt = storage.RoundUpDeadline(expiresAt)

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
	return NewTimerSuspend(ctx.InstanceID(), timerID, timerID, expiresAt)
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
	return sleepUntil(ctx, func() time.Time { return t }, opts)
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
// Call it from an activity (or a compensation). Nothing records an event sent
// from workflow code, so each replay of the workflow sends it again; such a
// call logs a warning.
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
	warnSendOutsideActivity(ctx, eventType)
	return outbox.SendEventTransactional(&workflowContextAdapter{ctx}, eventType, source, data)
}

// SendEventTransactional is an alias for outbox.SendEventTransactional.
// Use this when you need more control over event options. Like SendEvent, call
// it from an activity.
//
// Example:
//
//	err := romancy.SendEventTransactional(wfCtx, "order.created", "order-service", orderData,
//	    outbox.WithEventID("custom-event-id"),
//	    outbox.WithContentType("application/json"),
//	)
func SendEventTransactional[T any](ctx *WorkflowContext, eventType, source string, data T, opts ...outbox.SendEventOption) error {
	warnSendOutsideActivity(ctx, eventType)
	return outbox.SendEventTransactional(&workflowContextAdapter{ctx}, eventType, source, data, opts...)
}

// warnSendOutsideActivity logs a warning for an event sent from workflow code:
// nothing records it, so every replay of the workflow sends it again.
func warnSendOutsideActivity(ctx *WorkflowContext, eventType string) {
	if ctx != nil && !ctx.InActivity() {
		slog.Warn("event sent outside an activity: it is sent again each time the workflow replays; send it from an activity",
			"event_type", eventType, "instance_id", ctx.InstanceID())
	}
}
