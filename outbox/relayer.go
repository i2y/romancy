package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"

	"github.com/i2y/romancy/internal/storage"
)

// EventSender is a function that sends an event to an external system.
// The default implementation uses CloudEvents HTTP client.
type EventSender func(ctx context.Context, event *storage.OutboxEvent) error

// Relayer handles background delivery of outbox events.
type Relayer struct {
	storage      storage.Storage
	sender       EventSender
	targetURL    string
	pollInterval time.Duration
	batchSize    int
	maxRetries   int

	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
}

// RelayerConfig configures the outbox relayer.
type RelayerConfig struct {
	// TargetURL is the CloudEvents endpoint to send events to.
	TargetURL string
	// PollInterval is how often to check for pending events.
	// Default: 1 second.
	PollInterval time.Duration
	// BatchSize is the maximum number of events to process per poll.
	// Default: 100.
	BatchSize int
	// MaxRetries is the maximum number of delivery attempts.
	// Default: 5.
	MaxRetries int
	// CustomSender is an optional custom event sender.
	// If nil, the default CloudEvents HTTP sender is used.
	CustomSender EventSender
}

// NewRelayer creates a new outbox relayer.
func NewRelayer(s storage.Storage, config RelayerConfig) *Relayer {
	if config.PollInterval == 0 {
		config.PollInterval = 1 * time.Second
	}
	if config.BatchSize == 0 {
		config.BatchSize = 100
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 5
	}

	r := &Relayer{
		storage:      s,
		targetURL:    config.TargetURL,
		pollInterval: config.PollInterval,
		batchSize:    config.BatchSize,
		maxRetries:   config.MaxRetries,
		stopCh:       make(chan struct{}),
	}

	if config.CustomSender != nil {
		r.sender = config.CustomSender
	} else {
		r.sender = r.defaultSender
	}

	return r
}

// Start starts the background relayer.
func (r *Relayer) Start(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.stopCh = make(chan struct{})
	r.mu.Unlock()

	r.wg.Add(1)
	go r.run(ctx)
}

// Stop stops the background relayer gracefully.
func (r *Relayer) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stopCh)
	r.mu.Unlock()

	r.wg.Wait()
}

// run is the main loop for the relayer.
func (r *Relayer) run(ctx context.Context) {
	defer r.wg.Done()

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.processPendingEvents(ctx)
		}
	}
}

// processPendingEvents processes a batch of pending events.
func (r *Relayer) processPendingEvents(ctx context.Context) {
	events, err := r.storage.GetPendingOutboxEvents(ctx, r.batchSize)
	if err != nil {
		// Log error but continue
		return
	}

	for _, event := range events {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		default:
		}

		// Check if max retries exceeded
		if event.RetryCount >= r.maxRetries {
			_ = r.storage.MarkOutboxEventFailed(ctx, event.EventID)
			continue
		}

		// Increment attempts
		_ = r.storage.IncrementOutboxAttempts(ctx, event.EventID)

		// Send event
		if err := r.sender(ctx, event); err != nil {
			// Event will be retried on next poll
			continue
		}

		// Mark as sent
		_ = r.storage.MarkOutboxEventSent(ctx, event.EventID)
	}
}

// defaultSender sends events using CloudEvents HTTP client.
func (r *Relayer) defaultSender(ctx context.Context, event *storage.OutboxEvent) error {
	if r.targetURL == "" {
		return fmt.Errorf("target URL not configured")
	}

	// Create CloudEvent
	ce := cloudevents.NewEvent()
	ce.SetID(event.EventID)
	ce.SetType(event.EventType)
	ce.SetSource(event.EventSource)
	ce.SetTime(event.CreatedAt)

	// Parse event data
	var data any
	if err := json.Unmarshal(event.EventData, &data); err != nil {
		return fmt.Errorf("failed to parse event data: %w", err)
	}
	if err := ce.SetData(event.ContentType, data); err != nil {
		return fmt.Errorf("failed to set event data: %w", err)
	}

	// Create HTTP client
	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(r.targetURL),
	)
	if err != nil {
		return fmt.Errorf("failed to create CloudEvents client: %w", err)
	}

	// Send event
	result := client.Send(ctx, ce)
	if cloudevents.IsUndelivered(result) {
		return fmt.Errorf("failed to send event: %w", result)
	}
	if !cloudevents.IsACK(result) {
		return fmt.Errorf("event not acknowledged: %w", result)
	}

	return nil
}

// CleanupOldEvents removes old sent events from the outbox.
func (r *Relayer) CleanupOldEvents(ctx context.Context, olderThan time.Duration) error {
	return r.storage.CleanupOldOutboxEvents(ctx, olderThan)
}

// RelayOnce processes pending events once (useful for testing).
func (r *Relayer) RelayOnce(ctx context.Context) error {
	r.processPendingEvents(ctx)
	return nil
}
