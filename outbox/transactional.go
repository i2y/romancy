// Package outbox provides transactional outbox pattern for reliable event delivery.
package outbox

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/i2y/romancy/internal/storage"
)

// WorkflowContext interface that we need for outbox operations.
// This avoids circular imports with the main romancy package.
type WorkflowContext interface {
	Context() interface{ Done() <-chan struct{} }
	InstanceID() string
	Storage() storage.Storage
}

// contextAdapter adapts WorkflowContext to context.Context for storage calls
type contextAdapter struct {
	wfCtx WorkflowContext
}

func (a *contextAdapter) Done() <-chan struct{} {
	return a.wfCtx.Context().Done()
}

func (a *contextAdapter) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (a *contextAdapter) Err() error {
	return nil
}

func (a *contextAdapter) Value(key any) any {
	return nil
}

// SendEventOptions configures event sending.
type SendEventOptions struct {
	// EventID is the CloudEvents ID. If empty, a UUID will be generated.
	EventID string
	// ContentType is the MIME type of the data. Defaults to "application/json".
	ContentType string
}

// SendEventOption is a functional option for SendEvent.
type SendEventOption func(*SendEventOptions)

// WithEventID sets a custom event ID.
func WithEventID(id string) SendEventOption {
	return func(o *SendEventOptions) {
		o.EventID = id
	}
}

// WithContentType sets the content type.
func WithContentType(contentType string) SendEventOption {
	return func(o *SendEventOptions) {
		o.ContentType = contentType
	}
}

// SendEventTransactional sends an event through the transactional outbox.
// The event is stored in the database within the current transaction (if any)
// and will be delivered asynchronously by the outbox relayer.
//
// This ensures that the event is only sent if the activity/transaction commits,
// providing exactly-once delivery guarantees when combined with idempotent consumers.
func SendEventTransactional[T any](
	ctx WorkflowContext,
	eventType string,
	eventSource string,
	data T,
	opts ...SendEventOption,
) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available in workflow context")
	}

	options := &SendEventOptions{
		ContentType: "application/json",
	}
	for _, opt := range opts {
		opt(options)
	}

	// Generate event ID if not provided
	eventID := options.EventID
	if eventID == "" {
		eventID = uuid.New().String()
	}

	// Serialize data
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize event data: %w", err)
	}

	// Create outbox event
	event := &storage.OutboxEvent{
		EventID:     eventID,
		EventType:   eventType,
		EventSource: eventSource,
		EventData:   dataBytes,
		ContentType: options.ContentType,
		Status:      "pending",
		Attempts:    0,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Add to outbox (within current transaction if any)
	if err := s.AddOutboxEvent(&contextAdapter{ctx}, event); err != nil {
		return fmt.Errorf("failed to add event to outbox: %w", err)
	}

	return nil
}
