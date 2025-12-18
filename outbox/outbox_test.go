package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// mockStorage implements storage.Storage interface for testing.
// Only outbox-related methods are implemented; others are stubs.
type mockStorage struct {
	events []*storage.OutboxEvent
}

// OutboxManager methods (implemented)

func (m *mockStorage) AddOutboxEvent(ctx context.Context, event *storage.OutboxEvent) error {
	m.events = append(m.events, event)
	return nil
}

func (m *mockStorage) GetPendingOutboxEvents(ctx context.Context, limit int) ([]*storage.OutboxEvent, error) {
	var pending []*storage.OutboxEvent
	for _, e := range m.events {
		if e.Status == "pending" {
			pending = append(pending, e)
			if len(pending) >= limit {
				break
			}
		}
	}
	return pending, nil
}

func (m *mockStorage) MarkOutboxEventSent(ctx context.Context, eventID string) error {
	for _, e := range m.events {
		if e.EventID == eventID {
			e.Status = "sent"
			break
		}
	}
	return nil
}

func (m *mockStorage) MarkOutboxEventFailed(ctx context.Context, eventID string) error {
	for _, e := range m.events {
		if e.EventID == eventID {
			e.Status = "failed"
			break
		}
	}
	return nil
}

func (m *mockStorage) IncrementOutboxAttempts(ctx context.Context, eventID string) error {
	for _, e := range m.events {
		if e.EventID == eventID {
			e.RetryCount++
			break
		}
	}
	return nil
}

func (m *mockStorage) CleanupOldOutboxEvents(ctx context.Context, olderThan time.Duration) error {
	return nil
}

// Stub implementations for remaining Storage interface methods

func (m *mockStorage) Initialize(ctx context.Context) error { return nil }
func (m *mockStorage) Close() error                         { return nil }
func (m *mockStorage) DB() *sql.DB                          { return nil }

// TransactionManager stubs
func (m *mockStorage) BeginTransaction(ctx context.Context) (context.Context, error) { return ctx, nil }
func (m *mockStorage) CommitTransaction(ctx context.Context) error                   { return nil }
func (m *mockStorage) RollbackTransaction(ctx context.Context) error                 { return nil }
func (m *mockStorage) InTransaction(ctx context.Context) bool                        { return false }
func (m *mockStorage) Conn(ctx context.Context) storage.Executor                     { return nil }
func (m *mockStorage) RegisterPostCommitCallback(ctx context.Context, cb func() error) error {
	return nil
}

// InstanceManager stubs
func (m *mockStorage) CreateInstance(ctx context.Context, instance *storage.WorkflowInstance) error {
	return nil
}
func (m *mockStorage) GetInstance(ctx context.Context, instanceID string) (*storage.WorkflowInstance, error) {
	return nil, nil
}
func (m *mockStorage) UpdateInstanceStatus(ctx context.Context, instanceID string, status storage.WorkflowStatus, errorMsg string) error {
	return nil
}
func (m *mockStorage) UpdateInstanceActivity(ctx context.Context, instanceID string, activityID string) error {
	return nil
}
func (m *mockStorage) UpdateInstanceOutput(ctx context.Context, instanceID string, outputData []byte) error {
	return nil
}
func (m *mockStorage) CancelInstance(ctx context.Context, instanceID string, reason string) error {
	return nil
}
func (m *mockStorage) ListInstances(ctx context.Context, opts storage.ListInstancesOptions) (*storage.PaginationResult, error) {
	return &storage.PaginationResult{}, nil
}
func (m *mockStorage) FindResumableWorkflows(ctx context.Context, limit int) ([]*storage.ResumableWorkflow, error) {
	return nil, nil
}

// LockManager stubs
func (m *mockStorage) TryAcquireLock(ctx context.Context, instanceID, workerID string, timeoutSec int) (bool, error) {
	return true, nil
}
func (m *mockStorage) ReleaseLock(ctx context.Context, instanceID, workerID string) error { return nil }
func (m *mockStorage) RefreshLock(ctx context.Context, instanceID, workerID string, timeoutSec int) error {
	return nil
}
func (m *mockStorage) CleanupStaleLocks(ctx context.Context, timeoutSec int) ([]storage.StaleWorkflowInfo, error) {
	return nil, nil
}

// HistoryManager stubs
func (m *mockStorage) AppendHistory(ctx context.Context, event *storage.HistoryEvent) error {
	return nil
}
func (m *mockStorage) GetHistoryPaginated(ctx context.Context, instanceID string, afterID int64, limit int) ([]*storage.HistoryEvent, bool, error) {
	return nil, false, nil
}
func (m *mockStorage) GetHistoryCount(ctx context.Context, instanceID string) (int64, error) {
	return 0, nil
}

// TimerSubscriptionManager stubs
func (m *mockStorage) RegisterTimerSubscription(ctx context.Context, sub *storage.TimerSubscription) error {
	return nil
}
func (m *mockStorage) RegisterTimerSubscriptionAndReleaseLock(ctx context.Context, sub *storage.TimerSubscription, instanceID, workerID string) error {
	return nil
}
func (m *mockStorage) FindExpiredTimers(ctx context.Context, limit int) ([]*storage.TimerSubscription, error) {
	return nil, nil
}
func (m *mockStorage) RemoveTimerSubscription(ctx context.Context, instanceID, timerID string) error {
	return nil
}

// CompensationManager stubs
func (m *mockStorage) AddCompensation(ctx context.Context, entry *storage.CompensationEntry) error {
	return nil
}
func (m *mockStorage) GetCompensations(ctx context.Context, instanceID string) ([]*storage.CompensationEntry, error) {
	return nil, nil
}

// ChannelManager stubs
func (m *mockStorage) PublishToChannel(ctx context.Context, channelName string, dataJSON []byte, metadata []byte) (int64, error) {
	return 0, nil
}
func (m *mockStorage) SubscribeToChannel(ctx context.Context, instanceID, channelName string, mode storage.ChannelMode) error {
	return nil
}
func (m *mockStorage) UnsubscribeFromChannel(ctx context.Context, instanceID, channelName string) error {
	return nil
}
func (m *mockStorage) GetChannelSubscription(ctx context.Context, instanceID, channelName string) (*storage.ChannelSubscription, error) {
	return nil, nil
}
func (m *mockStorage) RegisterChannelReceiveAndReleaseLock(ctx context.Context, instanceID, channelName, workerID, activityID string, timeoutAt *time.Time) error {
	return nil
}
func (m *mockStorage) GetPendingChannelMessages(ctx context.Context, channelName string, afterID int64, limit int) ([]*storage.ChannelMessage, error) {
	return nil, nil
}
func (m *mockStorage) ClaimChannelMessage(ctx context.Context, messageID int64, instanceID string) (bool, error) {
	return false, nil
}
func (m *mockStorage) DeleteChannelMessage(ctx context.Context, messageID int64) error { return nil }
func (m *mockStorage) UpdateDeliveryCursor(ctx context.Context, instanceID, channelName string, lastMessageID int64) error {
	return nil
}
func (m *mockStorage) GetDeliveryCursor(ctx context.Context, instanceID, channelName string) (int64, error) {
	return 0, nil
}
func (m *mockStorage) GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*storage.ChannelSubscription, error) {
	return nil, nil
}
func (m *mockStorage) ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error {
	return nil
}
func (m *mockStorage) DeliverChannelMessage(ctx context.Context, instanceID string, message *storage.ChannelMessage) error {
	return nil
}
func (m *mockStorage) DeliverChannelMessageWithLock(ctx context.Context, instanceID, channelName string, message *storage.ChannelMessage, workerID string, lockTimeoutSec int) (*storage.ChannelDeliveryResult, error) {
	return nil, nil
}
func (m *mockStorage) GetPendingChannelMessagesForInstance(ctx context.Context, instanceID, channelName string) ([]*storage.ChannelMessage, error) {
	return nil, nil
}
func (m *mockStorage) CleanupOldChannelMessages(ctx context.Context, olderThan time.Duration) error {
	return nil
}
func (m *mockStorage) FindExpiredChannelSubscriptions(ctx context.Context, limit int) ([]*storage.ChannelSubscription, error) {
	return nil, nil
}

// GroupManager stubs
func (m *mockStorage) JoinGroup(ctx context.Context, instanceID, groupName string) error {
	return nil
}
func (m *mockStorage) LeaveGroup(ctx context.Context, instanceID, groupName string) error {
	return nil
}
func (m *mockStorage) GetGroupMembers(ctx context.Context, groupName string) ([]string, error) {
	return nil, nil
}
func (m *mockStorage) LeaveAllGroups(ctx context.Context, instanceID string) error { return nil }

// SystemLockManager stubs
func (m *mockStorage) TryAcquireSystemLock(ctx context.Context, lockName, workerID string, timeoutSec int) (bool, error) {
	return true, nil
}
func (m *mockStorage) ReleaseSystemLock(ctx context.Context, lockName, workerID string) error {
	return nil
}
func (m *mockStorage) CleanupExpiredSystemLocks(ctx context.Context) error { return nil }

// HistoryArchiveManager stubs
func (m *mockStorage) ArchiveHistory(ctx context.Context, instanceID string) error { return nil }
func (m *mockStorage) GetArchivedHistory(ctx context.Context, instanceID string) ([]*storage.ArchivedHistoryEvent, error) {
	return nil, nil
}
func (m *mockStorage) CleanupInstanceSubscriptions(ctx context.Context, instanceID string) error {
	return nil
}

func TestSendEventOptions(t *testing.T) {
	opts := &SendEventOptions{}

	WithEventID("custom-id")(opts)
	if opts.EventID != "custom-id" {
		t.Errorf("expected EventID=custom-id, got %s", opts.EventID)
	}

	WithContentType("application/xml")(opts)
	if opts.ContentType != "application/xml" {
		t.Errorf("expected ContentType=application/xml, got %s", opts.ContentType)
	}
}

func TestRelayerConfig(t *testing.T) {
	s := &mockStorage{}
	config := RelayerConfig{}

	r := NewRelayer(s, config)

	// Check defaults
	if r.pollInterval != 1*time.Second {
		t.Errorf("expected pollInterval=1s, got %v", r.pollInterval)
	}
	if r.batchSize != 100 {
		t.Errorf("expected batchSize=100, got %d", r.batchSize)
	}
	if r.maxRetries != 5 {
		t.Errorf("expected maxRetries=5, got %d", r.maxRetries)
	}
}

func TestRelayerCustomConfig(t *testing.T) {
	s := &mockStorage{}
	config := RelayerConfig{
		TargetURL:    "http://localhost:8080",
		PollInterval: 5 * time.Second,
		BatchSize:    50,
		MaxRetries:   3,
	}

	r := NewRelayer(s, config)

	if r.targetURL != "http://localhost:8080" {
		t.Errorf("expected targetURL=http://localhost:8080, got %s", r.targetURL)
	}
	if r.pollInterval != 5*time.Second {
		t.Errorf("expected pollInterval=5s, got %v", r.pollInterval)
	}
	if r.batchSize != 50 {
		t.Errorf("expected batchSize=50, got %d", r.batchSize)
	}
	if r.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d", r.maxRetries)
	}
}

func TestRelayerWithCustomSender(t *testing.T) {
	s := &mockStorage{}

	var sendCount int32
	customSender := func(ctx context.Context, event *storage.OutboxEvent) error {
		atomic.AddInt32(&sendCount, 1)
		return nil
	}

	config := RelayerConfig{
		CustomSender: customSender,
	}

	r := NewRelayer(s, config)

	// Add a pending event
	event := &storage.OutboxEvent{
		EventID:     "test-event-1",
		EventType:   "test.event",
		EventSource: "test",
		Status:      "pending",
	}
	s.events = append(s.events, event)

	// Process events
	_, _ = r.RelayOnce(context.Background())

	if atomic.LoadInt32(&sendCount) != 1 {
		t.Errorf("expected sendCount=1, got %d", sendCount)
	}

	// Check event was marked as sent
	if s.events[0].Status != "sent" {
		t.Errorf("expected status=sent, got %s", s.events[0].Status)
	}
}

func TestRelayerMaxRetries(t *testing.T) {
	s := &mockStorage{}

	// Sender that always fails
	failingSender := func(ctx context.Context, event *storage.OutboxEvent) error {
		return context.DeadlineExceeded
	}

	config := RelayerConfig{
		CustomSender: failingSender,
		MaxRetries:   3,
	}

	r := NewRelayer(s, config)

	// Add a pending event with max retries exceeded
	event := &storage.OutboxEvent{
		EventID:     "test-event-1",
		EventType:   "test.event",
		EventSource: "test",
		Status:      "pending",
		RetryCount:  3, // Already at max
	}
	s.events = append(s.events, event)

	// Process events
	_, _ = r.RelayOnce(context.Background())

	// Check event was marked as failed (not retried)
	if s.events[0].Status != "failed" {
		t.Errorf("expected status=failed, got %s", s.events[0].Status)
	}
}

func TestRelayerStartStop(t *testing.T) {
	s := &mockStorage{}

	var sendCount int32
	customSender := func(ctx context.Context, event *storage.OutboxEvent) error {
		atomic.AddInt32(&sendCount, 1)
		return nil
	}

	config := RelayerConfig{
		CustomSender: customSender,
		PollInterval: 50 * time.Millisecond,
	}

	r := NewRelayer(s, config)

	// Add a pending event
	event := &storage.OutboxEvent{
		EventID:     "test-event-1",
		EventType:   "test.event",
		EventSource: "test",
		Status:      "pending",
	}
	s.events = append(s.events, event)

	ctx := context.Background()
	r.Start(ctx)

	// Wait for at least one poll
	time.Sleep(100 * time.Millisecond)

	r.Stop()

	if atomic.LoadInt32(&sendCount) < 1 {
		t.Error("expected at least one event to be sent")
	}
}

type TestEventData struct {
	OrderID string `json:"order_id"`
	Amount  int    `json:"amount"`
}

func TestEventDataSerialization(t *testing.T) {
	data := TestEventData{
		OrderID: "ORD-123",
		Amount:  100,
	}

	bytes, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded TestEventData
	if err := json.Unmarshal(bytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.OrderID != "ORD-123" {
		t.Errorf("expected OrderID=ORD-123, got %s", decoded.OrderID)
	}
	if decoded.Amount != 100 {
		t.Errorf("expected Amount=100, got %d", decoded.Amount)
	}
}

func TestRelayerCalculateBackoff(t *testing.T) {
	s := &mockStorage{}
	config := RelayerConfig{
		PollInterval: 1 * time.Second,
		MaxBackoff:   30 * time.Second,
	}

	r := NewRelayer(s, config)

	tests := []struct {
		name             string
		consecutiveEmpty int
		expected         time.Duration
	}{
		{"zero returns poll interval", 0, 1 * time.Second},
		{"1 returns 2x poll interval", 1, 2 * time.Second},
		{"2 returns 4x poll interval", 2, 4 * time.Second},
		{"3 returns 8x poll interval", 3, 8 * time.Second},
		{"4 returns 16x poll interval", 4, 16 * time.Second},
		{"5 returns 32x (capped to maxBackoff)", 5, 30 * time.Second},
		{"10 returns maxBackoff (exponent capped)", 10, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.calculateBackoff(tt.consecutiveEmpty)
			if result != tt.expected {
				t.Errorf("calculateBackoff(%d) = %v, want %v",
					tt.consecutiveEmpty, result, tt.expected)
			}
		})
	}
}

func TestRelayerAdaptiveBackoffWithEvents(t *testing.T) {
	s := &mockStorage{}

	var processCount int32
	customSender := func(ctx context.Context, event *storage.OutboxEvent) error {
		atomic.AddInt32(&processCount, 1)
		return nil
	}

	config := RelayerConfig{
		CustomSender: customSender,
		PollInterval: 10 * time.Millisecond,
		MaxBackoff:   100 * time.Millisecond,
	}

	r := NewRelayer(s, config)

	// Add multiple pending events
	for i := 0; i < 5; i++ {
		event := &storage.OutboxEvent{
			EventID:     "test-event-" + string(rune('0'+i)),
			EventType:   "test.event",
			EventSource: "test",
			Status:      "pending",
		}
		s.events = append(s.events, event)
	}

	// Process events - should process all quickly without increasing backoff
	count, err := r.RelayOnce(context.Background())
	if err != nil {
		t.Fatalf("RelayOnce failed: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5 events processed, got %d", count)
	}
	if atomic.LoadInt32(&processCount) != 5 {
		t.Errorf("expected 5 sends, got %d", processCount)
	}

	// Now all events should be sent, next RelayOnce should return 0
	count, err = r.RelayOnce(context.Background())
	if err != nil {
		t.Fatalf("RelayOnce failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 events processed, got %d", count)
	}
}

func TestRelayerWakeEvent(t *testing.T) {
	s := &mockStorage{}

	var sendCount int32
	customSender := func(ctx context.Context, event *storage.OutboxEvent) error {
		atomic.AddInt32(&sendCount, 1)
		return nil
	}

	wakeChan := make(chan struct{}, 1)

	config := RelayerConfig{
		CustomSender: customSender,
		PollInterval: 1 * time.Second, // Long poll interval
		MaxBackoff:   30 * time.Second,
		WakeEvent:    wakeChan,
	}

	r := NewRelayer(s, config)

	// Start relayer
	ctx := context.Background()
	r.Start(ctx)

	// Give relayer time to start and enter wait state
	time.Sleep(50 * time.Millisecond)

	// Add event and send wake signal
	event := &storage.OutboxEvent{
		EventID:     "test-event-wake",
		EventType:   "test.event",
		EventSource: "test",
		Status:      "pending",
	}
	s.events = append(s.events, event)

	// Send wake signal
	wakeChan <- struct{}{}

	// Wait for event to be processed
	time.Sleep(100 * time.Millisecond)

	r.Stop()

	if atomic.LoadInt32(&sendCount) < 1 {
		t.Error("expected at least one event to be sent after wake signal")
	}
}

func TestRelayerMaxBackoffConfig(t *testing.T) {
	s := &mockStorage{}

	// Test with custom max backoff
	config := RelayerConfig{
		PollInterval: 100 * time.Millisecond,
		MaxBackoff:   500 * time.Millisecond,
	}

	r := NewRelayer(s, config)

	// With 100ms poll interval and 5 consecutive empty:
	// backoff = 100ms * 2^5 = 3200ms, but capped at 500ms
	result := r.calculateBackoff(5)
	if result != 500*time.Millisecond {
		t.Errorf("expected backoff capped at 500ms, got %v", result)
	}

	// With 10 consecutive empty (exponent capped at 5):
	// backoff = 100ms * 2^5 = 3200ms, but capped at 500ms
	result = r.calculateBackoff(10)
	if result != 500*time.Millisecond {
		t.Errorf("expected backoff capped at 500ms, got %v", result)
	}
}
