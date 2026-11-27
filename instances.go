package romancy

import "github.com/i2y/romancy/internal/storage"

// WorkflowInstance is a stored workflow instance, as returned by
// App.GetInstance and App.FindInstances.
type WorkflowInstance = storage.WorkflowInstance

// WorkflowStatus is the status of a workflow instance.
type WorkflowStatus = storage.WorkflowStatus

// The statuses of a workflow instance.
const (
	StatusRunning           = storage.StatusRunning
	StatusCompleted         = storage.StatusCompleted
	StatusFailed            = storage.StatusFailed
	StatusCancelled         = storage.StatusCancelled
	StatusWaitingForEvent   = storage.StatusWaitingForEvent
	StatusWaitingForTimer   = storage.StatusWaitingForTimer
	StatusWaitingForMessage = storage.StatusWaitingForMessage
	StatusRecurred          = storage.StatusRecurred
	StatusCompensating      = storage.StatusCompensating
)

// ListInstancesOptions filters and pages the instances that
// App.FindInstancesWithOptions returns.
type ListInstancesOptions = storage.ListInstancesOptions
