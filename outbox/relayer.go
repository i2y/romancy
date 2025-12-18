package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
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
	maxBackoff   time.Duration

	// wakeEvent is an optional channel to wake the relayer on NOTIFY events.
	// When a message is received, the relayer immediately checks for pending events.
	wakeEvent <-chan struct{}

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
	// MaxBackoff is the maximum backoff duration when no events are pending.
	// Default: 30 seconds.
	MaxBackoff time.Duration
	// CustomSender is an optional custom event sender.
	// If nil, the default CloudEvents HTTP sender is used.
	CustomSender EventSender
	// WakeEvent is an optional channel to wake the relayer on NOTIFY events.
	// When provided, the relayer will immediately check for pending events
	// when a message is received on this channel.
	WakeEvent <-chan struct{}
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
	if config.MaxBackoff == 0 {
		config.MaxBackoff = 30 * time.Second
	}

	r := &Relayer{
		storage:      s,
		targetURL:    config.TargetURL,
		pollInterval: config.PollInterval,
		batchSize:    config.BatchSize,
		maxRetries:   config.MaxRetries,
		maxBackoff:   config.MaxBackoff,
		wakeEvent:    config.WakeEvent,
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

// run is the main loop for the relayer with adaptive backoff.
func (r *Relayer) run(ctx context.Context) {
	defer r.wg.Done()

	consecutiveEmpty := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		default:
		}

		count, err := r.processPendingEvents(ctx)
		switch {
		case err != nil:
			slog.Error("outbox relayer error", "error", err)
			consecutiveEmpty = 0
		case count == 0:
			consecutiveEmpty++
		default:
			consecutiveEmpty = 0
		}

		// Calculate adaptive backoff
		backoff := r.calculateBackoff(consecutiveEmpty)
		jitter := time.Duration(rand.Float64() * float64(backoff) * 0.3)
		waitTime := backoff + jitter

		// Wait with optional NOTIFY wake
		if r.wakeEvent != nil {
			select {
			case <-r.wakeEvent:
				consecutiveEmpty = 0
				slog.Debug("outbox relayer woken by NOTIFY")
			case <-time.After(waitTime):
			case <-r.stopCh:
				return
			case <-ctx.Done():
				return
			}
		} else {
			select {
			case <-time.After(waitTime):
			case <-r.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

// calculateBackoff computes exponential backoff with max cap.
func (r *Relayer) calculateBackoff(consecutiveEmpty int) time.Duration {
	if consecutiveEmpty == 0 {
		return r.pollInterval
	}
	exp := min(consecutiveEmpty, 5) // Cap exponent to prevent overflow
	backoff := time.Duration(float64(r.pollInterval) * math.Pow(2, float64(exp)))
	if backoff > r.maxBackoff {
		return r.maxBackoff
	}
	return backoff
}

// processPendingEvents processes a batch of pending events.
// Returns the number of events processed and any error that occurred during fetch.
func (r *Relayer) processPendingEvents(ctx context.Context) (int, error) {
	events, err := r.storage.GetPendingOutboxEvents(ctx, r.batchSize)
	if err != nil {
		return 0, err
	}

	if len(events) == 0 {
		return 0, nil
	}

	for _, event := range events {
		select {
		case <-ctx.Done():
			return len(events), nil
		case <-r.stopCh:
			return len(events), nil
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

	return len(events), nil
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
// Returns the number of events processed and any error.
func (r *Relayer) RelayOnce(ctx context.Context) (int, error) {
	return r.processPendingEvents(ctx)
}
