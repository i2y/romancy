package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
)

// LockLeaseSeconds is how long a worker's lock on an instance lasts (the
// stale-lock timeout); lockRefreshEvery is how often a running execution
// extends it. The refresh must come often enough that a run pausing between
// two refreshes still loses its lock before the lease runs out and another
// worker takes over.
const (
	LockLeaseSeconds = 300
)

// lockRefreshEvery is how often a running execution extends its lease.
// A var so tests can shorten it; production code should treat it as fixed.
var lockRefreshEvery = 60 * time.Second

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
	lockID     string

	// Activity tracking
	activityCounter map[string]int
	counterMu       sync.Mutex

	// Replay state
	history      []*storage.HistoryEvent
	historyIndex int
	isReplaying  bool

	// Cached results from replay. cacheMu guards both maps: a workflow may
	// run activities from several goroutines.
	cacheMu       sync.RWMutex
	cachedResults map[string]*CachedResult
	cachedErrors  map[string]error

	// Replay statistics
	cacheHits     int
	newActivities int

	// abortErr is set when the run stops without an outcome (a storage
	// failure, a lost lock, shutdown): every later step of the run fails
	// with it, and the engine ends the run leaving the instance as it was,
	// so a later run executes the step again.
	abortMu  sync.Mutex
	abortErr error
}

// ErrRunAborted marks a run that stopped without an outcome. The instance is
// left as it was (running, its lock released) and a later run replays it.
var ErrRunAborted = errors.New("run aborted")

// AbortError is the error every step of an aborted run returns. It matches
// ErrRunAborted and the cause with errors.Is.
type AbortError struct {
	Cause error
}

func (e *AbortError) Error() string { return "run aborted: " + e.Cause.Error() }

// Unwrap exposes both ErrRunAborted and the cause.
func (e *AbortError) Unwrap() []error { return []error{ErrRunAborted, e.Cause} }

// Abort ends the run without an outcome. The first cause is kept; the error
// returned is the one every later step of the run gets.
func (ec *ExecutionContext) Abort(cause error) error {
	ec.abortMu.Lock()
	defer ec.abortMu.Unlock()
	if ec.abortErr == nil {
		var already *AbortError
		if errors.As(cause, &already) {
			ec.abortErr = already
		} else {
			ec.abortErr = &AbortError{Cause: cause}
		}
	}
	return ec.abortErr
}

// Aborted returns the abort error of the run, or nil while it may go on.
func (ec *ExecutionContext) Aborted() error {
	ec.abortMu.Lock()
	defer ec.abortMu.Unlock()
	return ec.abortErr
}

// RecordedFailure is an activity failure as history stores it. The engine
// hands it back on replay; the romancy package turns it into the error
// types it describes. History written before this format holds the message
// only (a JSON string), which reads back as a RecordedFailure with just
// Message set.
type RecordedFailure struct {
	ActivityName string `json:"activity_name,omitempty"`
	ErrorType    string `json:"error_type,omitempty"`
	Message      string `json:"error_message"`
	// Code names a kernel sentinel the error matched (see romancy's step
	// error codes).
	Code string `json:"code,omitempty"`
	// Terminal and Inner describe a TerminalError (Inner is its cause).
	Terminal bool   `json:"terminal,omitempty"`
	Inner    string `json:"inner,omitempty"`
	// Attempts and Inner describe a RetryExhaustedError.
	Attempts int `json:"attempts,omitempty"`
}

func (f *RecordedFailure) Error() string { return f.Message }

// FailureRecorder is implemented by errors that choose how they are written
// to history as an activity failure.
type FailureRecorder interface {
	FailureRecord() *RecordedFailure
}

func decodeRecordedFailure(data []byte) *RecordedFailure {
	var msg string
	if err := json.Unmarshal(data, &msg); err == nil {
		return &RecordedFailure{Message: msg}
	}
	var f RecordedFailure
	if err := json.Unmarshal(data, &f); err == nil {
		return &f
	}
	return &RecordedFailure{Message: string(data)}
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
	ec.cacheMu.RLock()
	cached, okResult := ec.cachedResults[activityID]
	cachedErr, okErr := ec.cachedErrors[activityID]
	ec.cacheMu.RUnlock()
	if okResult {
		ec.counterMu.Lock()
		ec.cacheHits++
		ec.counterMu.Unlock()
		return cached.Value, true
	}
	if okErr {
		ec.counterMu.Lock()
		ec.cacheHits++
		ec.counterMu.Unlock()
		return cachedErr, true
	}
	return nil, false
}

// GetCachedResultRaw retrieves a cached result with raw JSON bytes.
// Returns (*CachedResult, true) if found, (nil, false) if not found.
// Use this to avoid re-serialization when the raw JSON is needed.
func (ec *ExecutionContext) GetCachedResultRaw(activityID string) (*CachedResult, bool) {
	ec.cacheMu.RLock()
	defer ec.cacheMu.RUnlock()
	if cached, ok := ec.cachedResults[activityID]; ok {
		return cached, true
	}
	return nil, false
}

// SetCachedResult caches a result for replay.
func (ec *ExecutionContext) SetCachedResult(activityID string, result any) {
	ec.cacheMu.Lock()
	defer ec.cacheMu.Unlock()
	if err, ok := result.(error); ok {
		ec.cachedErrors[activityID] = err
	} else {
		ec.cachedResults[activityID] = &CachedResult{Value: result}
	}
}

// SetCachedResultWithRaw caches a result with raw JSON bytes for replay.
func (ec *ExecutionContext) SetCachedResultWithRaw(activityID string, rawJSON []byte, value any) {
	ec.cacheMu.Lock()
	defer ec.cacheMu.Unlock()
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
	return ec.engine.RecordActivityResult(ec.ctx, ec.instanceID, ec.lockID, activityID, result, activityErr)
}

// RecordActivityResultWithContext records an activity result to storage using the provided context.
// This is useful when recording within a transaction where the transaction context should be used.
func (ec *ExecutionContext) RecordActivityResultWithContext(ctx context.Context, activityID string, result any, activityErr error) error {
	ec.counterMu.Lock()
	ec.newActivities++
	ec.counterMu.Unlock()
	return ec.engine.RecordActivityResult(ctx, ec.instanceID, ec.lockID, activityID, result, activityErr)
}

// LockID returns the lock identity this run holds; every fenced write of the
// run passes it.
func (ec *ExecutionContext) LockID() string {
	return ec.lockID
}

// Storage returns the storage interface for advanced use cases.
func (ec *ExecutionContext) Storage() storage.Storage {
	return ec.engine.storage
}

// Hooks returns the workflow hooks for observability.
func (ec *ExecutionContext) Hooks() hooks.WorkflowHooks {
	return ec.engine.hooks
}

// StartWorkflow starts a new workflow instance. input is the workflow's input,
// reported to the OnWorkflowStart hook (nil when the caller does not have it).
func (e *Engine) StartWorkflow(
	ctx context.Context,
	instanceID string,
	workflowName string,
	input any,
	runner WorkflowRunner,
) error {
	return e.StartWorkflowWithLockID(ctx, instanceID, workflowName, input, runner, "")
}

// StartWorkflowWithLockID is StartWorkflow for a caller that created the
// instance holding its lock under lockID (so no resumption poll could claim
// it first); an empty lockID takes the lock under a new identity.
func (e *Engine) StartWorkflowWithLockID(
	ctx context.Context,
	instanceID string,
	workflowName string,
	input any,
	runner WorkflowRunner,
	lockID string,
) error {
	// Try to acquire lock under a fresh per-run lock identity. Every fenced
	// write for this run passes the same identity, so a write attempted after
	// the lease was lost and re-claimed (even by this same workerID) is
	// rejected by the `locked_by = holder` check. Taking a lock the caller
	// already holds under lockID renews it.
	if lockID == "" {
		lockID = storage.NewLockIdentity(e.workerID)
	}
	acquired, err := e.storage.TryAcquireLock(ctx, instanceID, lockID, LockLeaseSeconds)
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !acquired {
		// An instance created unlocked (inside a caller's transaction) is
		// running, so a resumption poll (of this worker or another) may claim
		// it before this run does, or it is not visible yet (the transaction
		// has not committed; a poll claims it after the commit). Either way
		// another run executes it: this one has nothing to do.
		slog.Debug("instance claimed by another run before its first run", "instance_id", instanceID)
		return nil
	}
	defer func() { _ = e.storage.ReleaseLock(ctx, instanceID, lockID) }()

	// Update status to running. A terminated instance (cancelled while it
	// waited) is not run again.
	if err := e.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusRunning, "", lockID); err != nil {
		if errors.Is(err, storage.ErrInstanceTerminated) {
			return nil
		}
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Execute the workflow
	return e.executeWorkflow(ctx, instanceID, workflowName, input, runner, lockID)
}

// ResumeWorkflow resumes a workflow from its history.
func (e *Engine) ResumeWorkflow(
	ctx context.Context,
	instanceID string,
	runner WorkflowRunner,
) error {
	return e.ResumeWorkflowWithLockID(ctx, instanceID, storage.NewLockIdentity(e.workerID), runner)
}

// ResumeWorkflowWithLockID resumes a workflow under an existing lock
// identity. Callers that batch-claimed instances (ClaimResumableWorkflows)
// pass the identity they claimed with, so the engine's re-acquisition is a
// self-reclaim of the same epoch rather than a competing claim.
func (e *Engine) ResumeWorkflowWithLockID(
	ctx context.Context,
	instanceID string,
	lockID string,
	runner WorkflowRunner,
) error {
	// Try to acquire lock
	acquired, err := e.storage.TryAcquireLock(ctx, instanceID, lockID, LockLeaseSeconds)
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("failed to acquire lock for instance %s", instanceID)
	}
	defer func() { _ = e.storage.ReleaseLock(ctx, instanceID, lockID) }()

	// Update status to running. A terminated instance (cancelled while it
	// waited) is not run again.
	if err := e.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusRunning, "", lockID); err != nil {
		if errors.Is(err, storage.ErrInstanceTerminated) {
			return nil
		}
		return fmt.Errorf("failed to update status: %w", err)
	}

	// The hooks report the workflow name; a resumed run reads it from the
	// instance.
	workflowName := ""
	if instance, err := e.storage.GetInstance(ctx, instanceID); err == nil && instance != nil {
		workflowName = instance.WorkflowName
	}

	// Execute the workflow
	return e.executeWorkflow(ctx, instanceID, workflowName, nil, runner, lockID)
}

// executeWorkflow executes or replays a workflow. Each run calls the
// OnWorkflowStart hook once, then exactly one of OnWorkflowComplete,
// OnWorkflowFailed and, on hooks that implement hooks.WorkflowSuspendHooks,
// OnWorkflowSuspended.
func (e *Engine) executeWorkflow(ctx context.Context, instanceID, workflowName string, input any, runner WorkflowRunner, lockID string) error {
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
		case storage.HistoryActivityCompleted, storage.HistoryChannelMessageReceived:
			// Both activity results and channel messages are cached for replay
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
		case storage.HistoryActivityFailed, storage.HistoryMessageTimeout:
			// Both activity failures and message timeouts are cached as errors
			cachedErrors[event.ActivityID] = decodeRecordedFailure(event.EventData)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to load history: %w", err)
	}

	// The run holds the instance's lock for its whole duration. A refresher
	// extends the lease so a run longer than the timeout is not taken over by
	// another worker; when the refresh answers that the lock is gone (it
	// expired during a long pause and another worker acquired it), the run is
	// cancelled and its writes are fenced out instead of landing over the new
	// owner's.
	runCtx, stopRun := context.WithCancel(ctx)
	var lockLost atomic.Bool
	lockWatch := make(chan struct{})
	go func() {
		defer close(lockWatch)
		ticker := time.NewTicker(lockRefreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := e.storage.RefreshLock(runCtx, instanceID, lockID, LockLeaseSeconds); errors.Is(err, storage.ErrLockNotHeld) {
					lockLost.Store(true)
					stopRun()
					return
				}
			}
		}
	}()
	defer func() { stopRun(); <-lockWatch }()

	// Create execution context
	isReplaying := historyCount > 0
	execCtx := &ExecutionContext{
		ctx:             runCtx,
		instanceID:      instanceID,
		engine:          e,
		lockID:          lockID,
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
			WorkflowName:   workflowName,
			HistoryEvents:  historyCount,
			ResumeActivity: "", // Not tracked in current implementation
		})
	}

	// Call the workflow start hook
	e.hooks.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		InputData:    input,
		StartTime:    startTime,
	})

	// Execute the workflow
	result, err := runner(execCtx)

	duration := time.Since(startTime)

	// A run that does not finish its workflow says so: it waits, recurs or
	// was aborted, and a later run, possibly on another worker, continues.
	finished := false
	suspended := hooks.WorkflowSuspendedInfo{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		Reason:       hooks.SuspendAborted,
		Duration:     duration,
	}
	defer func() {
		if !finished {
			e.notifySuspended(ctx, suspended)
		}
	}()

	// A run that lost its lock stops here: whatever it would record now
	// belongs to the run the new owner is making, or will make.
	if lockLost.Load() {
		suspended.Error = storage.ErrLockNotHeld
		return storage.ErrLockNotHeld
	}

	// A run that stopped without an outcome (a step hit a storage error or
	// the lost lock, or the worker is shutting down) leaves the instance as
	// it was: no compensation, no failure, no completion. This holds even
	// when workflow code swallowed the step's error and returned something
	// else. The lock is released by the caller and a later run replays it.
	if abortErr := execCtx.Aborted(); abortErr != nil {
		suspended.Error = abortErr
		return abortErr
	}
	if err != nil && (errors.Is(err, ErrRunAborted) || errors.Is(err, storage.ErrLockNotHeld) || ctx.Err() != nil) {
		suspended.Error = execCtx.Abort(err)
		return suspended.Error
	}

	if err != nil {
		// Check for SuspendSignal
		if sig := AsSuspendSignal(err); sig != nil {
			suspended.Reason = hooks.SuspendWaiting
			if sig.Type == SuspendForRecur {
				suspended.Reason = hooks.SuspendRecur
			}
			suspended.Error = e.handleSuspendSignal(ctx, instanceID, sig, lockID)
			return suspended.Error
		}

		// Regular error - run compensations before marking as failed. The
		// failure hook reports a compensation failure along with the error.
		failure := err
		if compErr := e.executeCompensations(ctx, instanceID, lockID); compErr != nil {
			failure = fmt.Errorf("workflow failed: %w; compensation also failed: %v", err, compErr)
		}

		// Record workflow failure in history
		eventData, _ := json.Marshal(map[string]any{"error": err.Error()})
		_ = e.storage.AppendHistoryIfLocked(ctx, &storage.HistoryEvent{
			InstanceID: instanceID,
			EventType:  storage.HistoryWorkflowFailed,
			EventData:  eventData,
			DataType:   "json",
		}, lockID)

		// Mark as failed
		_ = e.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusFailed, err.Error(), lockID)

		finished = true
		e.hooks.OnWorkflowFailed(ctx, hooks.WorkflowFailedInfo{
			InstanceID:   instanceID,
			WorkflowName: workflowName,
			Error:        failure,
			Duration:     duration,
		})

		return err
	}

	// Success - serialize result and mark as completed
	resultData, err := json.Marshal(result)
	if err != nil {
		serializeErr := fmt.Errorf("failed to serialize result: %w", err)
		_ = e.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusFailed, "failed to serialize result", lockID)
		finished = true
		e.hooks.OnWorkflowFailed(ctx, hooks.WorkflowFailedInfo{
			InstanceID:   instanceID,
			WorkflowName: workflowName,
			Error:        serializeErr,
			Duration:     duration,
		})
		return serializeErr
	}

	// An instance cancelled during this run stays cancelled: the result is
	// not kept, and the run ends quietly.
	if err := e.storage.UpdateInstanceOutputIfLocked(ctx, instanceID, resultData, lockID); err != nil {
		suspended.Error = err
		if errors.Is(err, storage.ErrInstanceTerminated) {
			return nil
		}
		return fmt.Errorf("failed to set result: %w", err)
	}

	if err := e.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusCompleted, "", lockID); err != nil {
		suspended.Error = err
		if errors.Is(err, storage.ErrInstanceTerminated) {
			return nil
		}
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Call OnReplayComplete hook if we were replaying
	if isReplaying {
		e.hooks.OnReplayComplete(ctx, hooks.ReplayCompleteInfo{
			InstanceID:    instanceID,
			WorkflowName:  workflowName,
			CacheHits:     execCtx.cacheHits,
			NewActivities: execCtx.newActivities,
			Duration:      duration,
		})
	}

	finished = true
	e.hooks.OnWorkflowComplete(ctx, hooks.WorkflowCompleteInfo{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		OutputData:   result,
		Duration:     duration,
	})

	return nil
}

// notifySuspended calls OnWorkflowSuspended on hooks that implement
// hooks.WorkflowSuspendHooks.
func (e *Engine) notifySuspended(ctx context.Context, info hooks.WorkflowSuspendedInfo) {
	if h, ok := e.hooks.(hooks.WorkflowSuspendHooks); ok {
		h.OnWorkflowSuspended(ctx, info)
	}
}

// handleSuspendSignal handles all workflow suspension cases.
func (e *Engine) handleSuspendSignal(ctx context.Context, instanceID string, sig *SuspendSignal, lockID string) error {
	switch sig.Type {
	case SuspendForTimer:
		return e.handleTimerSuspend(ctx, instanceID, sig, lockID)
	case SuspendForChannelMessage:
		return e.handleChannelMessageSuspend(ctx, instanceID, sig, lockID)
	case SuspendForRecur:
		return e.handleRecurSuspend(ctx, instanceID, sig, lockID)
	default:
		return fmt.Errorf("unknown suspend type: %v", sig.Type)
	}
}

// handleTimerSuspend handles the timer suspension case.
func (e *Engine) handleTimerSuspend(ctx context.Context, instanceID string, sig *SuspendSignal, lockID string) error {
	// The timer and the waiting status commit together, after a lock proof:
	// a run that lost its lock leaves no timer behind to fire into the new
	// owner's run, and an instance cancelled during this run gets none.
	return e.fenced(ctx, instanceID, lockID, "timer suspend", func(txCtx context.Context) error {
		timer := &storage.TimerSubscription{
			InstanceID: instanceID,
			TimerID:    sig.TimerID,
			ExpiresAt:  sig.ExpiresAt,
			ActivityID: sig.ActivityID,
		}
		if err := e.storage.RegisterTimerSubscription(txCtx, timer); err != nil {
			return fmt.Errorf("failed to register timer subscription: %w", err)
		}
		if err := e.storage.UpdateInstanceStatusIfLocked(txCtx, instanceID, storage.StatusWaitingForTimer, "", lockID); err != nil {
			return fmt.Errorf("failed to update status: %w", err)
		}
		return nil
	})
}

// fenced runs fn in one transaction that first proves this run still holds
// the instance lock (which also row-locks the instance first on PostgreSQL
// and MySQL, as a cancel does). A run that lost its lock changes nothing;
// when the instance was cancelled during the run (fn hit
// ErrInstanceTerminated), nothing of fn is kept and the run ends quietly.
func (e *Engine) fenced(ctx context.Context, instanceID, lockID, what string, fn func(context.Context) error) error {
	run := func(txCtx context.Context) error {
		if err := e.storage.RefreshLock(txCtx, instanceID, lockID, LockLeaseSeconds); err != nil {
			return fmt.Errorf("failed to prove lock for %s: %w", what, err)
		}
		return fn(txCtx)
	}
	if e.storage.InTransaction(ctx) {
		return run(ctx)
	}
	txCtx, err := e.storage.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin %s: %w", what, err)
	}
	if err := run(txCtx); err != nil {
		_ = e.storage.RollbackTransaction(txCtx)
		if errors.Is(err, storage.ErrInstanceTerminated) {
			return nil
		}
		return err
	}
	if err := e.storage.CommitTransaction(txCtx); err != nil {
		return fmt.Errorf("failed to commit %s: %w", what, err)
	}
	return nil
}

// handleChannelMessageSuspend handles the channel message suspension case (via Receive or WaitEvent).
func (e *Engine) handleChannelMessageSuspend(ctx context.Context, instanceID string, sig *SuspendSignal, lockID string) error {
	// Register channel receive and set waiting=TRUE
	var timeoutAt *time.Time
	if sig.Timeout != nil {
		t := time.Now().UTC().Add(*sig.Timeout)
		timeoutAt = &t
	}
	// registered=false means a message was already pending at registration
	// time: the instance is left 'running' and unlocked so a resumption
	// replays it and Receive drains the pending message.
	if _, err := e.storage.RegisterChannelReceiveAndReleaseLock(ctx, instanceID, sig.Channel, lockID, sig.ActivityID, timeoutAt); err != nil {
		if errors.Is(err, storage.ErrInstanceTerminated) {
			return nil // cancelled during this run: no wait is registered
		}
		return fmt.Errorf("failed to register channel receive: %w", err)
	}

	return nil
}

// handleRecurSuspend handles the Recur case (Erlang-style tail recursion).
// The archive, the cleanups and the restart record commit together, in a
// transaction that first proves the run still holds the lock: a run that
// lost its lease leaves the new owner's history, subscriptions and groups
// alone, and a failure part-way leaves the iteration as it was instead of
// an instance with no history that would re-run with its old input.
func (e *Engine) handleRecurSuspend(ctx context.Context, instanceID string, sig *SuspendSignal, lockID string) error {
	inputData, err := json.Marshal(sig.NewInput)
	if err != nil {
		return fmt.Errorf("failed to serialize recur input: %w", err)
	}
	// A run cancelled during this iteration does not continue.
	return e.fenced(ctx, instanceID, lockID, "recur", func(txCtx context.Context) error {
		return e.recur(txCtx, instanceID, inputData, lockID)
	})
}

func (e *Engine) recur(ctx context.Context, instanceID string, inputData []byte, lockID string) error {
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

	// 3b. Drop the iteration's compensations: the iteration finished, and a
	// failure in a later one must not undo it (as edda and shikibu do)
	if err := e.storage.ClearCompensations(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to clear compensations: %w", err)
	}

	// 4. Record the recur event in history (for the new iteration)
	recurEvent := &storage.HistoryEvent{
		InstanceID: instanceID,
		ActivityID: "recur",
		EventType:  storage.HistoryActivityCompleted,
		EventData:  inputData,
		DataType:   "json",
	}
	if err := e.storage.AppendHistoryIfLocked(ctx, recurEvent, lockID); err != nil {
		return fmt.Errorf("failed to record recur event: %w", err)
	}

	// 5. Replace the instance input: the next iteration resumes the same
	// instance, and the runner reads instance.InputData — without this the
	// iteration would re-run with the ORIGINAL input, dropping the state
	// Recur exists to carry.
	if err := e.storage.UpdateInstanceInputIfLocked(ctx, instanceID, inputData, lockID); err != nil {
		return fmt.Errorf("failed to update recur input: %w", err)
	}

	// 6. Update status to recurred (will be picked up and restarted)
	if err := e.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusRecurred, "", lockID); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	return nil
}

// RecordActivityResult records an activity result in history.
func (e *Engine) RecordActivityResult(
	ctx context.Context,
	instanceID string,
	lockID string,
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
		failure := &RecordedFailure{Message: activityErr.Error()}
		var recorder FailureRecorder
		if errors.As(activityErr, &recorder) {
			failure = recorder.FailureRecord()
		}
		errData, err := json.Marshal(failure)
		if err != nil {
			return fmt.Errorf("failed to serialize failure: %w", err)
		}
		event.EventData = errData
	} else {
		event.EventType = storage.HistoryActivityCompleted
		resultData, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("failed to serialize result: %w", err)
		}
		event.EventData = resultData
	}

	return e.storage.AppendHistoryIfLocked(ctx, event, lockID)
}

// CompensationRunner is a function that executes a compensation.
// It receives the compensated instance, the function name and the
// JSON-encoded argument.
type CompensationRunner func(ctx context.Context, instanceID, funcName string, arg []byte) error

// SetCompensationRunner sets the compensation runner for the engine.
func (e *Engine) SetCompensationRunner(runner CompensationRunner) {
	e.compensationRunner = runner
}

// executeCompensations runs all pending compensations for a workflow in LIFO order.
// Status is tracked via history events (CompensationExecuted, CompensationFailed).
func (e *Engine) executeCompensations(ctx context.Context, instanceID, lockID string) error {
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

	// Get executed compensation IDs from history
	executedIDs, err := e.getExecutedCompensationIDs(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get executed compensations: %w", err)
	}

	var firstErr error
	for _, entry := range entries {
		// Skip already executed compensations
		if _, executed := executedIDs[entry.ID]; executed {
			continue
		}

		execErr := e.compensationRunner(ctx, instanceID, entry.ActivityName, entry.Args)
		if execErr != nil {
			// Record failure in history
			_ = e.recordCompensationResult(ctx, instanceID, lockID, entry, false, execErr.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("compensation %s failed: %w", entry.ActivityID, execErr)
			}
			// Continue with remaining compensations
			continue
		}

		// Record success in history
		_ = e.recordCompensationResult(ctx, instanceID, lockID, entry, true, "")
	}

	return firstErr
}

// getExecutedCompensationIDs returns a set of compensation IDs that have been executed.
func (e *Engine) getExecutedCompensationIDs(ctx context.Context, instanceID string) (map[int64]bool, error) {
	executed := make(map[int64]bool)

	// Iterate through all history events to find compensation results
	var afterID int64
	const pageSize = 100
	for {
		events, hasMore, err := e.storage.GetHistoryPaginated(ctx, instanceID, afterID, pageSize)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if event.EventType == storage.HistoryCompensationExecuted ||
				event.EventType == storage.HistoryCompensationFailed {
				// Parse compensation_id from event data
				var eventData map[string]any
				if err := json.Unmarshal(event.EventData, &eventData); err == nil {
					if id, ok := eventData["compensation_id"].(float64); ok {
						executed[int64(id)] = true
					}
				}
			}
			afterID = event.ID
		}
		if !hasMore {
			break
		}
	}
	return executed, nil
}

// recordCompensationResult records a compensation execution result to history.
func (e *Engine) recordCompensationResult(ctx context.Context, instanceID, lockID string, entry *storage.CompensationEntry, success bool, errorMsg string) error {
	eventType := storage.HistoryCompensationExecuted
	if !success {
		eventType = storage.HistoryCompensationFailed
	}

	eventData := map[string]any{
		"compensation_id": entry.ID,
		"activity_id":     entry.ActivityID,
		"activity_name":   entry.ActivityName,
	}
	if errorMsg != "" {
		eventData["error"] = errorMsg
	}

	eventDataBytes, err := json.Marshal(eventData)
	if err != nil {
		return err
	}

	return e.storage.AppendHistoryIfLocked(ctx, &storage.HistoryEvent{
		InstanceID: instanceID,
		ActivityID: fmt.Sprintf("compensation:%d", entry.ID),
		EventType:  eventType,
		EventData:  eventDataBytes,
		DataType:   "json",
	}, lockID)
}
