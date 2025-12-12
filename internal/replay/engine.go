package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
)

// CachedResult holds both raw JSON bytes and the unmarshaled value.
// This avoids re-serialization when the raw JSON is needed.
type CachedResult struct {
	RawJSON []byte // Original JSON bytes from history
	Value   any    // Unmarshaled value
}

// Engine handles workflow execution with deterministic replay.
type Engine struct {
	storage            storage.Storage
	hooks              hooks.WorkflowHooks
	workerID           string
	compensationRunner CompensationRunner
}

// NewEngine creates a new replay engine.
func NewEngine(s storage.Storage, h hooks.WorkflowHooks, workerID string) *Engine {
	return &Engine{
		storage:  s,
		hooks:    h,
		workerID: workerID,
	}
}

// WorkflowRunner is a function that executes the workflow logic.
// It receives the context and returns the result or an error.
type WorkflowRunner func(ctx *ExecutionContext) (any, error)

// ExecutionContext provides context for workflow execution.
type ExecutionContext struct {
	ctx        context.Context
	instanceID string
	engine     *Engine

	// Activity tracking
	activityCounter map[string]int
	counterMu       sync.Mutex

	// Replay state
	history      []*storage.HistoryEvent
	historyIndex int
	isReplaying  bool

	// Cached results from replay
	cachedResults map[string]*CachedResult
	cachedErrors  map[string]error

	// Replay statistics
	cacheHits     int
	newActivities int
}

// Context returns the underlying context.Context.
func (ec *ExecutionContext) Context() context.Context {
	return ec.ctx
}

// InstanceID returns the workflow instance ID.
func (ec *ExecutionContext) InstanceID() string {
	return ec.instanceID
}

// IsReplaying returns true if the workflow is being replayed.
func (ec *ExecutionContext) IsReplaying() bool {
	return ec.isReplaying
}

// GenerateActivityID generates a unique activity ID for the given activity name.
func (ec *ExecutionContext) GenerateActivityID(activityName string) string {
	ec.counterMu.Lock()
	defer ec.counterMu.Unlock()

	ec.activityCounter[activityName]++
	return fmt.Sprintf("%s:%d", activityName, ec.activityCounter[activityName])
}

// GetCachedResult retrieves a cached result from replay history.
// Returns (result, true) if found, (nil, false) if not found.
// The returned value is the unmarshaled any value from CachedResult.Value.
func (ec *ExecutionContext) GetCachedResult(activityID string) (any, bool) {
	if cached, ok := ec.cachedResults[activityID]; ok {
		ec.counterMu.Lock()
		ec.cacheHits++
		ec.counterMu.Unlock()
		return cached.Value, true
	}
	if err, ok := ec.cachedErrors[activityID]; ok {
		ec.counterMu.Lock()
		ec.cacheHits++
		ec.counterMu.Unlock()
		return err, true
	}
	return nil, false
}

// GetCachedResultRaw retrieves a cached result with raw JSON bytes.
// Returns (*CachedResult, true) if found, (nil, false) if not found.
// Use this to avoid re-serialization when the raw JSON is needed.
func (ec *ExecutionContext) GetCachedResultRaw(activityID string) (*CachedResult, bool) {
	if cached, ok := ec.cachedResults[activityID]; ok {
		return cached, true
	}
	return nil, false
}

// SetCachedResult caches a result for replay.
func (ec *ExecutionContext) SetCachedResult(activityID string, result any) {
	if err, ok := result.(error); ok {
		ec.cachedErrors[activityID] = err
	} else {
		ec.cachedResults[activityID] = &CachedResult{Value: result}
	}
}

// SetCachedResultWithRaw caches a result with raw JSON bytes for replay.
func (ec *ExecutionContext) SetCachedResultWithRaw(activityID string, rawJSON []byte, value any) {
	ec.cachedResults[activityID] = &CachedResult{RawJSON: rawJSON, Value: value}
}

// RecordActivityID records that an activity has been executed.
func (ec *ExecutionContext) RecordActivityID(activityID string) {
	// This is called after activity execution to track which activities have run
}

// RecordActivityResult records an activity result to storage.
// This delegates to the engine's RecordActivityResult method.
func (ec *ExecutionContext) RecordActivityResult(activityID string, result any, activityErr error) error {
	ec.counterMu.Lock()
	ec.newActivities++
	ec.counterMu.Unlock()
	return ec.engine.RecordActivityResult(ec.ctx, ec.instanceID, activityID, result, activityErr)
}

// RecordActivityResultWithContext records an activity result to storage using the provided context.
// This is useful when recording within a transaction where the transaction context should be used.
func (ec *ExecutionContext) RecordActivityResultWithContext(ctx context.Context, activityID string, result any, activityErr error) error {
	ec.counterMu.Lock()
	ec.newActivities++
	ec.counterMu.Unlock()
	return ec.engine.RecordActivityResult(ctx, ec.instanceID, activityID, result, activityErr)
}

// Storage returns the storage interface for advanced use cases.
func (ec *ExecutionContext) Storage() storage.Storage {
	return ec.engine.storage
}

// Hooks returns the workflow hooks for observability.
func (ec *ExecutionContext) Hooks() hooks.WorkflowHooks {
	return ec.engine.hooks
}

// StartWorkflow starts a new workflow instance.
func (e *Engine) StartWorkflow(
	ctx context.Context,
	instanceID string,
	workflowName string,
	inputData []byte,
	runner WorkflowRunner,
) error {
	// Try to acquire lock
	acquired, err := e.storage.TryAcquireLock(ctx, instanceID, e.workerID, 300)
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("failed to acquire lock for instance %s", instanceID)
	}
	defer func() { _ = e.storage.ReleaseLock(ctx, instanceID, e.workerID) }()

	// Update status to running
	if err := e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusRunning, ""); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Execute the workflow
	return e.executeWorkflow(ctx, instanceID, runner)
}

// ResumeWorkflow resumes a workflow from its history.
func (e *Engine) ResumeWorkflow(
	ctx context.Context,
	instanceID string,
	runner WorkflowRunner,
) error {
	// Try to acquire lock
	acquired, err := e.storage.TryAcquireLock(ctx, instanceID, e.workerID, 300)
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("failed to acquire lock for instance %s", instanceID)
	}
	defer func() { _ = e.storage.ReleaseLock(ctx, instanceID, e.workerID) }()

	// Update status to running
	if err := e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusRunning, ""); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Execute the workflow
	return e.executeWorkflow(ctx, instanceID, runner)
}

// executeWorkflow executes or replays a workflow.
func (e *Engine) executeWorkflow(ctx context.Context, instanceID string, runner WorkflowRunner) error {
	// Load history using iterator (streams from storage in batches)
	iter := storage.NewHistoryIterator(ctx, e.storage, instanceID, &storage.HistoryIteratorOptions{
		BatchSize: 1000,
	})
	defer func() { _ = iter.Close() }()

	// Build cached results from history
	cachedResults := make(map[string]*CachedResult)
	cachedErrors := make(map[string]error)
	historyCount := 0

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		historyCount++

		switch event.EventType {
		case storage.HistoryActivityCompleted:
			var result any
			if event.DataType == "json" && len(event.EventData) > 0 {
				if err := json.Unmarshal(event.EventData, &result); err == nil {
					// Store both raw JSON and unmarshaled value
					cachedResults[event.ActivityID] = &CachedResult{
						RawJSON: event.EventData,
						Value:   result,
					}
				}
			}
		case storage.HistoryActivityFailed:
			errMsg := ""
			if len(event.EventData) > 0 {
				_ = json.Unmarshal(event.EventData, &errMsg)
			}
			cachedErrors[event.ActivityID] = fmt.Errorf("%s", errMsg)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to load history: %w", err)
	}

	// Create execution context
	isReplaying := historyCount > 0
	execCtx := &ExecutionContext{
		ctx:             ctx,
		instanceID:      instanceID,
		engine:          e,
		activityCounter: make(map[string]int),
		history:         nil, // History is processed via iterator, not stored
		historyIndex:    0,
		isReplaying:     isReplaying,
		cachedResults:   cachedResults,
		cachedErrors:    cachedErrors,
	}

	startTime := time.Now()

	// Call OnReplayStart hook if replaying
	if isReplaying {
		e.hooks.OnReplayStart(ctx, hooks.ReplayStartInfo{
			InstanceID:     instanceID,
			WorkflowName:   "",
			HistoryEvents:  historyCount,
			ResumeActivity: "", // Not tracked in current implementation
		})
	}

	// Call the workflow start hook
	e.hooks.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{
		InstanceID:   instanceID,
		WorkflowName: "",
		StartTime:    startTime,
	})

	// Execute the workflow
	result, err := runner(execCtx)

	duration := time.Since(startTime)

	if err != nil {
		// Check for SuspendSignal
		if sig := AsSuspendSignal(err); sig != nil {
			return e.handleSuspendSignal(ctx, instanceID, sig)
		}

		// Regular error - run compensations before marking as failed
		if compErr := e.executeCompensations(ctx, instanceID); compErr != nil {
			// Log compensation error but continue
			e.hooks.OnWorkflowFailed(ctx, hooks.WorkflowFailedInfo{
				InstanceID: instanceID,
				Error:      fmt.Errorf("workflow failed: %w; compensation also failed: %v", err, compErr),
				Duration:   duration,
			})
		}

		// Mark as failed
		_ = e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusFailed, err.Error())

		e.hooks.OnWorkflowFailed(ctx, hooks.WorkflowFailedInfo{
			InstanceID: instanceID,
			Error:      err,
			Duration:   duration,
		})

		return err
	}

	// Success - serialize result and mark as completed
	resultData, err := json.Marshal(result)
	if err != nil {
		_ = e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusFailed, "failed to serialize result")
		return fmt.Errorf("failed to serialize result: %w", err)
	}

	if err := e.storage.UpdateInstanceOutput(ctx, instanceID, resultData); err != nil {
		return fmt.Errorf("failed to set result: %w", err)
	}

	if err := e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusCompleted, ""); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Call OnReplayComplete hook if we were replaying
	if isReplaying {
		e.hooks.OnReplayComplete(ctx, hooks.ReplayCompleteInfo{
			InstanceID:    instanceID,
			WorkflowName:  "",
			CacheHits:     execCtx.cacheHits,
			NewActivities: execCtx.newActivities,
			Duration:      duration,
		})
	}

	e.hooks.OnWorkflowComplete(ctx, hooks.WorkflowCompleteInfo{
		InstanceID: instanceID,
		OutputData: result,
		Duration:   duration,
	})

	return nil
}

// handleSuspendSignal handles all workflow suspension cases.
func (e *Engine) handleSuspendSignal(ctx context.Context, instanceID string, sig *SuspendSignal) error {
	switch sig.Type {
	case SuspendForTimer:
		return e.handleTimerSuspend(ctx, instanceID, sig)
	case SuspendForChannelMessage:
		return e.handleChannelMessageSuspend(ctx, instanceID, sig)
	case SuspendForRecur:
		return e.handleRecurSuspend(ctx, instanceID, sig)
	default:
		return fmt.Errorf("unknown suspend type: %v", sig.Type)
	}
}

// handleTimerSuspend handles the timer suspension case.
func (e *Engine) handleTimerSuspend(ctx context.Context, instanceID string, sig *SuspendSignal) error {
	// Register timer subscription
	timer := &storage.TimerSubscription{
		InstanceID: instanceID,
		TimerID:    sig.TimerID,
		ExpiresAt:  sig.ExpiresAt,
		Step:       sig.Step,
	}

	if err := e.storage.RegisterTimerSubscription(ctx, timer); err != nil {
		return fmt.Errorf("failed to register timer subscription: %w", err)
	}

	// Update status to waiting
	if err := e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusWaitingForTimer, ""); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	return nil
}

// handleChannelMessageSuspend handles the channel message suspension case (via Receive or WaitEvent).
func (e *Engine) handleChannelMessageSuspend(ctx context.Context, instanceID string, sig *SuspendSignal) error {
	// Register channel receive and set waiting=TRUE
	var timeoutAt *time.Time
	if sig.Timeout != nil {
		t := time.Now().Add(*sig.Timeout)
		timeoutAt = &t
	}
	if err := e.storage.RegisterChannelReceiveAndReleaseLock(ctx, instanceID, sig.Channel, e.workerID, sig.ActivityID, timeoutAt); err != nil {
		return fmt.Errorf("failed to register channel receive: %w", err)
	}

	return nil
}

// handleRecurSuspend handles the Recur case (Erlang-style tail recursion).
func (e *Engine) handleRecurSuspend(ctx context.Context, instanceID string, sig *SuspendSignal) error {
	// 1. Archive current history
	if err := e.storage.ArchiveHistory(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to archive history: %w", err)
	}

	// 2. Clean up all subscriptions
	if err := e.storage.CleanupInstanceSubscriptions(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to cleanup subscriptions: %w", err)
	}

	// 3. Leave all groups
	if err := e.storage.LeaveAllGroups(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to leave groups: %w", err)
	}

	// 4. Serialize new input
	inputData, err := json.Marshal(sig.NewInput)
	if err != nil {
		return fmt.Errorf("failed to serialize recur input: %w", err)
	}

	// 5. Record the recur event in history (for the new iteration)
	recurEvent := &storage.HistoryEvent{
		InstanceID: instanceID,
		ActivityID: "recur",
		EventType:  storage.HistoryActivityCompleted,
		EventData:  inputData,
		DataType:   "json",
	}
	if err := e.storage.AppendHistory(ctx, recurEvent); err != nil {
		return fmt.Errorf("failed to record recur event: %w", err)
	}

	// 6. Update status to recurred (will be picked up and restarted)
	if err := e.storage.UpdateInstanceStatus(ctx, instanceID, storage.StatusRecurred, ""); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	return nil
}

// RecordActivityResult records an activity result in history.
func (e *Engine) RecordActivityResult(
	ctx context.Context,
	instanceID string,
	activityID string,
	result any,
	activityErr error,
) error {
	event := &storage.HistoryEvent{
		InstanceID: instanceID,
		ActivityID: activityID,
		DataType:   "json",
	}

	if activityErr != nil {
		event.EventType = storage.HistoryActivityFailed
		errData, _ := json.Marshal(activityErr.Error())
		event.EventData = errData
	} else {
		event.EventType = storage.HistoryActivityCompleted
		resultData, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("failed to serialize result: %w", err)
		}
		event.EventData = resultData
	}

	return e.storage.AppendHistory(ctx, event)
}

// CompensationRunner is a function that executes a compensation.
// It receives the function name and JSON-encoded argument.
type CompensationRunner func(ctx context.Context, funcName string, arg []byte) error

// SetCompensationRunner sets the compensation runner for the engine.
func (e *Engine) SetCompensationRunner(runner CompensationRunner) {
	e.compensationRunner = runner
}

// executeCompensations runs all pending compensations for a workflow in LIFO order.
func (e *Engine) executeCompensations(ctx context.Context, instanceID string) error {
	entries, err := e.storage.GetCompensations(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get compensations: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	if e.compensationRunner == nil {
		// No runner configured, skip compensation
		return nil
	}

	var firstErr error
	for _, entry := range entries {
		if entry.Status != "pending" {
			continue
		}

		execErr := e.compensationRunner(ctx, entry.CompensationFn, entry.CompensationArg)
		if execErr != nil {
			// Mark as failed
			_ = e.storage.MarkCompensationFailed(ctx, entry.ID)
			if firstErr == nil {
				firstErr = fmt.Errorf("compensation %s failed: %w", entry.ActivityID, execErr)
			}
			// Continue with remaining compensations
			continue
		}

		// Mark as executed
		_ = e.storage.MarkCompensationExecuted(ctx, entry.ID)
	}

	return firstErr
}
