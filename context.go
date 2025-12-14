package romancy

import (
	"context"
	"fmt"
	"sync"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
)

// workflowContextKey is the key used to store WorkflowContext in context.Context.
type workflowContextKey struct{}

// ContextWithWorkflowContext returns a new context with the WorkflowContext embedded.
// This is used internally to pass WorkflowContext to activities.
func ContextWithWorkflowContext(ctx context.Context, wfCtx *WorkflowContext) context.Context {
	return context.WithValue(ctx, workflowContextKey{}, wfCtx)
}

// GetWorkflowContext retrieves the WorkflowContext from a context.Context.
// This is useful in activities to access workflow-level features like Session()
// for database operations within the same transaction.
//
// Returns nil if the context does not contain a WorkflowContext.
//
// Example:
//
//	activity := romancy.DefineActivity("process_order", func(ctx context.Context, input OrderInput) (OrderResult, error) {
//	    wfCtx := romancy.GetWorkflowContext(ctx)
//	    if wfCtx == nil {
//	        return OrderResult{}, fmt.Errorf("workflow context not available")
//	    }
//	    session := wfCtx.Session()
//	    // Use session for database operations...
//	    return OrderResult{}, nil
//	})
func GetWorkflowContext(ctx context.Context) *WorkflowContext {
	if v := ctx.Value(workflowContextKey{}); v != nil {
		if wfCtx, ok := v.(*WorkflowContext); ok {
			return wfCtx
		}
	}
	return nil
}

// WorkflowContext provides context for workflow execution.
// It manages activity ID generation, replay state, and history caching.
type WorkflowContext struct {
	ctx        context.Context
	cancel     context.CancelFunc
	instanceID string
	workerID   string

	// Workflow metadata
	workflowName string

	// Replay state
	isReplaying  bool
	historyCache map[string]any // activity_id → result

	// Activity ID management
	activityCounts map[string]int // function_name → counter
	mu             sync.Mutex

	// Reference to the replay execution context (when using replay engine)
	execCtx *replay.ExecutionContext

	// Reference to the App for accessing app-level configuration
	app *App
}

// NewWorkflowContext creates a new WorkflowContext.
func NewWorkflowContext(
	ctx context.Context,
	instanceID, workerID, workflowName string,
	historyCache map[string]any,
	isReplaying bool,
) *WorkflowContext {
	ctx, cancel := context.WithCancel(ctx)
	return &WorkflowContext{
		ctx:            ctx,
		cancel:         cancel,
		instanceID:     instanceID,
		workerID:       workerID,
		workflowName:   workflowName,
		isReplaying:    isReplaying,
		historyCache:   historyCache,
		activityCounts: make(map[string]int),
	}
}

// NewWorkflowContextFromExecution creates a WorkflowContext from a replay ExecutionContext.
func NewWorkflowContextFromExecution(execCtx *replay.ExecutionContext) *WorkflowContext {
	ctx, cancel := context.WithCancel(execCtx.Context())
	return &WorkflowContext{
		ctx:            ctx,
		cancel:         cancel,
		instanceID:     execCtx.InstanceID(),
		isReplaying:    execCtx.IsReplaying(),
		activityCounts: make(map[string]int),
		execCtx:        execCtx,
	}
}

// Context returns the underlying context.Context.
func (c *WorkflowContext) Context() context.Context {
	return c.ctx
}

// WithContext returns a copy of the WorkflowContext with a new underlying context.
// This is useful for passing transaction contexts to storage operations.
func (c *WorkflowContext) WithContext(ctx context.Context) *WorkflowContext {
	return &WorkflowContext{
		ctx:            ctx,
		cancel:         c.cancel,
		instanceID:     c.instanceID,
		workerID:       c.workerID,
		workflowName:   c.workflowName,
		isReplaying:    c.isReplaying,
		historyCache:   c.historyCache,
		activityCounts: c.activityCounts,
		execCtx:        c.execCtx,
	}
}

// InstanceID returns the workflow instance ID.
func (c *WorkflowContext) InstanceID() string {
	return c.instanceID
}

// WorkflowName returns the workflow name.
func (c *WorkflowContext) WorkflowName() string {
	return c.workflowName
}

// WorkerID returns the worker ID that is executing this workflow.
func (c *WorkflowContext) WorkerID() string {
	return c.workerID
}

// IsReplaying returns true if the workflow is being replayed.
func (c *WorkflowContext) IsReplaying() bool {
	return c.isReplaying
}

// GenerateActivityID generates a unique activity ID for the given function name.
// Format: {function_name}:{counter}
// This is used for deterministic replay - the same sequence of calls
// will always generate the same IDs.
func (c *WorkflowContext) GenerateActivityID(functionName string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.activityCounts[functionName]++
	return fmt.Sprintf("%s:%d", functionName, c.activityCounts[functionName])
}

// GetCachedResult retrieves a cached result for the given activity ID.
// Returns the result and true if found, or nil and false if not cached.
func (c *WorkflowContext) GetCachedResult(activityID string) (any, bool) {
	// Delegate to execution context when using replay engine
	if c.execCtx != nil {
		return c.execCtx.GetCachedResult(activityID)
	}
	// Fallback to local cache
	if c.historyCache == nil {
		return nil, false
	}
	result, ok := c.historyCache[activityID]
	return result, ok
}

// GetCachedResultRaw retrieves a cached result with raw JSON bytes.
// Returns (rawJSON, true) if found, (nil, false) if not found.
// Use this to avoid re-serialization when the raw JSON is needed.
func (c *WorkflowContext) GetCachedResultRaw(activityID string) ([]byte, bool) {
	// Delegate to execution context when using replay engine
	if c.execCtx != nil {
		if cached, ok := c.execCtx.GetCachedResultRaw(activityID); ok {
			return cached.RawJSON, true
		}
	}
	return nil, false
}

// SetCachedResult stores a result in the history cache.
// This is typically called after activity execution to cache results locally.
func (c *WorkflowContext) SetCachedResult(activityID string, result any) {
	// Delegate to execution context when using replay engine
	if c.execCtx != nil {
		c.execCtx.SetCachedResult(activityID, result)
		return
	}
	// Fallback to local cache
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.historyCache == nil {
		c.historyCache = make(map[string]any)
	}
	c.historyCache[activityID] = result
}

// RecordActivityResult records an activity result to storage and caches it.
// This is called after activity execution when NOT replaying.
func (c *WorkflowContext) RecordActivityResult(activityID string, result any, err error) error {
	// Record to storage if using replay engine
	if c.execCtx != nil {
		// Use the WorkflowContext's ctx (which may be a transaction context)
		// instead of the ExecutionContext's original ctx
		if recordErr := c.execCtx.RecordActivityResultWithContext(c.ctx, activityID, result, err); recordErr != nil {
			return recordErr
		}
	}
	// Cache the result (either result or error)
	if err != nil {
		c.SetCachedResult(activityID, err)
	} else {
		c.SetCachedResult(activityID, result)
	}
	return nil
}

// RecordActivityID records that an activity ID has been used.
// This is used for tracking the current activity during execution.
func (c *WorkflowContext) RecordActivityID(activityID string) {
	// Record the current activity in execution context
	if c.execCtx != nil {
		c.execCtx.RecordActivityID(activityID)
	}
}

// Cancel cancels the workflow execution.
func (c *WorkflowContext) Cancel() {
	c.cancel()
}

// Err returns any error from the context.
func (c *WorkflowContext) Err() error {
	return c.ctx.Err()
}

// Done returns a channel that's closed when the context is cancelled.
func (c *WorkflowContext) Done() <-chan struct{} {
	return c.ctx.Done()
}

// Storage returns the storage interface for advanced operations like compensation.
// Returns nil if not using the replay engine.
func (c *WorkflowContext) Storage() storage.Storage {
	if c.execCtx != nil {
		return c.execCtx.Storage()
	}
	return nil
}

// Session returns the database executor for the current context.
// When called within a transactional activity, this returns the transaction,
// allowing you to execute custom SQL queries in the same transaction as
// the activity execution, history recording, and outbox events.
//
// Returns nil if storage is not available.
//
// Example:
//
//	activity := romancy.DefineActivity("process_order", func(ctx context.Context, input OrderInput) (OrderResult, error) {
//	    // Get the database session (transaction if in transactional activity)
//	    wfCtx := romancy.GetWorkflowContext(ctx)
//	    session := wfCtx.Session()
//	    if session != nil {
//	        // Execute custom SQL in the same transaction
//	        _, err := session.ExecContext(ctx, "INSERT INTO orders ...", input.OrderID)
//	        if err != nil {
//	            return OrderResult{}, err
//	        }
//	    }
//	    return OrderResult{Status: "completed"}, nil
//	})
func (c *WorkflowContext) Session() storage.Executor {
	if s := c.Storage(); s != nil {
		return s.Conn(c.ctx)
	}
	return nil
}

// Hooks returns the workflow hooks for observability.
// Returns nil if hooks are not available (e.g., not using replay engine).
func (c *WorkflowContext) Hooks() hooks.WorkflowHooks {
	if c.execCtx != nil {
		return c.execCtx.Hooks()
	}
	return nil
}

// SetApp sets the App reference for this workflow context.
// This is typically called internally when creating the context.
func (c *WorkflowContext) SetApp(app *App) {
	c.app = app
}

// App returns the App reference for this workflow context.
// Returns nil if not set.
func (c *WorkflowContext) App() *App {
	return c.app
}
