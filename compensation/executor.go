// Package compensation provides saga compensation execution.
package compensation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/i2y/romancy/internal/storage"
)

// CompensationFunc is a function that can be executed as compensation.
// The argument is JSON-encoded data that was passed when registering.
type CompensationFunc func(ctx context.Context, arg []byte) error

// Registry holds registered compensation functions by name.
type Registry struct {
	mu    sync.RWMutex
	funcs map[string]CompensationFunc
}

// NewRegistry creates a new compensation function registry.
func NewRegistry() *Registry {
	return &Registry{
		funcs: make(map[string]CompensationFunc),
	}
}

// Register adds a compensation function to the registry.
func (r *Registry) Register(name string, fn CompensationFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[name] = fn
}

// Get retrieves a compensation function by name.
func (r *Registry) Get(name string) (CompensationFunc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.funcs[name]
	return fn, ok
}

// Executor handles saga compensation execution.
type Executor struct {
	storage  storage.Storage
	registry *Registry
}

// NewExecutor creates a new compensation executor.
func NewExecutor(s storage.Storage, registry *Registry) *Executor {
	return &Executor{
		storage:  s,
		registry: registry,
	}
}

// Entry represents a compensation entry to be registered.
type Entry struct {
	ActivityID   string
	FunctionName string
	Argument     any // Will be JSON-encoded
}

// AddCompensation registers a compensation action for an activity.
// The compensation will be executed in reverse order (LIFO) if the workflow fails.
// Order is determined by created_at DESC, status is tracked via history events.
func (e *Executor) AddCompensation(ctx context.Context, instanceID string, entry Entry) error {
	// JSON-encode the argument
	argBytes, err := json.Marshal(entry.Argument)
	if err != nil {
		return fmt.Errorf("failed to encode compensation argument: %w", err)
	}

	// Create storage entry
	storageEntry := &storage.CompensationEntry{
		InstanceID:   instanceID,
		ActivityID:   entry.ActivityID,
		ActivityName: entry.FunctionName,
		Args:         argBytes,
	}

	return e.storage.AddCompensation(ctx, storageEntry)
}

// ExecuteCompensations runs all pending compensations for a workflow in LIFO order.
// Returns the first error encountered, but continues executing remaining compensations.
// Status is tracked via history events (CompensationExecuted, CompensationFailed).
func (e *Executor) ExecuteCompensations(ctx context.Context, instanceID string) error {
	entries, err := e.storage.GetCompensations(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get compensations: %w", err)
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

		execErr := e.executeOne(ctx, entry)
		if execErr != nil {
			// Record failure in history (ignore error, continue with remaining compensations)
			_ = e.recordCompensationResult(ctx, instanceID, entry, false, execErr.Error())
			if firstErr == nil {
				firstErr = execErr
			}
			continue
		}

		// Record success in history (ignore error, continue with remaining compensations)
		_ = e.recordCompensationResult(ctx, instanceID, entry, true, "")
	}

	return firstErr
}

// getExecutedCompensationIDs returns a set of compensation IDs that have been executed.
func (e *Executor) getExecutedCompensationIDs(ctx context.Context, instanceID string) (map[int64]bool, error) {
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
func (e *Executor) recordCompensationResult(ctx context.Context, instanceID string, entry *storage.CompensationEntry, success bool, errorMsg string) error {
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

	return e.storage.AppendHistory(ctx, &storage.HistoryEvent{
		InstanceID: instanceID,
		ActivityID: fmt.Sprintf("compensation:%d", entry.ID),
		EventType:  eventType,
		EventData:  eventDataBytes,
		DataType:   "json",
	})
}

// executeOne executes a single compensation.
func (e *Executor) executeOne(ctx context.Context, entry *storage.CompensationEntry) error {
	fn, ok := e.registry.Get(entry.ActivityName)
	if !ok {
		return fmt.Errorf("compensation function not found: %s", entry.ActivityName)
	}

	return fn(ctx, entry.Args)
}

// CompensationError wraps an error that occurred during compensation execution.
type CompensationError struct {
	ActivityID string
	FuncName   string
	Err        error
}

func (e *CompensationError) Error() string {
	return fmt.Sprintf("compensation failed for activity %s (%s): %v", e.ActivityID, e.FuncName, e.Err)
}

func (e *CompensationError) Unwrap() error {
	return e.Err
}

// Helper type for typed compensation functions

// TypedCompensation wraps a typed compensation function for registry.
type TypedCompensation[T any] struct {
	Name string
	Fn   func(ctx context.Context, arg T) error
}

// NewTypedCompensation creates a typed compensation wrapper.
func NewTypedCompensation[T any](name string, fn func(ctx context.Context, arg T) error) *TypedCompensation[T] {
	return &TypedCompensation[T]{
		Name: name,
		Fn:   fn,
	}
}

// Register registers the typed compensation function in the registry.
func (tc *TypedCompensation[T]) Register(r *Registry) {
	r.Register(tc.Name, func(ctx context.Context, arg []byte) error {
		var typedArg T
		if err := json.Unmarshal(arg, &typedArg); err != nil {
			return fmt.Errorf("failed to unmarshal compensation argument: %w", err)
		}
		return tc.Fn(ctx, typedArg)
	})
}

// AsFunc returns the raw compensation function for direct use.
func (tc *TypedCompensation[T]) AsFunc() CompensationFunc {
	return func(ctx context.Context, arg []byte) error {
		var typedArg T
		if err := json.Unmarshal(arg, &typedArg); err != nil {
			return fmt.Errorf("failed to unmarshal compensation argument: %w", err)
		}
		return tc.Fn(ctx, typedArg)
	}
}
