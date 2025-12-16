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
	Framework         string         `json:"framework"` // "go" or "python" (Edda unification)
	Status            WorkflowStatus `json:"status"`
	InputData         []byte         `json:"input_data"`          // JSON-encoded input
	OutputData        []byte         `json:"output_data"`         // JSON-encoded output (if completed)
	CurrentActivityID string         `json:"current_activity_id"` // Last executed activity ID
	SourceCode        string         `json:"source_code"`         // Source code snapshot (optional)
	SourceHash        string         `json:"source_hash"`         // Source code hash (Edda compatibility)
	OwnerService      string         `json:"owner_service"`       // Service that owns this workflow (Edda compatibility)
	ContinuedFrom     string         `json:"continued_from"`      // Previous instance ID (for recur)
	LockedBy          string         `json:"locked_by"`           // Worker ID holding the lock
	LockedAt          *time.Time     `json:"locked_at"`           // When the lock was acquired
	LockTimeoutSec    *int           `json:"lock_timeout_seconds"`
	LockExpiresAt     *time.Time     `json:"lock_expires_at"`
	StartedAt         time.Time      `json:"started_at"` // When the workflow was created (unified with Edda)
	UpdatedAt         time.Time      `json:"updated_at"`
}

// HistoryEventType represents the type of history event.
type HistoryEventType string

const (
	HistoryActivityStarted      HistoryEventType = "activity_started"
	HistoryActivityCompleted    HistoryEventType = "activity_completed"
	HistoryActivityFailed       HistoryEventType = "activity_failed"
	HistoryEventReceived        HistoryEventType = "event_received"
	HistoryTimerFired           HistoryEventType = "timer_fired"
	HistoryCompensationAdded    HistoryEventType = "compensation_added"
	HistoryCompensationExecuted HistoryEventType = "compensation_executed"
	HistoryCompensationFailed   HistoryEventType = "compensation_failed"
	HistoryWorkflowFailed       HistoryEventType = "workflow_failed"
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
	ActivityID string    `json:"activity_id"` // Activity ID for replay matching (Edda compatibility)
	Step       int       `json:"step"`        // Activity step for replay
	CreatedAt  time.Time `json:"created_at"`
}

// OutboxEvent represents an event in the transactional outbox.
type OutboxEvent struct {
	ID              int64      `json:"id"`
	EventID         string     `json:"event_id"`          // CloudEvents ID
	EventType       string     `json:"event_type"`        // CloudEvents type
	EventSource     string     `json:"event_source"`      // CloudEvents source
	EventData       []byte     `json:"event_data"`        // JSON payload
	EventDataBinary []byte     `json:"event_data_binary"` // Binary payload (Edda compatibility)
	DataType        string     `json:"data_type"`         // "json" or "binary"
	ContentType     string     `json:"content_type"`      // e.g., "application/json"
	Status          string     `json:"status"`            // "pending", "sent", "failed"
	RetryCount      int        `json:"retry_count"`       // Number of retry attempts (unified with Edda)
	LastError       string     `json:"last_error"`        // Last error message (Edda compatibility)
	PublishedAt     *time.Time `json:"published_at"`      // When the event was published (Edda compatibility)
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// CompensationEntry represents a registered compensation action.
// Order is determined by created_at DESC (LIFO).
// Status is tracked via history events (CompensationExecuted, CompensationFailed).
type CompensationEntry struct {
	ID           int64     `json:"id"`
	InstanceID   string    `json:"instance_id"`
	ActivityID   string    `json:"activity_id"`
	ActivityName string    `json:"activity_name"` // Function name (unified with Edda)
	Args         []byte    `json:"args"`          // JSON-encoded arguments (unified with Edda)
	CreatedAt    time.Time `json:"created_at"`
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
// SendTo uses dynamic channel names (e.g., "channel:instance_id") instead of target_instance_id.
type ChannelMessage struct {
	ID          int64     `json:"id"`
	Channel     string    `json:"channel"`               // Channel name (unified with Edda)
	MessageID   string    `json:"message_id"`            // UUID for message (Edda compatibility)
	DataType    string    `json:"data_type"`             // "json" or "binary" (Edda compatibility)
	Data        []byte    `json:"data,omitempty"`        // JSON data (unified with Edda)
	DataBinary  []byte    `json:"data_binary,omitempty"` // Binary data
	Metadata    []byte    `json:"metadata,omitempty"`    // JSON-encoded metadata
	PublishedAt time.Time `json:"published_at"`          // When the message was published (unified with Edda)
}

// ChannelSubscription represents a workflow's subscription to a channel.
// Waiting state is determined by "activity_id IS NOT NULL" instead of explicit waiting flag.
type ChannelSubscription struct {
	ID              int64       `json:"id"`
	InstanceID      string      `json:"instance_id"`
	Channel         string      `json:"channel"` // Channel name (unified with Edda)
	Mode            ChannelMode `json:"mode"`
	CursorMessageID int64       `json:"cursor_message_id"` // Last delivered message ID (Edda compatibility)
	TimeoutAt       *time.Time  `json:"timeout_at,omitempty"`
	ActivityID      string      `json:"activity_id,omitempty"` // Activity ID for replay matching; non-null = waiting
	SubscribedAt    time.Time   `json:"subscribed_at"`         // When the subscription was created (unified with Edda)
}

// ChannelDeliveryCursor tracks message delivery progress for broadcast mode.
type ChannelDeliveryCursor struct {
	ID              int64     `json:"id"`
	InstanceID      string    `json:"instance_id"`
	Channel         string    `json:"channel"`           // Channel name (unified with Edda)
	LastDeliveredID int64     `json:"last_delivered_id"` // Last delivered message ID (unified with Edda)
	UpdatedAt       time.Time `json:"updated_at"`
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
	JoinedAt   time.Time `json:"joined_at"` // When the instance joined the group (unified with Edda)
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
