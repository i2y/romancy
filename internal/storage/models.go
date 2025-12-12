// Package storage provides the storage layer for Romancy.
package storage

import (
	"time"
)

// WorkflowStatus represents the current state of a workflow instance.
type WorkflowStatus string

const (
	StatusPending           WorkflowStatus = "pending"
	StatusRunning           WorkflowStatus = "running"
	StatusCompleted         WorkflowStatus = "completed"
	StatusFailed            WorkflowStatus = "failed"
	StatusCancelled         WorkflowStatus = "cancelled"
	StatusWaitingForEvent   WorkflowStatus = "waiting_for_event"
	StatusWaitingForTimer   WorkflowStatus = "waiting_for_timer"
	StatusWaitingForMessage WorkflowStatus = "waiting_for_message"
	StatusRecurred          WorkflowStatus = "recurred"
	StatusCompensating      WorkflowStatus = "compensating"
)

// WorkflowInstance represents a single execution of a workflow.
type WorkflowInstance struct {
	InstanceID        string         `json:"instance_id"`
	WorkflowName      string         `json:"workflow_name"`
	Status            WorkflowStatus `json:"status"`
	InputData         []byte         `json:"input_data"`          // JSON-encoded input
	OutputData        []byte         `json:"output_data"`         // JSON-encoded output (if completed)
	ErrorMessage      string         `json:"error_message"`       // Error message (if failed)
	CurrentActivityID string         `json:"current_activity_id"` // Last executed activity ID
	SourceCode        string         `json:"source_code"`         // Source code snapshot (optional)
	ContinuedFrom     string         `json:"continued_from"`      // Previous instance ID (for recur)
	LockedBy          string         `json:"locked_by"`           // Worker ID holding the lock
	LockedAt          *time.Time     `json:"locked_at"`           // When the lock was acquired
	LockTimeoutSec    *int           `json:"lock_timeout_seconds"`
	LockExpiresAt     *time.Time     `json:"lock_expires_at"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// HistoryEventType represents the type of history event.
type HistoryEventType string

const (
	HistoryActivityStarted   HistoryEventType = "activity_started"
	HistoryActivityCompleted HistoryEventType = "activity_completed"
	HistoryActivityFailed    HistoryEventType = "activity_failed"
	HistoryEventReceived     HistoryEventType = "event_received"
	HistoryTimerFired        HistoryEventType = "timer_fired"
	HistoryCompensationAdded HistoryEventType = "compensation_added"
)

// HistoryEvent represents a single event in a workflow's execution history.
type HistoryEvent struct {
	ID              int64            `json:"id"`
	InstanceID      string           `json:"instance_id"`
	ActivityID      string           `json:"activity_id"`
	EventType       HistoryEventType `json:"event_type"`
	EventData       []byte           `json:"event_data"`        // JSON-encoded data
	EventDataBinary []byte           `json:"event_data_binary"` // Binary data (alternative)
	DataType        string           `json:"data_type"`         // "json" or "binary"
	CreatedAt       time.Time        `json:"created_at"`
}

// TimerSubscription represents a workflow waiting for a timer.
type TimerSubscription struct {
	ID         int64     `json:"id"`
	InstanceID string    `json:"instance_id"`
	TimerID    string    `json:"timer_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	Step       int       `json:"step"` // Activity step for replay
	CreatedAt  time.Time `json:"created_at"`
}

// OutboxEvent represents an event in the transactional outbox.
type OutboxEvent struct {
	ID          int64     `json:"id"`
	EventID     string    `json:"event_id"`     // CloudEvents ID
	EventType   string    `json:"event_type"`   // CloudEvents type
	EventSource string    `json:"event_source"` // CloudEvents source
	EventData   []byte    `json:"event_data"`   // JSON or binary payload
	DataType    string    `json:"data_type"`    // "json" or "binary"
	ContentType string    `json:"content_type"` // e.g., "application/json"
	Status      string    `json:"status"`       // "pending", "sent", "failed"
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CompensationEntry represents a registered compensation action.
type CompensationEntry struct {
	ID              int64     `json:"id"`
	InstanceID      string    `json:"instance_id"`
	ActivityID      string    `json:"activity_id"`
	CompensationFn  string    `json:"compensation_fn"`  // Function name
	CompensationArg []byte    `json:"compensation_arg"` // JSON-encoded arguments
	Order           int       `json:"order"`            // LIFO order (higher = execute first)
	Status          string    `json:"status"`           // "pending", "executed", "failed"
	CreatedAt       time.Time `json:"created_at"`
}

// StaleWorkflowInfo contains information about a workflow with a stale lock.
type StaleWorkflowInfo struct {
	InstanceID   string
	WorkflowName string
}

// ResumableWorkflow contains information about a workflow ready to be resumed.
// These are workflows with status='running' that don't have an active lock.
type ResumableWorkflow struct {
	InstanceID   string
	WorkflowName string
}

// ========================================
// Pagination
// ========================================

// PaginationResult represents a paginated list of workflow instances.
type PaginationResult struct {
	Instances     []*WorkflowInstance `json:"instances"`
	NextPageToken string              `json:"next_page_token,omitempty"`
	HasMore       bool                `json:"has_more"`
}

// ========================================
// Channels
// ========================================

// ChannelMode represents the subscription mode for a channel.
type ChannelMode string

const (
	ChannelModeBroadcast ChannelMode = "broadcast" // All subscribers receive all messages
	ChannelModeCompeting ChannelMode = "competing" // Each message goes to one subscriber
)

// ChannelMessage represents a message in a channel.
type ChannelMessage struct {
	ID               int64     `json:"id"`
	ChannelName      string    `json:"channel_name"`
	DataJSON         []byte    `json:"data_json,omitempty"`
	DataBinary       []byte    `json:"data_binary,omitempty"`
	Metadata         []byte    `json:"metadata,omitempty"` // JSON-encoded metadata
	TargetInstanceID string    `json:"target_instance_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ChannelSubscription represents a workflow's subscription to a channel.
type ChannelSubscription struct {
	ID          int64       `json:"id"`
	InstanceID  string      `json:"instance_id"`
	ChannelName string      `json:"channel_name"`
	Mode        ChannelMode `json:"mode"`
	Waiting     bool        `json:"waiting"`
	TimeoutAt   *time.Time  `json:"timeout_at,omitempty"`
	ActivityID  string      `json:"activity_id,omitempty"` // Activity ID for replay matching
	CreatedAt   time.Time   `json:"created_at"`
}

// ChannelDeliveryCursor tracks message delivery progress for broadcast mode.
type ChannelDeliveryCursor struct {
	ID            int64     `json:"id"`
	InstanceID    string    `json:"instance_id"`
	ChannelName   string    `json:"channel_name"`
	LastMessageID int64     `json:"last_message_id"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ChannelMessageClaim tracks which instance claimed a message in competing mode.
type ChannelMessageClaim struct {
	ID         int64     `json:"id"`
	MessageID  int64     `json:"message_id"`
	InstanceID string    `json:"instance_id"`
	ClaimedAt  time.Time `json:"claimed_at"`
}

// ChannelDeliveryResult contains information about a successful message delivery.
type ChannelDeliveryResult struct {
	InstanceID   string `json:"instance_id"`
	WorkflowName string `json:"workflow_name"`
	ActivityID   string `json:"activity_id"`
}

// ========================================
// History Archive (for Recur)
// ========================================

// ArchivedHistoryEvent represents an archived history event (from recur).
type ArchivedHistoryEvent struct {
	ID                int64            `json:"id"`
	OriginalID        int64            `json:"original_id"`
	InstanceID        string           `json:"instance_id"`
	ActivityID        string           `json:"activity_id"`
	EventType         HistoryEventType `json:"event_type"`
	EventData         []byte           `json:"event_data"`
	EventDataBinary   []byte           `json:"event_data_binary"`
	DataType          string           `json:"data_type"`
	OriginalCreatedAt time.Time        `json:"original_created_at"`
	ArchivedAt        time.Time        `json:"archived_at"`
}

// ========================================
// Group Memberships
// ========================================

// GroupMembership represents a workflow's membership in a group.
type GroupMembership struct {
	ID         int64     `json:"id"`
	InstanceID string    `json:"instance_id"`
	GroupName  string    `json:"group_name"`
	CreatedAt  time.Time `json:"created_at"`
}

// ========================================
// System Locks
// ========================================

// SystemLock represents a system-level lock for background tasks.
type SystemLock struct {
	LockName  string    `json:"lock_name"`
	LockedBy  string    `json:"locked_by"`
	LockedAt  time.Time `json:"locked_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
