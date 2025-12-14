package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/coordination"
	"github.com/i2y/romancy/internal/notify"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/outbox"
)

// App is the main entry point for Romancy.
// It manages workflow execution, storage, and background tasks.
type App struct {
	config  *appConfig
	storage storage.Storage
	hooks   hooks.WorkflowHooks
	engine  *replay.Engine

	// Outbox relayer (if enabled)
	outboxRelayer *outbox.Relayer

	// PostgreSQL LISTEN/NOTIFY listener (if enabled)
	notifyListener *notify.Listener

	// Registered workflows
	workflows   map[string]any
	workflowsMu sync.RWMutex

	// Workflow runners (workflow name → runner function)
	workflowRunners   map[string]replay.WorkflowRunner
	workflowRunnersMu sync.RWMutex

	// Event handlers (workflow name → event type)
	eventHandlers   map[string]string
	eventHandlersMu sync.RWMutex

	// Background task management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Concurrency control semaphores
	resumptionSem *semaphore.Weighted
	timerSem      *semaphore.Weighted
	messageSem    *semaphore.Weighted

	// Singleton task runners (for distributed coordination)
	staleLockRunner      *coordination.SingletonTaskRunner
	channelCleanupRunner *coordination.SingletonTaskRunner

	// State
	initialized bool
	running     bool
	mu          sync.Mutex
}

// NewApp creates a new Romancy application.
func NewApp(opts ...Option) *App {
	config := defaultConfig()
	for _, opt := range opts {
		opt(config)
	}

	// Generate worker ID if not set
	if config.workerID == "" {
		config.workerID = uuid.New().String()
	}

	return &App{
		config:          config,
		hooks:           config.hooks,
		workflows:       make(map[string]any),
		workflowRunners: make(map[string]replay.WorkflowRunner),
		eventHandlers:   make(map[string]string),
		resumptionSem:   semaphore.NewWeighted(int64(config.maxConcurrentResumptions)),
		timerSem:        semaphore.NewWeighted(int64(config.maxConcurrentTimers)),
		messageSem:      semaphore.NewWeighted(int64(config.maxConcurrentMessages)),
	}
}

// Start initializes the application and starts background tasks.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running {
		return fmt.Errorf("app already running")
	}

	// Create cancellable context
	a.ctx, a.cancel = context.WithCancel(ctx)

	// Initialize storage
	if err := a.initStorage(); err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	// Initialize singleton task runners if enabled
	if a.config.singletonStaleLockCleanup {
		a.staleLockRunner = coordination.NewSingletonTaskRunner(
			a.storage,
			a.config.workerID,
			coordination.DefaultSingletonConfig("stale_lock_cleanup"),
		)
	}
	if a.config.singletonChannelCleanup {
		a.channelCleanupRunner = coordination.NewSingletonTaskRunner(
			a.storage,
			a.config.workerID,
			coordination.DefaultSingletonConfig("channel_cleanup"),
		)
	}

	// Start background tasks
	a.startBackgroundTasks()

	a.running = true
	a.initialized = true
	return nil
}

// Shutdown gracefully shuts down the application.
func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return nil
	}
	a.running = false
	a.mu.Unlock()

	// Stop outbox relayer if enabled
	if a.outboxRelayer != nil {
		a.outboxRelayer.Stop()
	}

	// Stop LISTEN/NOTIFY listener if enabled
	if a.notifyListener != nil {
		if err := a.notifyListener.Stop(ctx); err != nil {
			slog.Debug("error stopping LISTEN/NOTIFY listener", "error", err)
		}
	}

	// Cancel background tasks
	if a.cancel != nil {
		a.cancel()
	}

	// Wait for background tasks with timeout
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All tasks completed
	case <-ctx.Done():
		return fmt.Errorf("shutdown timed out")
	case <-time.After(a.config.shutdownTimeout):
		return fmt.Errorf("shutdown timed out after %v", a.config.shutdownTimeout)
	}

	// Close storage
	if a.storage != nil {
		if err := a.storage.Close(); err != nil {
			return fmt.Errorf("failed to close storage: %w", err)
		}
	}

	return nil
}

// initStorage initializes the storage backend.
func (a *App) initStorage() error {
	if a.config.databaseURL == "" {
		a.config.databaseURL = "romancy.db"
	}

	// Determine storage type based on URL
	url := a.config.databaseURL
	isPostgres := len(url) >= 8 && url[:8] == "postgres"

	switch {
	case isPostgres:
		// PostgreSQL
		pgStorage, err := storage.NewPostgresStorage(url)
		if err != nil {
			return fmt.Errorf("failed to create PostgreSQL storage: %w", err)
		}
		a.storage = pgStorage

		// Configure LISTEN/NOTIFY if enabled
		if a.shouldEnableListenNotify(isPostgres) {
			pgStorage.SetNotifyEnabled(true)
			a.initNotifyListener(url)
		}
	case len(url) >= 5 && url[:5] == "mysql":
		// MySQL
		mysqlStorage, err := storage.NewMySQLStorage(url)
		if err != nil {
			return fmt.Errorf("failed to create MySQL storage: %w", err)
		}
		a.storage = mysqlStorage
	default:
		// Default to SQLite
		sqliteStorage, err := storage.NewSQLiteStorage(url)
		if err != nil {
			return fmt.Errorf("failed to create SQLite storage: %w", err)
		}
		a.storage = sqliteStorage
	}

	// Initialize storage (run migrations) if autoMigrate is enabled
	if a.config.autoMigrate {
		if err := a.storage.Initialize(a.ctx); err != nil {
			return fmt.Errorf("failed to initialize storage: %w", err)
		}
	}

	// Create the replay engine
	a.engine = replay.NewEngine(a.storage, a.hooks, a.config.workerID)

	return nil
}

// shouldEnableListenNotify determines if LISTEN/NOTIFY should be enabled.
// Returns true if:
//   - useListenNotify is explicitly set to true, or
//   - useListenNotify is nil (auto-detect) and isPostgres is true
func (a *App) shouldEnableListenNotify(isPostgres bool) bool {
	if a.config.useListenNotify != nil {
		return *a.config.useListenNotify
	}
	// Auto-detect: enable for PostgreSQL by default
	return isPostgres
}

// initNotifyListener initializes the PostgreSQL LISTEN/NOTIFY listener.
func (a *App) initNotifyListener(connString string) {
	a.notifyListener = notify.NewListener(
		connString,
		notify.WithReconnectDelay(a.config.notifyReconnectDelay),
	)

	// Register notification handlers
	a.notifyListener.OnNotification(notify.ChannelWorkflowResumable, a.handleWorkflowResumableNotify)
	a.notifyListener.OnNotification(notify.ChannelTimerExpired, a.handleTimerExpiredNotify)
	a.notifyListener.OnNotification(notify.ChannelChannelMessage, a.handleChannelMessageNotify)
	a.notifyListener.OnNotification(notify.ChannelOutboxPending, a.handleOutboxPendingNotify)

	slog.Info("PostgreSQL LISTEN/NOTIFY configured",
		"reconnect_delay", a.config.notifyReconnectDelay)
}

// handleWorkflowResumableNotify handles workflow resumable notifications.
func (a *App) handleWorkflowResumableNotify(ctx context.Context, channel notify.NotifyChannel, payload string) {
	n, err := notify.ParseWorkflowNotification(payload)
	if err != nil {
		slog.Debug("failed to parse workflow notification", "payload", payload, "error", err)
		return
	}

	slog.Debug("received workflow resumable notification", "instance_id", n.InstanceID)

	// Trigger immediate workflow resumption
	if err := a.resumptionSem.Acquire(ctx, 1); err != nil {
		return
	}
	go func() {
		defer a.resumptionSem.Release(1)
		if err := a.resumeWorkflow(ctx, n.InstanceID); err != nil {
			slog.Debug("error resuming workflow from notify", "instance_id", n.InstanceID, "error", err)
		}
	}()
}

// handleTimerExpiredNotify handles timer expired notifications.
func (a *App) handleTimerExpiredNotify(ctx context.Context, channel notify.NotifyChannel, payload string) {
	// Timer notifications are informational - the timer check loop will pick them up
	// We just log for debugging
	slog.Debug("received timer notification", "payload", payload)
}

// handleChannelMessageNotify handles channel message notifications.
func (a *App) handleChannelMessageNotify(ctx context.Context, channel notify.NotifyChannel, payload string) {
	n, err := notify.ParseChannelMessageNotification(payload)
	if err != nil {
		slog.Debug("failed to parse channel message notification", "payload", payload, "error", err)
		return
	}

	slog.Debug("received channel message notification",
		"channel_name", n.ChannelName, "message_id", n.MessageID, "target", n.TargetInstanceID)

	// Trigger immediate message delivery
	go a.wakeWaitingChannelSubscribersForNotify(ctx, n.ChannelName, n.TargetInstanceID)
}

// handleOutboxPendingNotify handles outbox pending notifications.
func (a *App) handleOutboxPendingNotify(ctx context.Context, channel notify.NotifyChannel, payload string) {
	// Outbox notifications are informational - the outbox relayer handles polling
	// We could trigger immediate relay here if needed
	slog.Debug("received outbox notification", "payload", payload)
}

// wakeWaitingChannelSubscribersForNotify triggers message delivery for waiting subscribers.
func (a *App) wakeWaitingChannelSubscribersForNotify(ctx context.Context, channelName, targetInstanceID string) {
	if a.storage == nil {
		return
	}

	// If targeted message, only wake the target subscriber
	if targetInstanceID != "" {
		a.deliverToWaitingSubscriber(ctx, targetInstanceID, channelName)
		return
	}

	// Get waiting subscribers for this channel
	subs, err := a.storage.GetChannelSubscribersWaiting(ctx, channelName)
	if err != nil {
		slog.Debug("error getting waiting subscribers", "channel", channelName, "error", err)
		return
	}

	for _, sub := range subs {
		if ctx.Err() != nil {
			break
		}
		a.deliverToWaitingSubscriber(ctx, sub.InstanceID, channelName)
	}
}

// deliverToWaitingSubscriber attempts to deliver messages to a single waiting subscriber.
func (a *App) deliverToWaitingSubscriber(ctx context.Context, instanceID, channelName string) {
	if err := a.messageSem.Acquire(ctx, 1); err != nil {
		return
	}
	go func() {
		defer a.messageSem.Release(1)

		messages, err := a.storage.GetPendingChannelMessagesForInstance(ctx, instanceID, channelName)
		if err != nil || len(messages) == 0 {
			return
		}

		msg := messages[0]
		result, err := a.storage.DeliverChannelMessageWithLock(
			ctx, instanceID, channelName, msg,
			a.config.workerID, int(a.config.staleLockTimeout.Seconds()))
		if err != nil {
			slog.Debug("failed to deliver message from notify", "instance_id", instanceID, "error", err)
			return
		}
		if result != nil {
			slog.Debug("delivered message from notify",
				"instance_id", instanceID, "channel", channelName, "message_id", msg.ID)
		}
	}()
}

// addJitter adds random jitter (±25%) to a duration to prevent thundering herd.
// This helps distribute load when multiple workers start simultaneously.
func addJitter(d time.Duration) time.Duration {
	const jitterPercent = 0.25
	// Generate a factor between (1 - jitterPercent) and (1 + jitterPercent)
	factor := 1.0 + jitterPercent*(2*rand.Float64()-1)
	return time.Duration(float64(d) * factor)
}

// startBackgroundTasks starts all background goroutines.
func (a *App) startBackgroundTasks() {
	// Start PostgreSQL LISTEN/NOTIFY listener if configured
	if a.notifyListener != nil {
		if err := a.notifyListener.Start(a.ctx); err != nil {
			slog.Error("failed to start LISTEN/NOTIFY listener", "error", err)
		}
	}

	// Stale lock cleanup
	a.wg.Add(1)
	go a.runStaleLockCleanup()

	// Timer check
	a.wg.Add(1)
	go a.runTimerCheck()

	// Event timeout check
	a.wg.Add(1)
	go a.runEventTimeoutCheck()

	// Channel timeout check
	a.wg.Add(1)
	go a.runChannelTimeoutCheck()

	// Recurred workflow check
	a.wg.Add(1)
	go a.runRecurCheck()

	// Channel message cleanup
	a.wg.Add(1)
	go a.runChannelCleanup()

	// Workflow resumption (for load balancing / fallback when LISTEN/NOTIFY unavailable)
	a.wg.Add(1)
	go a.runWorkflowResumption()

	// Outbox relayer (if enabled)
	if a.config.outboxEnabled {
		a.outboxRelayer = outbox.NewRelayer(a.storage, outbox.RelayerConfig{
			TargetURL:    a.config.brokerURL,
			PollInterval: a.config.outboxInterval,
			BatchSize:    a.config.outboxBatchSize,
		})
		a.outboxRelayer.Start(a.ctx)
	}
}

// runStaleLockCleanup periodically cleans up stale locks.
func (a *App) runStaleLockCleanup() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.staleLockInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			var err error
			if a.staleLockRunner != nil {
				// Run as singleton task (only one worker runs at a time)
				_, err = a.staleLockRunner.TryRun(a.ctx, func(ctx context.Context) error {
					return a.cleanupStaleLocks()
				})
			} else {
				// Run directly (all workers run)
				err = a.cleanupStaleLocks()
			}
			if err != nil {
				slog.Error("error cleaning up stale locks", "error", err)
			}
		}
	}
}

// cleanupStaleLocks cleans up stale locks and resumes stuck workflows.
func (a *App) cleanupStaleLocks() error {
	if a.storage == nil {
		return nil
	}

	timeoutSec := int(a.config.staleLockTimeout.Seconds())
	staleWorkflows, err := a.storage.CleanupStaleLocks(a.ctx, timeoutSec)
	if err != nil {
		return err
	}

	// Resume stale workflows
	var wg sync.WaitGroup
	for _, info := range staleWorkflows {
		if a.ctx.Err() != nil {
			break
		}

		if err := a.resumptionSem.Acquire(a.ctx, 1); err != nil {
			break
		}

		wg.Add(1)
		go func(info storage.StaleWorkflowInfo) {
			defer wg.Done()
			defer a.resumptionSem.Release(1)

			if err := a.resumeWorkflow(a.ctx, info.InstanceID); err != nil {
				slog.Error("error resuming workflow", "instance_id", info.InstanceID, "error", err)
			}
		}(info)
	}

	wg.Wait()
	return nil
}

// runTimerCheck periodically checks for expired timers.
func (a *App) runTimerCheck() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.timerCheckInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.checkExpiredTimers(); err != nil {
				slog.Error("error checking expired timers", "error", err)
			}
		}
	}
}

// checkExpiredTimers processes expired timers.
func (a *App) checkExpiredTimers() error {
	if a.storage == nil {
		return nil
	}

	timers, err := a.storage.FindExpiredTimers(a.ctx, a.config.maxTimersPerBatch)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, timer := range timers {
		if a.ctx.Err() != nil {
			break
		}

		if err := a.timerSem.Acquire(a.ctx, 1); err != nil {
			break
		}

		wg.Add(1)
		go func(timer *storage.TimerSubscription) {
			defer wg.Done()
			defer a.timerSem.Release(1)

			if err := a.handleExpiredTimer(timer); err != nil {
				slog.Error("error handling timer", "timer_id", timer.TimerID, "error", err)
			}
		}(timer)
	}

	wg.Wait()
	return nil
}

// handleExpiredTimer handles a single expired timer.
func (a *App) handleExpiredTimer(timer *storage.TimerSubscription) error {
	// Get instance to find workflow name
	instance, err := a.storage.GetInstance(a.ctx, timer.InstanceID)
	if err != nil {
		return err
	}
	if instance == nil {
		// Instance not found, skip (already deleted or invalid)
		return nil
	}

	// Check if we have this workflow registered
	// If not, skip and let another worker handle it
	a.workflowRunnersMu.RLock()
	_, registered := a.workflowRunners[instance.WorkflowName]
	a.workflowRunnersMu.RUnlock()

	if !registered {
		// This worker doesn't have the workflow registered, skip
		slog.Debug("skipping timer for unregistered workflow",
			"instance_id", timer.InstanceID,
			"workflow_name", instance.WorkflowName,
			"timer_id", timer.TimerID)
		return nil
	}

	// Try to acquire lock
	acquired, err := a.storage.TryAcquireLock(
		a.ctx,
		timer.InstanceID,
		a.config.workerID,
		int(a.config.staleLockTimeout.Seconds()),
	)
	if err != nil {
		return err
	}
	if !acquired {
		// Another worker is handling this
		return nil
	}

	// Remove the timer subscription
	if err := a.storage.RemoveTimerSubscription(a.ctx, timer.InstanceID, timer.TimerID); err != nil {
		_ = a.storage.ReleaseLock(a.ctx, timer.InstanceID, a.config.workerID)
		return err
	}

	// Record the timer completion in history so replay can skip it
	timerResult := map[string]any{
		"timer_id":   timer.TimerID,
		"expires_at": timer.ExpiresAt.Format(time.RFC3339),
		"completed":  true,
	}
	if err := a.engine.RecordActivityResult(a.ctx, timer.InstanceID, timer.TimerID, timerResult, nil); err != nil {
		_ = a.storage.ReleaseLock(a.ctx, timer.InstanceID, a.config.workerID)
		return fmt.Errorf("failed to record timer result: %w", err)
	}

	// Call OnTimerFired hook
	a.hooks.OnTimerFired(a.ctx, hooks.TimerFiredInfo{
		InstanceID:   timer.InstanceID,
		WorkflowName: instance.WorkflowName,
		TimerID:      timer.TimerID,
		FiredAt:      time.Now(),
	})

	// Resume workflow with lock already held
	return a.resumeWorkflowWithLock(a.ctx, timer.InstanceID)
}

// runEventTimeoutCheck periodically checks for event timeouts.
func (a *App) runEventTimeoutCheck() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.eventTimeoutInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.checkEventTimeouts(); err != nil {
				slog.Error("error checking event timeouts", "error", err)
			}
		}
	}
}

// checkEventTimeouts is deprecated - event timeouts are now handled through
// the channel subscription system via checkChannelTimeouts.
// This function now delegates to checkChannelTimeouts for backward compatibility.
func (a *App) checkEventTimeouts() error {
	// Events are now delivered via the channel system.
	// Timeouts are handled by checkChannelTimeouts.
	return a.checkChannelTimeouts()
}

// registerWorkflowWithRunner registers a workflow with its runner function.
// The runner bridges the generic workflow to the replay engine.
func (a *App) registerWorkflowWithRunner(name string, workflow any, runner replay.WorkflowRunner, opts *workflowOptions) {
	a.workflowsMu.Lock()
	a.workflows[name] = workflow
	a.workflowsMu.Unlock()

	a.workflowRunnersMu.Lock()
	a.workflowRunners[name] = runner
	a.workflowRunnersMu.Unlock()

	if opts != nil && opts.eventHandler {
		a.eventHandlersMu.Lock()
		a.eventHandlers[name] = name // Event type matches workflow name by default
		a.eventHandlersMu.Unlock()
	}
}

// startWorkflow starts a new workflow instance.
func (a *App) startWorkflow(ctx context.Context, workflowName string, input any, opts *startOptions) (string, error) {
	// Generate instance ID if not provided
	instanceID := opts.instanceID
	if instanceID == "" {
		instanceID = uuid.New().String()
	}

	// Serialize input
	inputData, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("failed to serialize input: %w", err)
	}

	// Create instance in storage
	now := time.Now()
	instance := &storage.WorkflowInstance{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		Status:       storage.StatusPending,
		InputData:    inputData,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if a.storage != nil {
		if err := a.storage.CreateInstance(ctx, instance); err != nil {
			return "", fmt.Errorf("failed to create instance: %w", err)
		}
	}

	// Call hook
	a.hooks.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		InputData:    input,
		StartTime:    now,
	})

	// Start workflow execution asynchronously
	go func() {
		if err := a.runWorkflow(context.Background(), instanceID, workflowName); err != nil {
			slog.Error("workflow failed", "instance_id", instanceID, "error", err)
		}
	}()

	return instanceID, nil
}

// runWorkflow executes a workflow instance.
func (a *App) runWorkflow(ctx context.Context, instanceID, workflowName string) error {
	// Get the runner for this workflow
	a.workflowRunnersMu.RLock()
	runner, ok := a.workflowRunners[workflowName]
	a.workflowRunnersMu.RUnlock()

	if !ok {
		return fmt.Errorf("workflow %s not registered", workflowName)
	}

	return a.engine.StartWorkflow(ctx, instanceID, workflowName, nil, runner)
}

// resumeWorkflow resumes a workflow (for stale lock recovery).
func (a *App) resumeWorkflow(ctx context.Context, instanceID string) error {
	// Get the instance to find the workflow name
	instance, err := a.storage.GetInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}
	if instance == nil {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	// Get the runner for this workflow
	a.workflowRunnersMu.RLock()
	runner, ok := a.workflowRunners[instance.WorkflowName]
	a.workflowRunnersMu.RUnlock()

	if !ok {
		return fmt.Errorf("workflow %s not registered", instance.WorkflowName)
	}

	return a.engine.ResumeWorkflow(ctx, instanceID, runner)
}

// resumeWorkflowWithLock resumes a workflow with lock already held.
func (a *App) resumeWorkflowWithLock(ctx context.Context, instanceID string) error {
	// For now, just resume normally - the lock handling is in the engine
	return a.resumeWorkflow(ctx, instanceID)
}

// GetInstance retrieves a workflow instance by ID.
func (a *App) GetInstance(ctx context.Context, instanceID string) (*storage.WorkflowInstance, error) {
	if a.storage == nil {
		return nil, fmt.Errorf("storage not initialized")
	}

	instance, err := a.storage.GetInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, &WorkflowNotFoundError{InstanceID: instanceID}
	}

	return instance, nil
}

// FindInstances searches for workflow instances with input filters.
// This is a convenience method for searching workflows by their input data.
//
// The inputFilters parameter maps JSON paths to expected values:
//   - "customer.id" -> "12345" matches input like {"customer": {"id": "12345"}}
//   - "status" -> "active" matches input like {"status": "active"}
//
// Values are compared with exact match. Supported value types:
//   - string: Exact string match
//   - int/float64: Numeric comparison
//   - bool: Boolean comparison
//   - nil: Matches null values
//
// Example:
//
//	instances, err := app.FindInstances(ctx, map[string]any{
//	    "order.customer_id": "cust_123",
//	    "order.status": "pending",
//	})
func (a *App) FindInstances(ctx context.Context, inputFilters map[string]any) ([]*storage.WorkflowInstance, error) {
	return a.FindInstancesWithOptions(ctx, storage.ListInstancesOptions{
		InputFilters: inputFilters,
	})
}

// FindInstancesWithOptions searches for workflow instances with full options.
// Use this when you need pagination, status filters, or other advanced options
// in addition to input filters.
func (a *App) FindInstancesWithOptions(ctx context.Context, opts storage.ListInstancesOptions) ([]*storage.WorkflowInstance, error) {
	if a.storage == nil {
		return nil, fmt.Errorf("storage not initialized")
	}

	result, err := a.storage.ListInstances(ctx, opts)
	if err != nil {
		return nil, err
	}

	return result.Instances, nil
}

// cancelWorkflow cancels a running workflow and executes compensations.
func (a *App) cancelWorkflow(ctx context.Context, instanceID, reason string) error {
	if a.storage == nil {
		return fmt.Errorf("storage not initialized")
	}

	// Get the instance to check its status
	instance, err := a.storage.GetInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}
	if instance == nil {
		return &WorkflowNotFoundError{InstanceID: instanceID}
	}

	// Check if the workflow can be cancelled
	switch instance.Status {
	case storage.StatusCompleted:
		return fmt.Errorf("cannot cancel completed workflow")
	case storage.StatusFailed:
		return fmt.Errorf("cannot cancel failed workflow")
	case storage.StatusCancelled:
		return nil // Already cancelled, idempotent
	}

	// Try to acquire lock
	acquired, err := a.storage.TryAcquireLock(
		ctx,
		instanceID,
		a.config.workerID,
		int(a.config.staleLockTimeout.Seconds()),
	)
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("workflow is locked by another worker")
	}
	defer func() { _ = a.storage.ReleaseLock(ctx, instanceID, a.config.workerID) }()

	// Note: Event subscription cleanup is handled by CancelInstance

	// Execute compensations in reverse order (LIFO)
	if err := a.executeCompensations(ctx, instanceID); err != nil {
		// Log compensation errors but continue with cancellation
		slog.Error("error executing compensations", "instance_id", instanceID, "error", err)
	}

	// Mark the workflow as cancelled
	if err := a.storage.CancelInstance(ctx, instanceID, reason); err != nil {
		// Translate internal storage error to public API error
		if errors.Is(err, storage.ErrWorkflowNotCancellable) {
			return ErrWorkflowNotCancellable
		}
		return fmt.Errorf("failed to cancel instance: %w", err)
	}

	// Call hook
	a.hooks.OnWorkflowCancelled(ctx, hooks.WorkflowCancelledInfo{
		InstanceID:   instanceID,
		WorkflowName: instance.WorkflowName,
		Reason:       reason,
		Duration:     time.Since(instance.CreatedAt),
	})

	return nil
}

// executeCompensations executes all pending compensations for a workflow in LIFO order.
func (a *App) executeCompensations(ctx context.Context, instanceID string) error {
	// Get pending compensations (ordered by Order DESC - LIFO)
	compensations, err := a.storage.GetCompensations(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get compensations: %w", err)
	}

	if len(compensations) == 0 {
		return nil
	}

	var lastErr error
	for _, comp := range compensations {
		if comp.Status != "pending" {
			continue // Skip already executed compensations
		}

		// Get the compensation executor
		executor, ok := GetCompensationExecutor(comp.CompensationFn)
		if !ok {
			slog.Warn("compensation executor not found", "activity", comp.CompensationFn)
			_ = a.storage.MarkCompensationFailed(ctx, comp.ID)
			continue
		}

		// Execute the compensation
		if err := executor(ctx, comp.CompensationArg); err != nil {
			slog.Error("compensation failed", "activity", comp.CompensationFn, "error", err)
			_ = a.storage.MarkCompensationFailed(ctx, comp.ID)
			lastErr = err
			continue
		}

		// Mark as executed
		if err := a.storage.MarkCompensationExecuted(ctx, comp.ID); err != nil {
			slog.Error("failed to mark compensation as executed", "error", err)
		}
	}

	return lastErr
}

// Handler returns an http.Handler for CloudEvents and health endpoints.
// This allows integration with existing HTTP routers (gin, echo, chi, etc.).
//
// Example with chi:
//
//	r := chi.NewRouter()
//	r.Mount("/romancy", app.Handler())
//	http.ListenAndServe(":8080", r)
//
// Example with standard http.ServeMux:
//
//	mux := http.NewServeMux()
//	mux.Handle("/api/workflows/", app.Handler())
//	http.ListenAndServe(":8080", mux)
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleCloudEvent)
	mux.HandleFunc("/health/live", a.handleLivenessProbe)
	mux.HandleFunc("/health/ready", a.handleReadinessProbe)
	mux.HandleFunc("/cancel/", a.handleCancel)
	return mux
}

// ListenAndServe starts the HTTP server for CloudEvents.
// For integration with existing HTTP routers, use Handler() instead.
func (a *App) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, a.Handler())
}

// CloudEvent represents a CloudEvents v1.0 event.
type CloudEvent struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Source      string          `json:"source"`
	SpecVersion string          `json:"specversion"`
	Time        *time.Time      `json:"time,omitempty"`
	DataSchema  string          `json:"dataschema,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Extensions  map[string]any  `json:"-"` // CloudEvents extension attributes
}

// UnmarshalJSON implements custom JSON unmarshaling to capture extension attributes.
func (e *CloudEvent) UnmarshalJSON(data []byte) error {
	// First, unmarshal into a map to get all fields
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Known standard fields
	standardFields := map[string]bool{
		"id": true, "type": true, "source": true, "specversion": true,
		"time": true, "dataschema": true, "subject": true, "data": true,
		"datacontenttype": true,
	}

	// Unmarshal standard fields
	if v, ok := raw["id"]; ok {
		_ = json.Unmarshal(v, &e.ID)
	}
	if v, ok := raw["type"]; ok {
		_ = json.Unmarshal(v, &e.Type)
	}
	if v, ok := raw["source"]; ok {
		_ = json.Unmarshal(v, &e.Source)
	}
	if v, ok := raw["specversion"]; ok {
		_ = json.Unmarshal(v, &e.SpecVersion)
	}
	if v, ok := raw["time"]; ok {
		_ = json.Unmarshal(v, &e.Time)
	}
	if v, ok := raw["dataschema"]; ok {
		_ = json.Unmarshal(v, &e.DataSchema)
	}
	if v, ok := raw["subject"]; ok {
		_ = json.Unmarshal(v, &e.Subject)
	}
	if v, ok := raw["data"]; ok {
		e.Data = v
	}

	// Collect extension attributes (non-standard fields)
	e.Extensions = make(map[string]any)
	for key, val := range raw {
		if !standardFields[key] {
			var extVal any
			if err := json.Unmarshal(val, &extVal); err == nil {
				e.Extensions[key] = extVal
			}
		}
	}

	return nil
}

// handleCloudEvent handles incoming CloudEvents.
func (a *App) handleCloudEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Parse CloudEvent from request body
	var event CloudEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":      err.Error(),
			"error_type": "ParseError",
			"retryable":  "false",
		})
		return
	}

	// Validate required fields
	if event.Type == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":      "missing required field: type",
			"error_type": "ValidationError",
			"retryable":  "false",
		})
		return
	}

	// Process the event asynchronously
	// Use background context since HTTP request context is canceled after response
	go a.processCloudEvent(context.Background(), &event)

	// Return 202 Accepted (CloudEvents HTTP Protocol Binding)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// processCloudEvent processes an incoming CloudEvent.
func (a *App) processCloudEvent(ctx context.Context, event *CloudEvent) {
	// Call OnEventReceived hook
	a.hooks.OnEventReceived(ctx, hooks.EventReceivedInfo{
		EventType:   event.Type,
		EventID:     event.ID,
		EventSource: event.Source,
		InstanceID:  "", // Not known at this point
	})

	// First, try to deliver to waiting workflows
	if err := a.deliverEventToWaitingWorkflows(ctx, event); err != nil {
		slog.Error("error delivering event to waiting workflows", "error", err)
	}

	// Then, check if this event type triggers a new workflow
	a.eventHandlersMu.RLock()
	workflowName, hasHandler := a.eventHandlers[event.Type]
	a.eventHandlersMu.RUnlock()

	if hasHandler {
		// Start a new workflow with the event data as input
		var inputData any
		if event.Data != nil {
			_ = json.Unmarshal(event.Data, &inputData)
		}

		_, err := a.startWorkflow(ctx, workflowName, inputData, &startOptions{})
		if err != nil {
			slog.Error("error starting workflow from event", "workflow", workflowName, "error", err)
		}
	}
}

// deliverEventToWaitingWorkflows delivers an event to workflows waiting for it.
// Uses the unified channel system: event_type is used as the channel name.
// Workflows waiting via WaitEvent (which internally uses Receive) are subscribed to the event_type channel.
func (a *App) deliverEventToWaitingWorkflows(ctx context.Context, event *CloudEvent) error {
	if a.storage == nil {
		return nil
	}

	// Prepare CloudEvent data in ReceivedEvent format
	eventData := map[string]any{
		"id":          event.ID,
		"type":        event.Type,
		"source":      event.Source,
		"specversion": event.SpecVersion,
	}
	if event.Time != nil {
		eventData["time"] = event.Time
	}
	if event.Data != nil {
		var data any
		if err := json.Unmarshal(event.Data, &data); err == nil {
			eventData["data"] = data
		}
	}
	if len(event.Extensions) > 0 {
		eventData["extensions"] = event.Extensions
	}

	dataJSON, err := json.Marshal(eventData)
	if err != nil {
		return fmt.Errorf("failed to serialize event data: %w", err)
	}

	// Check if a specific instance is targeted via romancyinstanceid extension
	targetInstanceID := ""
	if id, ok := event.Extensions["romancyinstanceid"].(string); ok && id != "" {
		targetInstanceID = id
	}

	// Publish to channel using event.Type as channel name
	messageID, err := a.storage.PublishToChannel(ctx, event.Type, dataJSON, nil, targetInstanceID)
	if err != nil {
		return fmt.Errorf("failed to publish event to channel: %w", err)
	}

	// Wake up waiting subscribers using the unified channel system
	// This uses DeliverChannelMessageWithLock which handles locking and history recording
	a.wakeWaitingChannelSubscribers(ctx, event.Type, messageID, dataJSON, nil, targetInstanceID)

	return nil
}

// wakeWaitingChannelSubscribers delivers a message to waiting channel subscribers.
// This is the App-level version of wakeWaitingSubscribers from channels.go.
func (a *App) wakeWaitingChannelSubscribers(
	ctx context.Context,
	channelName string,
	messageID int64,
	dataJSON []byte,
	metadataJSON []byte,
	targetInstanceID string,
) {
	// Find waiting subscribers
	waitingSubs, err := a.storage.GetChannelSubscribersWaiting(ctx, channelName)
	if err != nil {
		slog.Error("error getting waiting subscribers", "error", err)
		return
	}

	if len(waitingSubs) == 0 {
		return
	}

	// Create message for delivery
	message := &storage.ChannelMessage{
		ID:          messageID,
		ChannelName: channelName,
		DataJSON:    dataJSON,
		Metadata:    metadataJSON,
	}

	lockTimeoutSec := int(a.config.staleLockTimeout.Seconds())

	for _, sub := range waitingSubs {
		// If targetInstanceID is specified, only deliver to that instance
		if targetInstanceID != "" && sub.InstanceID != targetInstanceID {
			continue
		}

		// Deliver using Lock-First pattern
		result, err := a.storage.DeliverChannelMessageWithLock(
			ctx,
			sub.InstanceID,
			channelName,
			message,
			a.config.workerID,
			lockTimeoutSec,
		)
		if err != nil {
			slog.Error("error delivering message", "instance_id", sub.InstanceID, "error", err)
			continue
		}

		// For competing mode, stop after first successful delivery
		if result != nil && sub.Mode == storage.ChannelModeCompeting {
			break
		}
	}
}

// handleLivenessProbe handles the liveness probe.
func (a *App) handleLivenessProbe(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleReadinessProbe handles the readiness probe.
func (a *App) handleReadinessProbe(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	ready := a.running && a.initialized
	a.mu.Unlock()

	if ready {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Not Ready"))
	}
}

// handleCancel handles workflow cancellation requests.
func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Extract instance ID from path
	instanceID := r.URL.Path[len("/cancel/"):]
	if instanceID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing instance ID"})
		return
	}

	// Parse reason from body
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if err := a.cancelWorkflow(r.Context(), instanceID, body.Reason); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}

// Storage returns the storage instance for advanced usage.
func (a *App) Storage() storage.Storage {
	return a.storage
}

// WorkerID returns the worker ID.
func (a *App) WorkerID() string {
	return a.config.workerID
}

// LLMDefaults returns the LLM defaults configured for this app.
// Returns nil if no defaults are set.
func (a *App) LLMDefaults() []any {
	return a.config.llmDefaults
}

// ========================================
// Channel Message Handling
// ========================================

// runChannelTimeoutCheck periodically checks for channel subscription timeouts.
func (a *App) runChannelTimeoutCheck() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.messageCheckInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.checkChannelTimeouts(); err != nil {
				slog.Error("error checking channel timeouts", "error", err)
			}
		}
	}
}

// checkChannelTimeouts handles expired channel subscriptions.
func (a *App) checkChannelTimeouts() error {
	if a.storage == nil {
		return nil
	}

	// Check for expired channel subscriptions
	expiredSubs, err := a.storage.FindExpiredChannelSubscriptions(a.ctx, a.config.maxMessagesPerBatch)
	if err != nil {
		return fmt.Errorf("failed to find expired channel subscriptions: %w", err)
	}

	var wg sync.WaitGroup
	for _, sub := range expiredSubs {
		if a.ctx.Err() != nil {
			break
		}

		if err := a.messageSem.Acquire(a.ctx, 1); err != nil {
			break
		}

		wg.Add(1)
		go func(sub *storage.ChannelSubscription) {
			defer wg.Done()
			defer a.messageSem.Release(1)

			if err := a.handleChannelTimeout(sub); err != nil {
				slog.Error("error handling channel timeout", "instance_id", sub.InstanceID, "error", err)
			}
		}(sub)
	}

	wg.Wait()
	return nil
}

// handleChannelTimeout handles a timed-out channel subscription.
// Instead of failing the workflow directly, we record the timeout error in history
// and set the workflow to 'running' so it can be resumed and handle the error gracefully.
func (a *App) handleChannelTimeout(sub *storage.ChannelSubscription) error {
	// Try to acquire lock
	acquired, err := a.storage.TryAcquireLock(
		a.ctx,
		sub.InstanceID,
		a.config.workerID,
		int(a.config.staleLockTimeout.Seconds()),
	)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}

	// Call OnEventTimeout hook
	instance, _ := a.storage.GetInstance(a.ctx, sub.InstanceID)
	workflowName := ""
	if instance != nil {
		workflowName = instance.WorkflowName
	}
	a.hooks.OnEventTimeout(a.ctx, hooks.EventTimeoutInfo{
		InstanceID:   sub.InstanceID,
		WorkflowName: workflowName,
		EventType:    sub.ChannelName,
	})

	// Record timeout error in history using the activity_id from the subscription
	// This allows Receive to return the timeout error when the workflow resumes
	activityID := sub.ActivityID
	if activityID == "" {
		activityID = fmt.Sprintf("receive_%s:timeout", sub.ChannelName)
	}

	timeoutErr := &ChannelMessageTimeoutError{
		InstanceID:  sub.InstanceID,
		ChannelName: sub.ChannelName,
	}
	errJSON, _ := json.Marshal(timeoutErr.Error())

	historyEvent := &storage.HistoryEvent{
		InstanceID: sub.InstanceID,
		ActivityID: activityID,
		EventType:  storage.HistoryActivityFailed,
		EventData:  errJSON,
		DataType:   "json",
	}
	if err := a.storage.AppendHistory(a.ctx, historyEvent); err != nil {
		_ = a.storage.ReleaseLock(a.ctx, sub.InstanceID, a.config.workerID)
		return fmt.Errorf("failed to record timeout error: %w", err)
	}

	// Clear waiting state
	if err := a.storage.ClearChannelWaitingState(a.ctx, sub.InstanceID, sub.ChannelName); err != nil {
		_ = a.storage.ReleaseLock(a.ctx, sub.InstanceID, a.config.workerID)
		return err
	}

	// Update workflow status to running (so it will be resumed)
	err = a.storage.UpdateInstanceStatus(
		a.ctx,
		sub.InstanceID,
		storage.StatusRunning,
		"",
	)

	_ = a.storage.ReleaseLock(a.ctx, sub.InstanceID, a.config.workerID)

	return err
}

// ========================================
// Recur Handling
// ========================================

// runRecurCheck periodically checks for recurred workflows that need to be restarted.
func (a *App) runRecurCheck() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.recurCheckInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.checkRecurredWorkflows(); err != nil {
				slog.Error("error checking recurred workflows", "error", err)
			}
		}
	}
}

// checkRecurredWorkflows restarts workflows that have been marked as recurred.
func (a *App) checkRecurredWorkflows() error {
	if a.storage == nil {
		return nil
	}

	// Find recurred workflows
	opts := storage.ListInstancesOptions{
		StatusFilter: storage.StatusRecurred,
		Limit:        50,
	}
	result, err := a.storage.ListInstances(a.ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to find recurred workflows: %w", err)
	}

	var wg sync.WaitGroup
	for _, instance := range result.Instances {
		if a.ctx.Err() != nil {
			break
		}

		if err := a.resumptionSem.Acquire(a.ctx, 1); err != nil {
			break
		}

		wg.Add(1)
		go func(instance *storage.WorkflowInstance) {
			defer wg.Done()
			defer a.resumptionSem.Release(1)

			if err := a.handleRecurredWorkflow(instance); err != nil {
				slog.Error("error handling recurred workflow", "instance_id", instance.InstanceID, "error", err)
			}
		}(instance)
	}

	wg.Wait()
	return nil
}

// handleRecurredWorkflow restarts a recurred workflow.
func (a *App) handleRecurredWorkflow(instance *storage.WorkflowInstance) error {
	// Try to acquire lock
	acquired, err := a.storage.TryAcquireLock(
		a.ctx,
		instance.InstanceID,
		a.config.workerID,
		int(a.config.staleLockTimeout.Seconds()),
	)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}

	// Update status back to running
	if err := a.storage.UpdateInstanceStatus(a.ctx, instance.InstanceID, storage.StatusRunning, ""); err != nil {
		_ = a.storage.ReleaseLock(a.ctx, instance.InstanceID, a.config.workerID)
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Resume the workflow (it will replay from the recur event and continue)
	_ = a.storage.ReleaseLock(a.ctx, instance.InstanceID, a.config.workerID)
	return a.resumeWorkflow(a.ctx, instance.InstanceID)
}

// ========================================
// Workflow Resumption (Load Balancing)
// ========================================

// runWorkflowResumption periodically checks for workflows ready to be resumed.
// This task finds workflows with status='running' that don't have an active lock
// (e.g., after message delivery) and resumes them. This is essential for load
// balancing in multi-worker environments - any worker can pick up and resume
// the workflow, not just the one that delivered the message.
func (a *App) runWorkflowResumption() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.workflowResumptionInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.resumeResumableWorkflows(); err != nil {
				slog.Error("error resuming workflows", "error", err)
			}
		}
	}
}

// resumeResumableWorkflows finds and resumes workflows that are ready.
func (a *App) resumeResumableWorkflows() error {
	if a.storage == nil {
		return nil
	}

	// Find resumable workflows (status='running', no lock)
	workflows, err := a.storage.FindResumableWorkflows(a.ctx, a.config.maxWorkflowsPerBatch)
	if err != nil {
		return fmt.Errorf("failed to find resumable workflows: %w", err)
	}

	var wg sync.WaitGroup
	for _, wf := range workflows {
		// Check context before acquiring semaphore
		if a.ctx.Err() != nil {
			break
		}

		// Acquire semaphore (blocks if at capacity)
		if err := a.resumptionSem.Acquire(a.ctx, 1); err != nil {
			// Context cancelled
			break
		}

		wg.Add(1)
		go func(wf *storage.ResumableWorkflow) {
			defer wg.Done()
			defer a.resumptionSem.Release(1)

			// Error is expected when another worker picks up the workflow first
			_ = a.resumeWorkflow(a.ctx, wf.InstanceID)
		}(wf)
	}

	wg.Wait()
	return nil
}

// ========================================
// Channel Message Cleanup
// ========================================

// runChannelCleanup periodically cleans up old channel messages.
func (a *App) runChannelCleanup() {
	defer a.wg.Done()

	ticker := time.NewTicker(addJitter(a.config.channelCleanupInterval))
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			var err error
			if a.channelCleanupRunner != nil {
				// Run as singleton task (only one worker runs at a time)
				_, err = a.channelCleanupRunner.TryRun(a.ctx, func(ctx context.Context) error {
					return a.cleanupChannelMessages()
				})
			} else {
				// Run directly (all workers run)
				err = a.cleanupChannelMessages()
			}
			if err != nil {
				slog.Error("error cleaning up channel messages", "error", err)
			}
		}
	}
}

// cleanupChannelMessages removes old channel messages.
func (a *App) cleanupChannelMessages() error {
	if a.storage == nil {
		return nil
	}

	if err := a.storage.CleanupOldChannelMessages(a.ctx, a.config.channelMessageRetention); err != nil {
		return fmt.Errorf("failed to cleanup channel messages: %w", err)
	}

	return nil
}
