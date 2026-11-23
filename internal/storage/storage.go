package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrWorkflowNotCancellable indicates that the workflow cannot be cancelled.
// This happens when the workflow is already completed, cancelled, failed, or does not exist.
var ErrWorkflowNotCancellable = errors.New("workflow cannot be cancelled")

// ErrInstanceAlreadyExists indicates that CreateInstance found an instance
// with the same ID. Callers can use it for idempotent starts: starting the
// same instance ID twice results in a single instance.
var ErrInstanceAlreadyExists = errors.New("workflow instance already exists")

// ErrLockNotHeld is returned by the IfLocked operations and by RefreshLock
// when the worker no longer holds the instance's lock (it expired and another
// worker acquired it, or it was released). Writes guarded by it are fencing
// checks: they are refused instead of being applied over a lost lock.
var ErrLockNotHeld = errors.New("workflow instance lock is not held by this worker")

// ChannelClaimStaleSeconds bounds how long a competing-mode message claim may
// stay unconsumed before another instance may take it over. A claim normally
// lives only for the window between claiming and the workflow recording the
// receive result, so a claim older than this implies its owner died
// mid-consume. Aligned with the workflow lock lease duration.
const ChannelClaimStaleSeconds = 300

// NewLockIdentity returns a fresh lock-holder identity for one claim epoch
// ("workerID#epoch"). Store it in place of the plain workerID everywhere a
// lock operation expects a holder: every fenced write (`...IfLocked`) checks
// `locked_by = holder`, so a stale write issued after the instance was
// re-claimed under a newer epoch is rejected — even when the new holder is
// the same worker process (e.g. a run that lost its lease, or a restarted
// worker reusing the same workerID).
func NewLockIdentity(workerID string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s#%x", workerID, b[:])
}

// Executor is a database executor interface that can be either *sql.DB or *sql.Tx.
// This allows users to execute custom SQL queries within the same transaction
// as the workflow activity.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Storage defines the interface for workflow persistence.
// Implementations must be safe for concurrent use.
type Storage interface {
	// Initialize sets up the storage (create tables, etc.)
	Initialize(ctx context.Context) error

	// Close closes the storage connection
	Close() error

	// DB returns the underlying database connection.
	// This is primarily used for migrations.
	DB() *sql.DB

	// Transaction Management
	TransactionManager

	// Workflow Instance Operations
	InstanceManager

	// Locking Operations
	LockManager

	// History Operations
	HistoryManager

	// Timer Subscription Operations
	TimerSubscriptionManager

	// Outbox Operations
	OutboxManager

	// Compensation Operations
	CompensationManager

	// Channel Operations
	ChannelManager

	// Group Operations
	GroupManager

	// Actor Registry/Monitor Operations
	ActorRegistryManager

	// Capability Operations
	CapabilityManager

	BlobStore

	// System Lock Operations
	SystemLockManager

	// History Archive Operations
	HistoryArchiveManager
}

// TransactionManager handles transaction operations.
type TransactionManager interface {
	// BeginTransaction starts a new transaction.
	// Returns a context with the transaction attached.
	BeginTransaction(ctx context.Context) (context.Context, error)

	// CommitTransaction commits the current transaction.
	CommitTransaction(ctx context.Context) error

	// RollbackTransaction rolls back the current transaction.
	RollbackTransaction(ctx context.Context) error

	// InTransaction returns true if there is an active transaction.
	InTransaction(ctx context.Context) bool

	// Conn returns the database executor for the current context.
	// If a transaction is active, returns the transaction; otherwise, returns the database.
	// This allows users to execute custom SQL queries within the same transaction.
	Conn(ctx context.Context) Executor

	// RegisterPostCommitCallback registers a callback to be executed after a successful commit.
	// This is used for operations that should only happen if the transaction commits,
	// such as notifying waiting subscribers after a message is published.
	// Returns an error if not currently in a transaction.
	RegisterPostCommitCallback(ctx context.Context, cb func() error) error
}

// InstanceManager handles workflow instance CRUD operations.
type InstanceManager interface {
	// CreateInstance creates a new workflow instance.
	CreateInstance(ctx context.Context, instance *WorkflowInstance) error

	// GetInstance retrieves a workflow instance by ID.
	GetInstance(ctx context.Context, instanceID string) (*WorkflowInstance, error)

	// UpdateInstanceStatus updates the status and related fields.
	UpdateInstanceStatus(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg string) error

	// UpdateInstanceStatusIfLocked is UpdateInstanceStatus with a fencing
	// check: the update is applied only while workerID holds the instance's
	// lock, and ErrLockNotHeld is returned otherwise.
	UpdateInstanceStatusIfLocked(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg string, workerID string) error

	// UpdateInstanceActivity updates the current activity ID.
	UpdateInstanceActivity(ctx context.Context, instanceID string, activityID string) error

	// UpdateInstanceOutput updates the output data for a completed workflow.
	UpdateInstanceOutput(ctx context.Context, instanceID string, outputData []byte) error

	// UpdateInstanceOutputIfLocked is UpdateInstanceOutput with a fencing
	// check on workerID, as UpdateInstanceStatusIfLocked.
	UpdateInstanceOutputIfLocked(ctx context.Context, instanceID string, outputData []byte, workerID string) error

	// CancelInstance marks a workflow as cancelled.
	CancelInstance(ctx context.Context, instanceID string, reason string) error

	// ListInstances lists workflow instances with optional filtering and pagination.
	// Returns PaginationResult with cursor-based pagination.
	ListInstances(ctx context.Context, opts ListInstancesOptions) (*PaginationResult, error)

	// FindResumableWorkflows finds workflows that are ready to be resumed.
	// Returns workflows with status='running' that don't have an active lock.
	// These are typically workflows that had a message delivered and are waiting
	// for a worker to pick them up and resume execution.
	// workflowNames narrows the result to the named workflows: a worker asks
	// for only the workflows it can run, so that instances of other workflows
	// are left for other workers. nil (or empty) matches every workflow.
	// limit specifies the maximum number of workflows to return (0 = default 100).
	FindResumableWorkflows(ctx context.Context, workflowNames []string, limit int) ([]*ResumableWorkflow, error)
}

// ListInstancesOptions defines options for listing instances.
type ListInstancesOptions struct {
	// Pagination
	Limit     int    // Page size (default: 50)
	PageToken string // Cursor token for pagination (format: "ISO_DATETIME||INSTANCE_ID")

	// Filters
	StatusFilter       WorkflowStatus // Filter by status
	WorkflowNameFilter string         // Filter by workflow name (partial match, case-insensitive)
	InstanceIDFilter   string         // Filter by instance ID (partial match, case-insensitive)
	StartedAfter       *time.Time     // Filter instances started after this time
	StartedBefore      *time.Time     // Filter instances started before this time
	InputFilters       map[string]any // Filter by JSON paths in input data (exact match)

	// Deprecated: Use StatusFilter instead
	Status WorkflowStatus
	// Deprecated: Use WorkflowNameFilter instead
	WorkflowName string
	// Deprecated: Use PageToken instead
	Offset int
}

// LockManager handles distributed locking operations.
type LockManager interface {
	// TryAcquireLock attempts to acquire a lock on a workflow instance.
	// Returns true if the lock was acquired, false if already locked.
	TryAcquireLock(ctx context.Context, instanceID, workerID string, timeoutSec int) (bool, error)

	// ReleaseLock releases the lock on a workflow instance.
	ReleaseLock(ctx context.Context, instanceID, workerID string) error

	// RefreshLock extends the lock timeout. It answers ErrLockNotHeld when the
	// worker no longer holds the lock.
	RefreshLock(ctx context.Context, instanceID, workerID string, timeoutSec int) error

	// CleanupStaleLocks releases locks that have expired and returns
	// information about running workflows that need to be resumed.
	CleanupStaleLocks(ctx context.Context, timeoutSec int) ([]StaleWorkflowInfo, error)

	// ClaimResumableWorkflows atomically claims up to limit resumable
	// instances for workerID: one statement both selects claimable rows
	// (running, this worker's workflow names, unlocked or lease-expired) and
	// installs the lease, so concurrent workers take disjoint sets instead of
	// all racing to claim the same head rows that FindResumableWorkflows
	// returns. workflowNames must be non-empty.
	ClaimResumableWorkflows(ctx context.Context, workerID string, workflowNames []string, leaseSec, limit int) ([]*ResumableWorkflow, error)
}

// HistoryManager handles workflow execution history.
type HistoryManager interface {
	// AppendHistory adds a new history event.
	AppendHistory(ctx context.Context, event *HistoryEvent) error

	// AppendHistoryIfLocked is AppendHistory with a fencing check: the event
	// is appended only while workerID holds the instance's lock, and
	// ErrLockNotHeld is returned otherwise. A duplicate (instance_id,
	// activity_id) is a no-op as it is for AppendHistory.
	AppendHistoryIfLocked(ctx context.Context, event *HistoryEvent, workerID string) error

	// GetHistoryPaginated retrieves history events for an instance with pagination.
	// Returns events with id > afterID, up to limit events.
	// Returns (events, hasMore, error).
	GetHistoryPaginated(ctx context.Context, instanceID string, afterID int64, limit int) ([]*HistoryEvent, bool, error)

	// GetHistoryCount returns the total number of history events for an instance.
	GetHistoryCount(ctx context.Context, instanceID string) (int64, error)
}

// TimerSubscriptionManager handles timer subscriptions.
type TimerSubscriptionManager interface {
	// RegisterTimerSubscription registers a workflow to wait for a timer.
	RegisterTimerSubscription(ctx context.Context, sub *TimerSubscription) error

	// FindExpiredTimers finds timers that have expired.
	// limit specifies the maximum number of timers to return (0 = default 100).
	FindExpiredTimers(ctx context.Context, limit int) ([]*TimerSubscription, error)

	// RemoveTimerSubscription removes a timer subscription.
	RemoveTimerSubscription(ctx context.Context, instanceID, timerID string) error
}

// OutboxManager handles the transactional outbox.
type OutboxManager interface {
	// AddOutboxEvent adds an event to the outbox.
	AddOutboxEvent(ctx context.Context, event *OutboxEvent) error

	// GetPendingOutboxEvents retrieves pending events for sending.
	// Peek only: concurrent relayers must use ClaimOutboxEvents, which moves
	// the rows to 'processing' atomically.
	GetPendingOutboxEvents(ctx context.Context, limit int) ([]*OutboxEvent, error)

	// ClaimOutboxEvents atomically moves up to limit events to 'processing'
	// and returns them, so concurrent relayers take disjoint batches. It
	// claims 'pending' events and 'processing' events whose claim is older
	// than staleBefore (a crashed relayer's leftovers: at-least-once delivery
	// already tolerates the resulting rare duplicate). For 'processing' rows
	// published_at holds the claim time.
	ClaimOutboxEvents(ctx context.Context, staleBefore time.Time, limit int) ([]*OutboxEvent, error)

	// ReleaseOutboxEvent returns a claimed ('processing') event to 'pending'
	// for a later attempt, e.g. after a transient send failure.
	ReleaseOutboxEvent(ctx context.Context, eventID string) error

	// MarkOutboxEventSent marks an event as successfully sent.
	MarkOutboxEventSent(ctx context.Context, eventID string) error

	// MarkOutboxEventFailed marks an event as failed to send.
	MarkOutboxEventFailed(ctx context.Context, eventID string) error

	// IncrementOutboxAttempts increments the attempt count for an event.
	IncrementOutboxAttempts(ctx context.Context, eventID string) error

	// CleanupOldOutboxEvents removes old sent events.
	CleanupOldOutboxEvents(ctx context.Context, olderThan time.Duration) error
}

// CompensationManager handles saga compensation.
type CompensationManager interface {
	// AddCompensation registers a compensation action.
	AddCompensation(ctx context.Context, entry *CompensationEntry) error

	// GetCompensations retrieves compensation entries in LIFO order (by created_at DESC).
	// Status tracking is done via history events (CompensationExecuted, CompensationFailed).
	GetCompensations(ctx context.Context, instanceID string) ([]*CompensationEntry, error)
}

// ========================================
// Channel Manager
// ========================================

// ChannelManager handles channel-based messaging operations.
type ChannelManager interface {
	// PublishToChannel publishes a message to a channel.
	// For direct messages, use dynamic channel names (e.g., "channel:instance_id").
	PublishToChannel(ctx context.Context, channelName string, dataJSON []byte, metadata []byte) (int64, error)

	// SubscribeToChannel subscribes an instance to a channel.
	SubscribeToChannel(ctx context.Context, instanceID, channelName string, mode ChannelMode) error

	// UnsubscribeFromChannel unsubscribes an instance from a channel.
	UnsubscribeFromChannel(ctx context.Context, instanceID, channelName string) error

	// GetChannelSubscription retrieves a subscription for an instance and channel.
	GetChannelSubscription(ctx context.Context, instanceID, channelName string) (*ChannelSubscription, error)

	// GetChannelMode retrieves the mode for a channel (from any existing subscription).
	// Returns empty string if no subscriptions exist.
	GetChannelMode(ctx context.Context, channelName string) (ChannelMode, error)

	// RegisterChannelReceiveAndReleaseLock atomically registers a channel receive wait
	// and releases the workflow lock, in a single transaction. It re-checks pending
	// messages inside that transaction: a message committed between the caller's
	// earlier pending check and this call must not leave the instance waiting
	// forever. When a pending message exists it does not register the wait; it
	// only releases the lock with the instance still 'running' so a resumption
	// drains the message, and returns registered=false.
	RegisterChannelReceiveAndReleaseLock(
		ctx context.Context,
		instanceID, channelName, workerID, activityID string,
		timeoutAt *time.Time,
	) (registered bool, err error)

	// RequeueChannelWaitsWithPending flips instances back to 'running' when they
	// are waiting for a channel message that has already arrived. This is the
	// backstop for the publish/registration race the registration transaction
	// cannot cover (a publisher's waiter scan that ran between the pending check
	// and the commit): it bounds the wakeup delay to one sweep interval and also
	// heals waits stranded by notification loss. Returns the number requeued.
	RequeueChannelWaitsWithPending(ctx context.Context) (int, error)

	// GetPendingChannelMessages retrieves pending messages for a channel after a given ID.
	GetPendingChannelMessages(ctx context.Context, channelName string, afterID int64, limit int) ([]*ChannelMessage, error)

	// GetPendingChannelMessagesForInstance gets pending messages for a specific subscriber.
	// For broadcast mode: Returns messages with id > cursor (messages not yet seen by this instance)
	// For competing mode: Returns unclaimed messages (not yet claimed by any instance)
	GetPendingChannelMessagesForInstance(ctx context.Context, instanceID, channelName string) ([]*ChannelMessage, error)

	// ClaimChannelMessage claims a channel_messages row (identified by its
	// numeric id) for competing mode. The claim is keyed on the row's
	// message_id UUID, matching channel_message_claims's foreign key. It is
	// idempotent for the same instance and allows takeover of claims whose
	// owner is in a terminal status or whose claimed_at is older than
	// ChannelClaimStaleSeconds (crash between claim and history record).
	// Returns false only when another live instance holds a fresh claim.
	ClaimChannelMessage(ctx context.Context, messageID int64, instanceID string) (bool, error)

	// DeleteChannelMessage deletes a message from the channel.
	DeleteChannelMessage(ctx context.Context, messageID int64) error

	// UpdateDeliveryCursor updates the delivery cursor for broadcast mode.
	UpdateDeliveryCursor(ctx context.Context, instanceID, channelName string, lastMessageID int64) error

	// GetDeliveryCursor gets the current delivery cursor for an instance and channel.
	GetDeliveryCursor(ctx context.Context, instanceID, channelName string) (int64, error)

	// GetChannelSubscribersWaiting finds subscribers waiting for messages on a channel.
	GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*ChannelSubscription, error)

	// ClearChannelWaitingState clears the waiting state for an instance's channel subscription.
	ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error

	// DeliverChannelMessage delivers a message to a waiting subscriber (records in history).
	DeliverChannelMessage(ctx context.Context, instanceID string, message *ChannelMessage) error

	// DeliverChannelMessageWithLock delivers a message using Lock-First pattern.
	// This is used for load balancing in multi-worker environments:
	// 1. Try to acquire lock on the target instance
	// 2. If lock fails, return (nil, nil) - another worker will handle it
	// 3. Record message in history
	// 4. Update status to 'running'
	// 5. Release lock
	// Returns the delivery info if successful, nil if lock was not acquired.
	DeliverChannelMessageWithLock(
		ctx context.Context,
		instanceID string,
		channelName string,
		message *ChannelMessage,
		workerID string,
		lockTimeoutSec int,
	) (*ChannelDeliveryResult, error)

	// CleanupOldChannelMessages removes old channel messages.
	CleanupOldChannelMessages(ctx context.Context, olderThan time.Duration) error

	// FindExpiredChannelSubscriptions finds channel subscriptions that have timed out.
	// limit specifies the maximum number of subscriptions to return (0 = default 100).
	FindExpiredChannelSubscriptions(ctx context.Context, limit int) ([]*ChannelSubscription, error)
}

// ========================================
// Group Manager
// ========================================

// GroupManager handles Erlang pg-style group membership operations.
type GroupManager interface {
	// JoinGroup adds an instance to a group.
	JoinGroup(ctx context.Context, instanceID, groupName string) error

	// LeaveGroup removes an instance from a group.
	LeaveGroup(ctx context.Context, instanceID, groupName string) error

	// GetGroupMembers retrieves all instance IDs in a group.
	GetGroupMembers(ctx context.Context, groupName string) ([]string, error)

	// LeaveAllGroups removes an instance from all groups.
	LeaveAllGroups(ctx context.Context, instanceID string) error
}

// ========================================
// Actor Registry Manager
// ========================================

// ActorRegistryManager provides Erlang-style actor naming and monitoring:
// register/whereis for name lookup, and monitor/emit-down for termination
// notification (Erlang 'DOWN' messages delivered over a channel).
type ActorRegistryManager interface {
	// RegisterActorName binds a unique name to an instance. Returns an error
	// if the name is already taken by a different (live) instance.
	RegisterActorName(ctx context.Context, name, instanceID, framework string) error

	// UnregisterActorName removes a name binding (idempotent).
	UnregisterActorName(ctx context.Context, name string) error

	// WhereisActor resolves a name to an instance ID. Returns "" if unbound.
	WhereisActor(ctx context.Context, name string) (string, error)

	// MonitorActor registers that watcherInstanceID be notified on channel
	// (typically its direct channel, "channel:{watcher}") when targetInstanceID
	// reaches a terminal status. Idempotent per (watcher, target).
	MonitorActor(ctx context.Context, watcherInstanceID, targetInstanceID, channel string) error

	// UnmonitorActor removes a monitor registration (idempotent).
	UnmonitorActor(ctx context.Context, watcherInstanceID, targetInstanceID string) error

	// EmitActorDownForTerminated publishes a DOWN message to each monitor's
	// channel for every target already in a terminal status, then removes the
	// monitor rows and the target's actor_names bindings — all in one
	// transaction so it is safe to run repeatedly (leader sweep).
	// Returns the number of DOWN notifications emitted.
	EmitActorDownForTerminated(ctx context.Context) (int, error)
}

// ========================================
// Capability Manager
// ========================================

// Capability kinds for the actor permission model.
const (
	CapabilityKindActor      = "actor"       // operate on an actor (send/cancel/inspect)
	CapabilityKindChannelPub = "channel_pub" // publish to a channel
	CapabilityKindChannelSub = "channel_sub" // receive/drain a channel
	CapabilityKindFSWrite    = "fs_write"    // write to a shared-FS path
)

// ActorCapability is a stored capability grant. Only the secret's hash is
// persisted — the plaintext secret leaves the system in the issued token.
type ActorCapability struct {
	ID         string
	Kind       string
	Target     string
	SecretHash string
	GrantedBy  string
	ExpiresAt  *time.Time
}

// CapabilityManager manages capability grants: issue (store hash), check
// (hash+expiry lookup), and revoke.
type CapabilityManager interface {
	// GrantCapability stores a capability grant.
	GrantCapability(ctx context.Context, cap *ActorCapability) error

	// RevokeCapability removes a grant by its capability ID (idempotent).
	RevokeCapability(ctx context.Context, capabilityID string) error

	// CheckCapability verifies a presented secret for (kind, target) against
	// the stored hash and expiry. Returns false for unknown/expired grants.
	CheckCapability(ctx context.Context, kind, target, secretHash string) (bool, error)
}

// BlobStore is a content-addressed store for large payloads. The key is
// the content hash (e.g. sha256 hex); writes are idempotent.
type BlobStore interface {
	// PutBlob stores data under its content hash (idempotent).
	PutBlob(ctx context.Context, hash string, data []byte, createdBy string) error

	// GetBlob returns the data stored under hash, or nil if absent.
	GetBlob(ctx context.Context, hash string) ([]byte, error)
}

// ========================================
// System Lock Manager
// ========================================

// SystemLockManager handles system-level locks for background tasks.
type SystemLockManager interface {
	// TryAcquireSystemLock attempts to acquire a system lock.
	// Returns true if the lock was acquired.
	TryAcquireSystemLock(ctx context.Context, lockName, workerID string, timeoutSec int) (bool, error)

	// ReleaseSystemLock releases a system lock.
	ReleaseSystemLock(ctx context.Context, lockName, workerID string) error

	// CleanupExpiredSystemLocks removes expired system locks.
	CleanupExpiredSystemLocks(ctx context.Context) error
}

// ========================================
// History Archive Manager
// ========================================

// HistoryArchiveManager handles history archival for the recur pattern.
type HistoryArchiveManager interface {
	// ArchiveHistory moves all history events for an instance to the archive table.
	ArchiveHistory(ctx context.Context, instanceID string) error

	// GetArchivedHistory retrieves archived history events for an instance.
	GetArchivedHistory(ctx context.Context, instanceID string) ([]*ArchivedHistoryEvent, error)

	// CleanupInstanceSubscriptions removes all subscriptions for an instance.
	// This is used during recur to clean up stale subscriptions.
	CleanupInstanceSubscriptions(ctx context.Context, instanceID string) error
}
