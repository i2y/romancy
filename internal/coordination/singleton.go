package coordination

import (
	"context"
	"time"
)

// SystemLockManager is the interface for system lock operations.
// This is a subset of storage.Storage that SingletonTaskRunner needs.
type SystemLockManager interface {
	TryAcquireSystemLock(ctx context.Context, lockName, workerID string, timeoutSec int) (bool, error)
	ReleaseSystemLock(ctx context.Context, lockName, workerID string) error
}

// SingletonTaskConfig configures a singleton task.
type SingletonTaskConfig struct {
	// TaskName is the unique name for this task (used as lock name).
	TaskName string

	// LockTimeout is how long the lock is held before expiring.
	// Should be longer than the expected task duration.
	// Default: 60 seconds
	LockTimeout time.Duration
}

// DefaultSingletonConfig returns the default configuration.
func DefaultSingletonConfig(taskName string) SingletonTaskConfig {
	return SingletonTaskConfig{
		TaskName:    taskName,
		LockTimeout: 60 * time.Second,
	}
}

// SingletonTaskRunner runs tasks as a singleton across the cluster.
// Only one worker can run the task at a time using system locks.
type SingletonTaskRunner struct {
	lockManager SystemLockManager
	workerID    string
	config      SingletonTaskConfig
}

// NewSingletonTaskRunner creates a new singleton task runner.
func NewSingletonTaskRunner(lockManager SystemLockManager, workerID string, config SingletonTaskConfig) *SingletonTaskRunner {
	if config.LockTimeout == 0 {
		config.LockTimeout = 60 * time.Second
	}
	return &SingletonTaskRunner{
		lockManager: lockManager,
		workerID:    workerID,
		config:      config,
	}
}

// TryRun attempts to acquire the lock and run the task.
// Returns (true, nil) if the task was executed successfully.
// Returns (false, nil) if the lock was not acquired (another worker is running).
// Returns (false, error) if there was an error acquiring lock or running task.
func (r *SingletonTaskRunner) TryRun(ctx context.Context, task func(context.Context) error) (bool, error) {
	// Try to acquire the system lock
	acquired, err := r.lockManager.TryAcquireSystemLock(
		ctx,
		r.config.TaskName,
		r.workerID,
		int(r.config.LockTimeout.Seconds()),
	)
	if err != nil {
		return false, err
	}
	if !acquired {
		// Another worker is running this task
		return false, nil
	}

	// Run the task
	taskErr := task(ctx)

	// Release the lock
	if err := r.lockManager.ReleaseSystemLock(ctx, r.config.TaskName, r.workerID); err != nil {
		// Log but don't fail - the lock will expire anyway
		_ = err
	}

	if taskErr != nil {
		return false, taskErr
	}

	return true, nil
}
