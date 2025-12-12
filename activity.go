package romancy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/retry"
)

// Activity represents a single unit of work within a workflow.
// Activities are the only way to perform I/O or side effects in workflows.
// I is the input type, O is the output type.
type Activity[I, O any] struct {
	name string
	fn   func(ctx context.Context, input I) (O, error)

	// Configuration
	retryPolicy    *retry.Policy
	timeout        time.Duration
	compensationFn func(ctx context.Context, input I) error
	transactional  bool // Wrap execution in a transaction (default: true)
}

// ActivityOption configures an activity.
type ActivityOption[I, O any] func(*Activity[I, O])

// DefineActivity creates a new activity.
// By default, activities are wrapped in a transaction for atomicity with
// history recording and outbox events.
func DefineActivity[I, O any](
	name string,
	fn func(ctx context.Context, input I) (O, error),
	opts ...ActivityOption[I, O],
) *Activity[I, O] {
	a := &Activity[I, O]{
		name:          name,
		fn:            fn,
		transactional: true, // Default: wrap in transaction
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name returns the activity name.
func (a *Activity[I, O]) Name() string {
	return a.name
}

// WithRetryPolicy sets the retry policy for the activity.
func WithRetryPolicy[I, O any](policy *retry.Policy) ActivityOption[I, O] {
	return func(a *Activity[I, O]) {
		a.retryPolicy = policy
	}
}

// WithTimeout sets the timeout for each activity execution attempt.
func WithTimeout[I, O any](d time.Duration) ActivityOption[I, O] {
	return func(a *Activity[I, O]) {
		a.timeout = d
	}
}

// WithCompensation sets the compensation function for the activity.
// This function will be called during saga rollback.
func WithCompensation[I, O any](fn func(ctx context.Context, input I) error) ActivityOption[I, O] {
	return func(a *Activity[I, O]) {
		a.compensationFn = fn
	}
}

// WithTransactional sets whether the activity execution should be wrapped
// in a database transaction. Default is true.
//
// When transactional=true (default):
//   - Activity execution, history recording, and outbox events are atomic
//   - Rollback occurs on failure
//   - Use ctx.Storage() for database operations within the same transaction
//
// When transactional=false:
//   - Useful for activities that call external APIs or don't need atomicity
//   - History and outbox events are still recorded, but not atomically
func WithTransactional[I, O any](transactional bool) ActivityOption[I, O] {
	return func(a *Activity[I, O]) {
		a.transactional = transactional
	}
}

// Execute runs the activity within a workflow context.
// If activityID is empty, it will be auto-generated.
//
// When transactional=true (default), the activity execution, history recording,
// and outbox events are wrapped in a database transaction for atomicity.
func (a *Activity[I, O]) Execute(
	ctx *WorkflowContext,
	input I,
	opts ...ExecuteOption,
) (O, error) {
	options := &executeOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Generate or use provided activity ID
	activityID := options.activityID
	if activityID == "" {
		activityID = ctx.GenerateActivityID(a.name)
	}

	// Call OnActivityStart hook
	if h := ctx.Hooks(); h != nil {
		h.OnActivityStart(ctx.Context(), hooks.ActivityStartInfo{
			InstanceID:   ctx.InstanceID(),
			WorkflowName: ctx.WorkflowName(),
			ActivityID:   activityID,
			ActivityName: a.name,
			InputData:    input,
			IsReplay:     ctx.IsReplaying(),
		})
	}

	// Check if result is cached (replay)
	if result, ok := ctx.GetCachedResult(activityID); ok {
		// Call OnActivityCacheHit hook
		if h := ctx.Hooks(); h != nil {
			h.OnActivityCacheHit(ctx.Context(), hooks.ActivityCacheHitInfo{
				InstanceID:   ctx.InstanceID(),
				WorkflowName: ctx.WorkflowName(),
				ActivityID:   activityID,
				ActivityName: a.name,
				CachedResult: result,
			})
		}
		// Type assertion with proper handling
		if typedResult, ok := result.(O); ok {
			return typedResult, nil
		}
		// Handle error results stored in cache
		if err, ok := result.(error); ok {
			var zero O
			return zero, err
		}
		var zero O
		return zero, fmt.Errorf("cached result type mismatch for activity %s", activityID)
	}

	// Execute with or without transaction based on configuration
	if a.transactional && ctx.Storage() != nil {
		return a.executeInTransaction(ctx, input, activityID)
	}

	return a.executeWithoutTransaction(ctx, input, activityID)
}

// executeInTransaction runs the activity within a database transaction.
func (a *Activity[I, O]) executeInTransaction(
	ctx *WorkflowContext,
	input I,
	activityID string,
) (O, error) {
	var zero O

	// Begin transaction
	txCtx, err := ctx.Storage().BeginTransaction(ctx.Context())
	if err != nil {
		return zero, fmt.Errorf("failed to begin transaction: %w", err)
	}

	// Create a new context with the transaction
	txWorkflowCtx := ctx.WithContext(txCtx)

	// Execute the activity
	result, execErr := a.executeWithRetry(txWorkflowCtx, input, activityID)

	// Record result to storage and cache
	if recordErr := txWorkflowCtx.RecordActivityResult(activityID, result, execErr); recordErr != nil {
		// Rollback on record failure
		_ = ctx.Storage().RollbackTransaction(txCtx)
		return zero, fmt.Errorf("failed to record activity result: %w", recordErr)
	}

	if execErr != nil {
		// Rollback on execution failure
		_ = ctx.Storage().RollbackTransaction(txCtx)
		return zero, execErr
	}

	// Record activity ID for tracking
	txWorkflowCtx.RecordActivityID(activityID)

	// Register compensation if defined
	if a.compensationFn != nil {
		if compErr := a.registerCompensation(txWorkflowCtx, activityID, input); compErr != nil {
			// Log but don't fail the activity
			slog.Debug("failed to register compensation", "error", compErr, "activityID", activityID)
		}
	}

	// Commit the transaction
	if commitErr := ctx.Storage().CommitTransaction(txCtx); commitErr != nil {
		return zero, fmt.Errorf("failed to commit transaction: %w", commitErr)
	}

	return result, nil
}

// executeWithoutTransaction runs the activity without a transaction wrapper.
func (a *Activity[I, O]) executeWithoutTransaction(
	ctx *WorkflowContext,
	input I,
	activityID string,
) (O, error) {
	// Execute the activity
	result, err := a.executeWithRetry(ctx, input, activityID)

	// Record result to storage and cache (only when not replaying)
	if recordErr := ctx.RecordActivityResult(activityID, result, err); recordErr != nil {
		var zero O
		return zero, fmt.Errorf("failed to record activity result: %w", recordErr)
	}

	if err != nil {
		var zero O
		return zero, err
	}

	// Record activity ID for tracking
	ctx.RecordActivityID(activityID)

	// Register compensation if defined and storage is available
	if a.compensationFn != nil && ctx.Storage() != nil {
		if compErr := a.registerCompensation(ctx, activityID, input); compErr != nil {
			// Log but don't fail the activity
			slog.Debug("failed to register compensation", "error", compErr, "activityID", activityID)
		}
	}

	return result, nil
}

// registerCompensation registers a compensation action for the activity.
func (a *Activity[I, O]) registerCompensation(ctx *WorkflowContext, activityID string, input I) error {
	// JSON-encode the input for later use
	inputData, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("failed to encode compensation argument: %w", err)
	}

	// Get current compensation count for ordering
	entries, err := ctx.Storage().GetCompensations(ctx.Context(), ctx.InstanceID())
	if err != nil {
		return fmt.Errorf("failed to get existing compensations: %w", err)
	}

	entry := &storage.CompensationEntry{
		InstanceID:      ctx.InstanceID(),
		ActivityID:      activityID,
		CompensationFn:  a.name, // Use activity name as the compensation function name
		CompensationArg: inputData,
		Order:           len(entries) + 1, // Higher order = execute first (LIFO)
		Status:          "pending",
	}

	return ctx.Storage().AddCompensation(ctx.Context(), entry)
}

// executeWithRetry executes the activity with retry logic.
func (a *Activity[I, O]) executeWithRetry(ctx *WorkflowContext, input I, activityID string) (O, error) {
	policy := a.retryPolicy
	if policy == nil {
		policy = retry.DefaultPolicy()
	}

	var lastErr error
	attempts := 0
	startTime := time.Now()

	for {
		attempts++

		// Create execution context with timeout
		execCtx := ctx.Context()
		var cancel context.CancelFunc
		if a.timeout > 0 {
			execCtx, cancel = context.WithTimeout(execCtx, a.timeout)
		}

		// Embed WorkflowContext in the execution context
		// This allows activities to access workflow features via GetWorkflowContext()
		execCtx = ContextWithWorkflowContext(execCtx, ctx)

		// Execute the activity
		result, err := a.fn(execCtx, input)

		if cancel != nil {
			cancel()
		}

		if err == nil {
			// Call OnActivityComplete hook
			if h := ctx.Hooks(); h != nil {
				h.OnActivityComplete(ctx.Context(), hooks.ActivityCompleteInfo{
					InstanceID:   ctx.InstanceID(),
					WorkflowName: ctx.WorkflowName(),
					ActivityID:   activityID,
					ActivityName: a.name,
					OutputData:   result,
					Duration:     time.Since(startTime),
					IsReplay:     false,
				})
			}
			return result, nil
		}

		lastErr = err

		// Check if error is terminal (should not retry)
		if IsTerminalError(err) {
			// Call OnActivityFailed hook
			if h := ctx.Hooks(); h != nil {
				h.OnActivityFailed(ctx.Context(), hooks.ActivityFailedInfo{
					InstanceID:   ctx.InstanceID(),
					WorkflowName: ctx.WorkflowName(),
					ActivityID:   activityID,
					ActivityName: a.name,
					Error:        err,
					Duration:     time.Since(startTime),
					Attempt:      attempts,
				})
			}
			var zero O
			return zero, err
		}

		// Check if we should retry
		if !policy.ShouldRetry(attempts, err) {
			retryErr := &RetryExhaustedError{
				ActivityName: a.name,
				Attempts:     attempts,
				LastErr:      lastErr,
			}
			// Call OnActivityFailed hook
			if h := ctx.Hooks(); h != nil {
				h.OnActivityFailed(ctx.Context(), hooks.ActivityFailedInfo{
					InstanceID:   ctx.InstanceID(),
					WorkflowName: ctx.WorkflowName(),
					ActivityID:   activityID,
					ActivityName: a.name,
					Error:        retryErr,
					Duration:     time.Since(startTime),
					Attempt:      attempts,
				})
			}
			var zero O
			return zero, retryErr
		}

		// Wait before retrying
		delay := policy.GetDelay(attempts)

		// Call OnActivityRetry hook
		if h := ctx.Hooks(); h != nil {
			h.OnActivityRetry(ctx.Context(), hooks.ActivityRetryInfo{
				InstanceID:   ctx.InstanceID(),
				WorkflowName: ctx.WorkflowName(),
				ActivityID:   activityID,
				ActivityName: a.name,
				Attempt:      attempts,
				MaxAttempts:  policy.MaxAttempts,
				NextDelay:    delay,
				Error:        lastErr,
			})
		}

		select {
		case <-time.After(delay):
			// Continue to next attempt
		case <-ctx.Done():
			var zero O
			return zero, ctx.Err()
		}
	}
}

// ExecuteOption configures activity execution.
type ExecuteOption func(*executeOptions)

type executeOptions struct {
	activityID string
}

// WithActivityID specifies a manual activity ID.
// Required for concurrent activity execution to maintain determinism.
func WithActivityID(id string) ExecuteOption {
	return func(o *executeOptions) {
		o.activityID = id
	}
}

// Compensate executes the compensation function if defined.
func (a *Activity[I, O]) Compensate(ctx context.Context, input I) error {
	if a.compensationFn == nil {
		return nil
	}
	return a.compensationFn(ctx, input)
}

// HasCompensation returns true if the activity has a compensation function.
func (a *Activity[I, O]) HasCompensation() bool {
	return a.compensationFn != nil
}

// CompensationExecutor is a function that executes a compensation.
// It takes JSON-encoded input and returns an error.
type CompensationExecutor func(ctx context.Context, input []byte) error

// activityRegistry stores compensation executors by activity name.
var (
	activityRegistry   = make(map[string]CompensationExecutor)
	activityRegistryMu sync.RWMutex
)

// RegisterActivity registers an activity's compensation function globally.
// This is called automatically when an activity with compensation is created.
func RegisterActivity[I, O any](activity *Activity[I, O]) {
	if activity.compensationFn == nil {
		return
	}

	activityRegistryMu.Lock()
	defer activityRegistryMu.Unlock()

	activityRegistry[activity.name] = func(ctx context.Context, inputData []byte) error {
		var input I
		if err := json.Unmarshal(inputData, &input); err != nil {
			return fmt.Errorf("failed to unmarshal compensation input: %w", err)
		}
		return activity.compensationFn(ctx, input)
	}
}

// GetCompensationExecutor returns the compensation executor for an activity.
func GetCompensationExecutor(activityName string) (CompensationExecutor, bool) {
	activityRegistryMu.RLock()
	defer activityRegistryMu.RUnlock()

	executor, ok := activityRegistry[activityName]
	return executor, ok
}
