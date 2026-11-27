package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
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
	// Auto-register compensation executor if compensation is set
	// This makes the compensation function available for execution during rollback
	if a.compensationFn != nil {
		RegisterActivity(a)
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

	// An aborted run goes no further (see ErrRunAborted).
	if err := ctx.abortedErr(); err != nil {
		var zero O
		return zero, err
	}

	// Generate or use provided activity ID
	activityID := options.activityID
	if activityID == "" {
		activityID = ctx.GenerateActivityID(a.name)
	} else {
		ctx.useExplicitID(activityID)
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
			return zero, restoreActivityError(err)
		}
		// A result loaded from history is a generic JSON value (a map for a
		// struct, float64 for an int, ...): decode the recorded bytes into
		// the activity's output type.
		var zero O
		if raw, ok := ctx.GetCachedResultRaw(activityID); ok && len(raw) > 0 {
			var typed O
			if err := json.Unmarshal(raw, &typed); err != nil {
				return zero, fmt.Errorf("failed to decode cached result for activity %s: %w", activityID, err)
			}
			return typed, nil
		}
		return zero, fmt.Errorf("cached result type mismatch for activity %s", activityID)
	}

	// Execute with or without transaction based on configuration
	if a.transactional && ctx.Storage() != nil {
		return a.executeInTransaction(ctx, input, activityID)
	}

	return a.executeWithoutTransaction(ctx, input, activityID)
}

// maxConflictRestarts bounds how many times an attempt is run again in a new
// transaction after an SQLite write conflict.
const maxConflictRestarts = 10

// attemptAbortedError ends an activity at once, without a retry: the
// attempt's transaction could not begin, record its result or commit.
type attemptAbortedError struct{ err error }

func (e *attemptAbortedError) Error() string { return e.err.Error() }
func (e *attemptAbortedError) Unwrap() error { return e.err }

// executeInTransaction runs each attempt of the activity in its own database
// transaction, which commits the body's writes together with the result
// record. A failed attempt is rolled back before the policy retries it, so
// none of its writes, outbox events or post-commit callbacks survive, and a
// retry on PostgreSQL does not inherit an aborted transaction (edda and
// shikibu also run each attempt in its own transaction).
//
// When the activity gives up, its failure is recorded after the last
// attempt's rollback (recordFailure), so a replay returns the failure
// instead of running the body again.
func (a *Activity[I, O]) executeInTransaction(
	ctx *WorkflowContext,
	input I,
	activityID string,
) (O, error) {
	result, err := a.executeWithRetry(ctx, activityID, func() (O, error) {
		return a.attemptInTransaction(ctx, input, activityID)
	})
	if err == nil {
		return result, nil
	}
	var zero O
	return zero, a.recordFailure(ctx, activityID, err)
}

// recordFailure ends an activity that failed. A failure of the activity
// itself (its body gave up, or the retry policy did) is recorded in history,
// so a replay returns the same failure without running the body again. An
// attempt that could not begin, record or commit, a lost lock and a
// cancelled run are not outcomes of the activity: they abort the run, and a
// later run executes the activity again.
func (a *Activity[I, O]) recordFailure(ctx *WorkflowContext, activityID string, err error) error {
	var aborted *attemptAbortedError
	if errors.As(err, &aborted) {
		return ctx.abortRun(aborted.err)
	}
	if errors.Is(err, ErrRunAborted) || errors.Is(err, storage.ErrLockNotHeld) || ctx.Context().Err() != nil {
		return ctx.abortRun(err)
	}
	failure := &recordedActivityError{err: err, record: activityFailureRecord(a.name, err)}
	if recErr := ctx.RecordActivityResult(activityID, nil, failure); recErr != nil {
		return ctx.abortRun(fmt.Errorf("failed to record activity failure: %w", recErr))
	}
	return err
}

// attemptInTransaction runs one attempt in a new transaction.
//
// On SQLite (WAL), a body that reads before the transaction's first write
// (the result record is such a write) fails to write at once when another
// connection writes in between (storage.IsWriteConflict). The attempt is
// rolled back and runs again from the start in a new transaction; the restart
// does not count as a retry.
func (a *Activity[I, O]) attemptInTransaction(
	ctx *WorkflowContext,
	input I,
	activityID string,
) (O, error) {
	for restarts := 0; ; restarts++ {
		result, err := a.attemptInTransactionOnce(ctx, input, activityID)
		if err == nil || restarts >= maxConflictRestarts || !storage.IsWriteConflict(err) {
			return result, err
		}
		select {
		case <-time.After(time.Duration(restarts+1) * 10 * time.Millisecond):
		case <-ctx.Done():
			var zero O
			return zero, &attemptAbortedError{err: ctx.Err()}
		}
	}
}

// attemptInTransactionOnce runs the body once in a new transaction and, when
// it succeeds, records the result, registers the compensation and commits.
// A body error rolls the transaction back and is returned as is.
func (a *Activity[I, O]) attemptInTransactionOnce(
	ctx *WorkflowContext,
	input I,
	activityID string,
) (O, error) {
	var zero O

	txCtx, err := ctx.Storage().BeginTransaction(ctx.Context())
	if err != nil {
		return zero, &attemptAbortedError{err: fmt.Errorf("failed to begin transaction: %w", err)}
	}

	// The body reaches the transaction through this context.
	txWorkflowCtx := ctx.WithContext(txCtx)

	result, bodyErr := a.runBody(txWorkflowCtx, input)
	if bodyErr != nil {
		_ = ctx.Storage().RollbackTransaction(txCtx)
		return zero, bodyErr
	}

	if recordErr := txWorkflowCtx.RecordActivityResult(activityID, result, nil); recordErr != nil {
		_ = ctx.Storage().RollbackTransaction(txCtx)
		return zero, &attemptAbortedError{err: fmt.Errorf("failed to record activity result: %w", recordErr)}
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

	if commitErr := ctx.Storage().CommitTransaction(txCtx); commitErr != nil {
		return zero, &attemptAbortedError{err: fmt.Errorf("failed to commit transaction: %w", commitErr)}
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
	result, err := a.executeWithRetry(ctx, activityID, func() (O, error) {
		return a.runBody(ctx, input)
	})
	if err != nil {
		var zero O
		return zero, a.recordFailure(ctx, activityID, err)
	}

	// Record result to storage and cache (only when not replaying)
	if recordErr := ctx.RecordActivityResult(activityID, result, nil); recordErr != nil {
		var zero O
		return zero, ctx.abortRun(fmt.Errorf("failed to record activity result: %w", recordErr))
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

	entry := &storage.CompensationEntry{
		InstanceID:   ctx.InstanceID(),
		ActivityID:   activityID,
		ActivityName: a.name, // Use activity name as the compensation function name
		Args:         inputData,
	}

	return ctx.Storage().AddCompensation(ctx.Context(), entry)
}

// runBody runs the activity function once, under the activity's timeout. The
// function reaches ctx (and through it any transaction) with
// GetWorkflowContext.
func (a *Activity[I, O]) runBody(ctx *WorkflowContext, input I) (O, error) {
	execCtx := ctx.Context()
	if a.timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, a.timeout)
		defer cancel()
	}
	// The body has its own copy of the context, marked as inside an
	// activity: activities run from several goroutines of one workflow mark
	// neither the workflow nor each other.
	bodyCtx := ctx.WithContext(ctx.Context())
	bodyCtx.inActivity = true
	execCtx = ContextWithWorkflowContext(execCtx, bodyCtx)
	return a.fn(execCtx, input)
}

// executeWithRetry runs attempt under the activity's retry policy until it
// succeeds or fails with an error the policy does not retry. An
// attemptAbortedError is returned at once.
func (a *Activity[I, O]) executeWithRetry(ctx *WorkflowContext, activityID string, attempt func() (O, error)) (O, error) {
	policy := a.retryPolicy
	if policy == nil {
		policy = retry.DefaultPolicy()
	}

	var lastErr error
	attempts := 0
	startTime := time.Now()

	for {
		attempts++

		result, err := attempt()

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

		var aborted *attemptAbortedError
		if errors.As(err, &aborted) {
			var zero O
			return zero, err
		}

		// A run that is over (aborted, its lock lost, shutting down) does not
		// retry: it ends, and a later run executes the activity again.
		if errors.Is(err, ErrRunAborted) || errors.Is(err, storage.ErrLockNotHeld) || ctx.Context().Err() != nil {
			var zero O
			return zero, err
		}

		lastErr = err

		// Run inside another activity's transaction (a nested activity), a
		// write conflict is handed back to the enclosing attempt, which
		// restarts as a whole, instead of being retried here.
		if s := ctx.Storage(); s != nil && s.InTransaction(ctx.Context()) && storage.IsWriteConflict(err) {
			var zero O
			return zero, err
		}

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

// recordedActivityError is an activity's failure on its way to history: it
// is the original error (errors.Is/As see through it) and carries the record
// written for it.
type recordedActivityError struct {
	err    error
	record *replay.RecordedFailure
}

func (e *recordedActivityError) Error() string                          { return e.err.Error() }
func (e *recordedActivityError) Unwrap() error                          { return e.err }
func (e *recordedActivityError) FailureRecord() *replay.RecordedFailure { return e.record }

// activityFailureRecord describes err for history: its message, and what a
// replay needs to rebuild the error types callers match with errors.Is/As
// (TerminalError, RetryExhaustedError, the kernel errors).
func activityFailureRecord(activityName string, err error) *replay.RecordedFailure {
	rec := &replay.RecordedFailure{
		ActivityName: activityName,
		ErrorType:    fmt.Sprintf("%T", err),
		Message:      err.Error(),
		Code:         stepErrorCode(err),
	}
	var exhausted *RetryExhaustedError
	var terminal *TerminalError
	switch {
	case errors.As(err, &exhausted):
		rec.ActivityName = exhausted.ActivityName
		rec.Attempts = exhausted.Attempts
		if exhausted.LastErr != nil {
			rec.Inner = exhausted.LastErr.Error()
		}
	case errors.As(err, &terminal):
		rec.Terminal = true
		if terminal.Err != nil {
			rec.Inner = terminal.Err.Error()
		}
	}
	return rec
}

// restoredFailure is an error read back from history: the original message,
// unwrapping to the error types the failure was recorded with.
type restoredFailure struct {
	msg     string
	wrapped []error
}

func (e *restoredFailure) Error() string   { return e.msg }
func (e *restoredFailure) Unwrap() []error { return e.wrapped }

// restoreActivityError turns a failure recorded in history back into an
// error with the original message that still matches TerminalError,
// RetryExhaustedError and the kernel errors. Failures recorded before these
// fields existed come back as their message only.
func restoreActivityError(err error) error {
	var f *replay.RecordedFailure
	if !errors.As(err, &f) {
		return err
	}
	var sentinels []error
	if s := stepSentinel(f.Code); s != nil {
		sentinels = append(sentinels, s)
	}
	inner := func(msg string) error {
		if len(sentinels) == 0 {
			return errors.New(msg)
		}
		return &restoredFailure{msg: msg, wrapped: sentinels}
	}
	switch {
	case f.Attempts > 0:
		exhausted := &RetryExhaustedError{ActivityName: f.ActivityName, Attempts: f.Attempts, LastErr: inner(f.Inner)}
		if exhausted.Error() == f.Message {
			return exhausted
		}
		return &restoredFailure{msg: f.Message, wrapped: []error{exhausted}}
	case f.Terminal:
		terminal := &TerminalError{Err: inner(f.Inner)}
		if terminal.Error() == f.Message {
			return terminal
		}
		return &restoredFailure{msg: f.Message, wrapped: []error{terminal}}
	case len(sentinels) > 0:
		return &restoredFailure{msg: f.Message, wrapped: sentinels}
	default:
		return f
	}
}
