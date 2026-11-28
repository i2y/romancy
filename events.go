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
	// CloudEvents attributes. An event that the CloudEvents endpoint of any
	// runtime (Romancy, Edda, Shikibu) stored has them. A message published
	// on the channel from workflow code has none: only Type is set, to the
	// type waited for.
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Source      string     `json:"source"`
	SpecVersion string     `json:"specversion"`
	Time        *time.Time `json:"time,omitempty"`
	Subject     string     `json:"subject,omitempty"`
	DataSchema  string     `json:"dataschema,omitempty"`
	// DataContentType is the media type of the data, if the event has one.
	// JSON data decodes into T. Text (text/*) is a string. Other data is
	// bytes, which T = []byte, string or any (holding a []byte) receives.
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

	// Use Receive internally: the message data is the event's data, and its
	// metadata the event's attributes.
	msg, err := Receive[json.RawMessage](ctx, eventType, receiveOpts...)
	if err != nil {
		return nil, err // SuspendSignal or other errors propagate
	}

	// Convert ReceivedMessage to ReceivedEvent
	return convertToReceivedEvent[T](msg, eventType)
}

// convertToReceivedEvent reads a received channel message as an event. The
// CloudEvents endpoint of every runtime stores an event as a message whose
// data is the event's data and whose metadata holds the event's attributes
// under ce_ keys (SEMANTICS §3c). Earlier versions of Romancy stored the
// attributes and the data together in the message data; such messages, the
// pending ones and the receives that histories recorded, read as before. A
// message without attributes, such as one published from workflow code, is
// read as its data, of the type waited for.
func convertToReceivedEvent[T any](msg *ReceivedMessage[json.RawMessage], eventType string) (*ReceivedEvent[T], error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}
	var event *ReceivedEvent[T]
	data := msg.Data
	switch {
	case isEventAttributes(msg.Metadata):
		event = eventFromAttributes[T](msg.Metadata, "ce_")
	case !msg.binary:
		var envelope map[string]json.RawMessage
		if json.Unmarshal(msg.Data, &envelope) == nil && isEventEnvelope(envelope) {
			attributes := make(map[string]any, len(envelope))
			for name, value := range envelope {
				if name != "data" {
					var decoded any
					_ = json.Unmarshal(value, &decoded)
					attributes[name] = decoded
				}
			}
			event, data = eventFromAttributes[T](attributes, ""), envelope["data"]
		}
	}
	if event == nil {
		event = &ReceivedEvent[T]{Type: eventType}
	}
	if msg.binary {
		if err := decodeBytes(msg.binaryData, &event.Data); err != nil {
			return nil, fmt.Errorf("failed to deserialize event data: %w", err)
		}
		return event, nil
	}
	if err := decodeEventData(data, &event.Data); err != nil {
		return nil, err
	}
	return event, nil
}

// isEventAttributes reports whether a message's metadata holds the
// attributes of an event under ce_ keys, as the CloudEvents endpoint of
// every runtime stores them.
func isEventAttributes(metadata map[string]any) bool {
	_, ok := metadata["ce_type"].(string)
	return ok
}

// isEventEnvelope reports whether a message's data is an event as earlier
// versions of the CloudEvents endpoint stored it: an object that always had
// specversion and type, with the data under "data" and the extension
// attributes under "extensions".
func isEventEnvelope(envelope map[string]json.RawMessage) bool {
	var specVersion, eventType *string
	return json.Unmarshal(envelope["specversion"], &specVersion) == nil && specVersion != nil &&
		json.Unmarshal(envelope["type"], &eventType) == nil && eventType != nil && *eventType != ""
}

// eventFromAttributes builds an event, without its data, from its
// attributes, named with prefix (the ce_ keys of a message's metadata, or the
// names of an envelope).
func eventFromAttributes[T any](attributes map[string]any, prefix string) *ReceivedEvent[T] {
	attribute := func(name string) string {
		value, _ := attributes[prefix+name].(string)
		return value
	}
	event := &ReceivedEvent[T]{
		ID:              attribute("id"),
		Type:            attribute("type"),
		Source:          attribute("source"),
		SpecVersion:     attribute("specversion"),
		Subject:         attribute("subject"),
		DataSchema:      attribute("dataschema"),
		DataContentType: attribute("datacontenttype"),
	}
	if t := attribute("time"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			event.Time = &parsed
		}
	}
	if extensions, ok := attributes[prefix+"extensions"].(map[string]any); ok {
		event.Extensions = extensions
	}
	return event
}

// decodeEventData decodes an event's JSON data into T (none leaves T's zero
// value).
func decodeEventData[T any](data json.RawMessage, into *T) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("failed to deserialize event data: %w", err)
	}
	return nil
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
