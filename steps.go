package romancy

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/i2y/romancy/internal/storage"
)

// Kernel primitives (registry, monitors, capabilities, tasks, channel
// reads) are side effects or nondeterministic reads. SEMANTICS §2 requires
// both to enter a workflow through recorded history: called from workflow
// code, a primitive runs once and every replay returns its recorded
// outcome. Called from inside an executing activity body it runs directly —
// the enclosing activity is then the unit that is recorded and replayed,
// and generating step ids there would desynchronize the counters on replay
// (activity bodies do not run when replayed).

// stepRecord is the history payload of a recorded step. Errors are part of
// the outcome, so a replay observes the same error, and Code keeps the
// sentinel matchable with errors.Is after the round trip.
type stepRecord struct {
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
}

// stepErrorCodes maps the kernel-defined errors to stable codes, shared
// with edda and shikibu. Only these errors are outcomes of a step: any other
// error (a storage failure, the lost lock, shutdown) aborts the run instead
// of being recorded, so the step runs again in a later run.
var stepErrorCodes = []struct {
	code string
	err  error
}{
	{"task_conflict", storage.ErrTaskConflict},
	{"task_not_found", storage.ErrTaskNotFound},
	{"actor_name_taken", storage.ErrActorNameTaken},
	{"fs_conflict", storage.ErrFSConflict},
	{"sandboxed", ErrSandboxed},
	{"channel_forbidden", ErrChannelForbidden},
	{"not_owner", storage.ErrNotOwner},
	{"cursor_not_found", storage.ErrCursorNotFound},
	{"invalid_argument", ErrInvalidArgument},
}

// stepErrorCode returns the code of a kernel-defined error, or "".
func stepErrorCode(err error) string {
	for _, c := range stepErrorCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}

// stepSentinel returns the kernel error a code stands for, or nil.
func stepSentinel(code string) error {
	for _, c := range stepErrorCodes {
		if c.code == code {
			return c.err
		}
	}
	return nil
}

// stepError is an error restored from a step record: it carries the
// recorded message and unwraps to the recorded sentinel, if any.
type stepError struct {
	msg      string
	sentinel error
}

func (e *stepError) Error() string { return e.msg }
func (e *stepError) Unwrap() error { return e.sentinel }

// runStep runs the read fn as the recorded workflow step name (see above):
// its outcome is recorded, so a replay observes the same answer.
func runStep[T any](ctx *WorkflowContext, name string, fn func(*WorkflowContext) (T, error)) (T, error) {
	return recordedStep(ctx, name, false, fn)
}

// runEffect runs the side effect fn as the recorded workflow step name. The
// effect and its record commit together, in a transaction that first proves
// the run still holds the instance lock: a run that lost its lease neither
// applies the effect nor records it, and an effect cannot land after a newer
// run took the instance over.
func runEffect[T any](ctx *WorkflowContext, name string, fn func(*WorkflowContext) (T, error)) (T, error) {
	return recordedStep(ctx, name, true, fn)
}

// runEffectErr is runEffect for effects without a result value.
func runEffectErr(ctx *WorkflowContext, name string, fn func(*WorkflowContext) error) error {
	_, err := runEffect(ctx, name, func(c *WorkflowContext) (struct{}, error) { return struct{}{}, fn(c) })
	return err
}

func recordedStep[T any](ctx *WorkflowContext, name string, effect bool, fn func(*WorkflowContext) (T, error)) (T, error) {
	var zero T
	if err := ctx.abortedErr(); err != nil {
		return zero, err
	}
	if ctx.InActivity() {
		return fn(ctx)
	}
	stepID := ctx.GenerateActivityID(name)
	if rec, ok := cachedStep(ctx, stepID); ok {
		return decodeStep[T](rec, stepID)
	}
	if !effect {
		value, err := fn(ctx)
		return recordStep(ctx, stepID, value, err)
	}

	var result T
	var fnErr error
	err := ctx.inFencedTransaction(func(tx *WorkflowContext) error {
		result, fnErr = fn(tx)
		if fnErr != nil {
			return fnErr
		}
		_, recErr := recordStep(tx, stepID, result, nil)
		return recErr
	})
	if fnErr != nil {
		// Nothing of a failed effect survives; its outcome, the error, is
		// recorded on its own.
		return recordStep(ctx, stepID, result, fnErr)
	}
	if err != nil {
		// The lock proof or the commit failed: not an outcome.
		return zero, ctx.abortRun(err)
	}
	return result, nil
}

// recordStep records the outcome of stepID and hands it back. Only a value
// or a kernel-defined error is an outcome; any other error (a storage
// failure, the lost lock, shutdown) aborts the run unrecorded, and a later
// run executes the step again.
func recordStep[T any](ctx *WorkflowContext, stepID string, value T, err error) (T, error) {
	var zero T
	rec := stepRecord{}
	if err != nil {
		rec.Code = stepErrorCode(err)
		if rec.Code == "" {
			return zero, ctx.abortRun(err)
		}
		rec.Error = err.Error()
	} else {
		raw, encErr := json.Marshal(value)
		if encErr != nil {
			return zero, ctx.abortRun(fmt.Errorf("failed to record step %s: %w", stepID, encErr))
		}
		rec.Value = raw
	}
	if recErr := ctx.RecordActivityResult(stepID, rec, nil); recErr != nil {
		return zero, ctx.abortRun(recErr)
	}
	return value, err
}

// cachedStep returns the recorded outcome of stepID, from history on a
// replay or from the in-run cache.
func cachedStep(ctx *WorkflowContext, stepID string) (stepRecord, bool) {
	var rec stepRecord
	if raw, ok := ctx.GetCachedResultRaw(stepID); ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &rec); err == nil {
			return rec, true
		}
	}
	if v, ok := ctx.GetCachedResult(stepID); ok {
		if r, isRec := v.(stepRecord); isRec {
			return r, true
		}
	}
	return rec, false
}

// decodeStep turns a recorded outcome back into the step's result.
func decodeStep[T any](rec stepRecord, stepID string) (T, error) {
	var out T
	if rec.Error != "" || rec.Code != "" {
		return out, &stepError{msg: rec.Error, sentinel: stepSentinel(rec.Code)}
	}
	if len(rec.Value) > 0 {
		if err := json.Unmarshal(rec.Value, &out); err != nil {
			return out, fmt.Errorf("failed to replay step %s: %w", stepID, err)
		}
	}
	return out, nil
}
