// Package storage provides the storage layer for Romancy.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/i2y/romancy/internal/migrations"
)

// txKey is the context key for transactions.
type txKey struct{}

// txState holds transaction state including post-commit callbacks.
type txState struct {
	tx        *sql.Tx
	callbacks []func() error
}

// SQLiteStorage implements the Storage interface using SQLite.
type SQLiteStorage struct {
	db     *sql.DB
	driver Driver
}

// NewSQLiteStorage creates a new SQLite storage.
func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return &SQLiteStorage{
		db:     db,
		driver: &SQLiteDriver{},
	}, nil
}

// Initialize creates the database schema using migrations.
func (s *SQLiteStorage) Initialize(ctx context.Context) error {
	migrator := migrations.NewMigrator(s.db, migrations.DriverSQLite)
	return migrator.Up()
}

// DB returns the underlying database connection.
func (s *SQLiteStorage) DB() *sql.DB {
	return s.db
}

// Close closes the database connection.
func (s *SQLiteStorage) Close() error {
	return s.db.Close()
}

// getConn returns the appropriate database handle based on context (internal use).
func (s *SQLiteStorage) getConn(ctx context.Context) Executor {
	if state, ok := ctx.Value(txKey{}).(*txState); ok {
		return state.tx
	}
	return s.db
}

// --- Transaction Manager ---

// BeginTransaction starts a new transaction.
func (s *SQLiteStorage) BeginTransaction(ctx context.Context) (context.Context, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ctx, err
	}
	state := &txState{tx: tx}
	return context.WithValue(ctx, txKey{}, state), nil
}

// CommitTransaction commits the current transaction.
func (s *SQLiteStorage) CommitTransaction(ctx context.Context) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return fmt.Errorf("no transaction in context")
	}

	// First commit the transaction
	if err := state.tx.Commit(); err != nil {
		return err
	}

	// Execute callbacks AFTER successful commit
	for _, cb := range state.callbacks {
		if err := cb(); err != nil {
			// Log error but don't fail - commit already succeeded
			slog.Debug("post-commit callback error", "error", err)
		}
	}

	return nil
}

// RollbackTransaction rolls back the current transaction.
// Callbacks are not executed on rollback.
func (s *SQLiteStorage) RollbackTransaction(ctx context.Context) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return nil // No transaction to rollback
	}
	return state.tx.Rollback()
}

// InTransaction returns whether a transaction is in progress.
func (s *SQLiteStorage) InTransaction(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{}).(*txState)
	return ok
}

// Conn returns the database executor for the current context.
// If a transaction is active, returns the transaction; otherwise, returns the database.
// This allows users to execute custom SQL queries within the same transaction
// as the workflow activity when using transactional activities.
//
// Example:
//
//	conn := ctx.Storage().Conn(ctx.Context())
//	_, err := conn.ExecContext(ctx.Context(), "INSERT INTO orders ...", ...)
func (s *SQLiteStorage) Conn(ctx context.Context) Executor {
	return s.getConn(ctx)
}

// RegisterPostCommitCallback registers a callback to be executed after a successful commit.
// Returns an error if not currently in a transaction.
func (s *SQLiteStorage) RegisterPostCommitCallback(ctx context.Context, cb func() error) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return fmt.Errorf("not in a transaction")
	}
	state.callbacks = append(state.callbacks, cb)
	return nil
}

// --- Instance Manager ---

// CreateInstance creates a new workflow instance.
func (s *SQLiteStorage) CreateInstance(ctx context.Context, instance *WorkflowInstance) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			instance_id, workflow_name, status, input_data, source_code, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, instance.InstanceID, instance.WorkflowName, instance.Status,
		string(instance.InputData), instance.SourceCode,
		instance.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		instance.UpdatedAt.UTC().Format("2006-01-02 15:04:05"))
	return err
}

// GetInstance retrieves a workflow instance by ID.
func (s *SQLiteStorage) GetInstance(ctx context.Context, instanceID string) (*WorkflowInstance, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, status, input_data, output_data,
			   error_message, current_activity_id, source_code,
			   locked_by, locked_at, lock_timeout_seconds, lock_expires_at,
			   created_at, updated_at
		FROM workflow_instances WHERE instance_id = ?
	`, instanceID)

	var inst WorkflowInstance
	var inputData, outputData, errorMsg, activityID, sourceCode sql.NullString
	var lockedBy sql.NullString
	var lockedAt, lockExpiresAt sql.NullTime
	var lockTimeout sql.NullInt64

	err := row.Scan(
		&inst.InstanceID, &inst.WorkflowName, &inst.Status,
		&inputData, &outputData, &errorMsg, &activityID, &sourceCode,
		&lockedBy, &lockedAt, &lockTimeout, &lockExpiresAt,
		&inst.CreatedAt, &inst.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if inputData.Valid {
		inst.InputData = []byte(inputData.String)
	}
	if outputData.Valid {
		inst.OutputData = []byte(outputData.String)
	}
	if errorMsg.Valid {
		inst.ErrorMessage = errorMsg.String
	}
	if activityID.Valid {
		inst.CurrentActivityID = activityID.String
	}
	if sourceCode.Valid {
		inst.SourceCode = sourceCode.String
	}
	if lockedBy.Valid {
		inst.LockedBy = lockedBy.String
	}
	if lockedAt.Valid {
		inst.LockedAt = &lockedAt.Time
	}
	if lockTimeout.Valid {
		timeout := int(lockTimeout.Int64)
		inst.LockTimeoutSec = &timeout
	}
	if lockExpiresAt.Valid {
		inst.LockExpiresAt = &lockExpiresAt.Time
	}

	return &inst, nil
}

// UpdateInstanceStatus updates the status of a workflow instance.
func (s *SQLiteStorage) UpdateInstanceStatus(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, error_message = ?, updated_at = datetime('now')
		WHERE instance_id = ?
	`, status, errorMsg, instanceID)
	return err
}

// UpdateInstanceActivity updates the current activity ID.
func (s *SQLiteStorage) UpdateInstanceActivity(ctx context.Context, instanceID, activityID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET current_activity_id = ?, updated_at = datetime('now')
		WHERE instance_id = ?
	`, activityID, instanceID)
	return err
}

// UpdateInstanceOutput updates the output data.
func (s *SQLiteStorage) UpdateInstanceOutput(ctx context.Context, instanceID string, outputData []byte) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET output_data = ?, status = 'completed', updated_at = datetime('now')
		WHERE instance_id = ?
	`, string(outputData), instanceID)
	return err
}

// CancelInstance cancels a workflow instance.
// Returns ErrWorkflowNotCancellable if the workflow is already completed, cancelled, failed, or does not exist.
func (s *SQLiteStorage) CancelInstance(ctx context.Context, instanceID, reason string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'cancelled', error_message = ?, updated_at = datetime('now')
		WHERE instance_id = ? AND status IN ('pending', 'running', 'waiting_for_event', 'waiting_for_timer', 'waiting_for_message', 'recurred')
	`, reason, instanceID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrWorkflowNotCancellable
	}
	return nil
}

// ListInstances lists workflow instances with cursor-based pagination and filters.
func (s *SQLiteStorage) ListInstances(ctx context.Context, opts ListInstancesOptions) (*PaginationResult, error) {
	conn := s.getConn(ctx)
	query := "SELECT instance_id, workflow_name, status, created_at, updated_at FROM workflow_instances WHERE 1=1"
	args := []any{}

	// Handle both new and deprecated filter options
	workflowFilter := opts.WorkflowNameFilter
	if workflowFilter == "" {
		workflowFilter = opts.WorkflowName // Deprecated field
	}
	statusFilter := opts.StatusFilter
	if statusFilter == "" {
		statusFilter = opts.Status // Deprecated field
	}

	// Apply filters
	if workflowFilter != "" {
		query += " AND LOWER(workflow_name) LIKE LOWER(?)"
		args = append(args, "%"+workflowFilter+"%")
	}
	if statusFilter != "" {
		query += " AND status = ?"
		args = append(args, statusFilter)
	}
	if opts.InstanceIDFilter != "" {
		query += " AND LOWER(instance_id) LIKE LOWER(?)"
		args = append(args, "%"+opts.InstanceIDFilter+"%")
	}
	if opts.StartedAfter != nil {
		query += " AND datetime(created_at) > datetime(?)"
		args = append(args, opts.StartedAfter.UTC().Format("2006-01-02 15:04:05"))
	}
	if opts.StartedBefore != nil {
		query += " AND datetime(created_at) < datetime(?)"
		args = append(args, opts.StartedBefore.UTC().Format("2006-01-02 15:04:05"))
	}

	// Parse cursor token (format: "ISO_DATETIME||INSTANCE_ID")
	if opts.PageToken != "" {
		parts := strings.SplitN(opts.PageToken, "||", 2)
		if len(parts) == 2 {
			cursorTime, err := time.Parse(time.RFC3339Nano, parts[0])
			if err == nil {
				cursorID := parts[1]
				// For descending order: get rows where (created_at, instance_id) < (cursor_time, cursor_id)
				query += " AND (datetime(created_at) < datetime(?) OR (datetime(created_at) = datetime(?) AND instance_id < ?))"
				cursorTimeStr := cursorTime.UTC().Format("2006-01-02 15:04:05")
				args = append(args, cursorTimeStr, cursorTimeStr, cursorID)
			}
		}
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50 // Default page size
	}
	// Fetch one extra to determine if there are more pages
	query += " ORDER BY created_at DESC, instance_id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var instances []*WorkflowInstance
	for rows.Next() {
		var inst WorkflowInstance
		if err := rows.Scan(&inst.InstanceID, &inst.WorkflowName, &inst.Status, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		instances = append(instances, &inst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Determine pagination
	hasMore := len(instances) > limit
	if hasMore {
		instances = instances[:limit] // Remove the extra row
	}

	var nextPageToken string
	if hasMore && len(instances) > 0 {
		lastInst := instances[len(instances)-1]
		nextPageToken = lastInst.CreatedAt.UTC().Format(time.RFC3339Nano) + "||" + lastInst.InstanceID
	}

	return &PaginationResult{
		Instances:     instances,
		NextPageToken: nextPageToken,
		HasMore:       hasMore,
	}, nil
}

// FindResumableWorkflows finds workflows with status='running' that don't have an active lock.
// These are workflows that had a message delivered and are waiting for a worker to resume them.
func (s *SQLiteStorage) FindResumableWorkflows(ctx context.Context) ([]*ResumableWorkflow, error) {
	conn := s.getConn(ctx)
	query := `
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE status = ?
		AND (locked_by IS NULL OR locked_by = '')
		ORDER BY updated_at ASC
		LIMIT 100
	`

	rows, err := conn.QueryContext(ctx, query, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var workflows []*ResumableWorkflow
	for rows.Next() {
		var wf ResumableWorkflow
		if err := rows.Scan(&wf.InstanceID, &wf.WorkflowName); err != nil {
			return nil, err
		}
		workflows = append(workflows, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return workflows, nil
}

// --- Lock Manager ---

// TryAcquireLock attempts to acquire a lock on a workflow instance.
func (s *SQLiteStorage) TryAcquireLock(ctx context.Context, instanceID, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	nowStr := now.Format("2006-01-02 15:04:05")
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)
	expiresStr := expiresAt.Format("2006-01-02 15:04:05")

	// Allow acquiring if:
	// 1. No lock exists (locked_by IS NULL)
	// 2. Lock is expired (lock_expires_at < now)
	// 3. Same worker already holds the lock (re-entrant)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = ?, locked_at = ?, lock_expires_at = ?, updated_at = ?
		WHERE instance_id = ?
		AND (locked_by IS NULL OR datetime(lock_expires_at) < datetime('now') OR locked_by = ?)
	`, workerID, nowStr, expiresStr, nowStr, instanceID, workerID)
	if err != nil {
		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected > 0, nil
}

// ReleaseLock releases a lock on a workflow instance.
func (s *SQLiteStorage) ReleaseLock(ctx context.Context, instanceID, workerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
	`, instanceID, workerID)
	return err
}

// RefreshLock extends the lock expiration time.
func (s *SQLiteStorage) RefreshLock(ctx context.Context, instanceID, workerID string, timeoutSec int) error {
	conn := s.getConn(ctx)
	expiresAt := time.Now().UTC().Add(time.Duration(timeoutSec) * time.Second)
	expiresStr := expiresAt.Format("2006-01-02 15:04:05")
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET lock_expires_at = ?, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
	`, expiresStr, instanceID, workerID)
	return err
}

// CleanupStaleLocks cleans up expired locks and returns workflows to resume.
func (s *SQLiteStorage) CleanupStaleLocks(ctx context.Context, timeoutSec int) ([]StaleWorkflowInfo, error) {
	conn := s.getConn(ctx)

	// Find stale locks (limit to 100 to prevent memory spikes)
	rows, err := conn.QueryContext(ctx, `
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE locked_by IS NOT NULL
		AND datetime(lock_expires_at) < datetime('now')
		AND status IN ('running', 'waiting_for_event', 'waiting_for_timer')
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var stale []StaleWorkflowInfo
	for rows.Next() {
		var info StaleWorkflowInfo
		if err := rows.Scan(&info.InstanceID, &info.WorkflowName); err != nil {
			return nil, err
		}
		stale = append(stale, info)
	}

	// Clear stale locks
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = datetime('now')
		WHERE locked_by IS NOT NULL AND datetime(lock_expires_at) < datetime('now')
	`)
	if err != nil {
		return nil, err
	}

	return stale, nil
}

// --- History Manager ---

// AppendHistory appends a history event.
func (s *SQLiteStorage) AppendHistory(ctx context.Context, event *HistoryEvent) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, event_data_binary, data_type)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance_id, activity_id) DO NOTHING
	`, event.InstanceID, event.ActivityID, event.EventType, string(event.EventData), event.EventDataBinary, event.DataType)
	return err
}

// GetHistoryPaginated retrieves history events with pagination.
// Returns events with id > afterID, up to limit events.
// Returns (events, hasMore, error).
func (s *SQLiteStorage) GetHistoryPaginated(ctx context.Context, instanceID string, afterID int64, limit int) ([]*HistoryEvent, bool, error) {
	conn := s.getConn(ctx)
	// Fetch one extra to check if there are more events
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at
		FROM workflow_history
		WHERE instance_id = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?
	`, instanceID, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	var history []*HistoryEvent
	for rows.Next() {
		var h HistoryEvent
		var eventData sql.NullString
		var eventDataBinary []byte

		if err := rows.Scan(&h.ID, &h.InstanceID, &h.ActivityID, &h.EventType, &eventData, &eventDataBinary, &h.DataType, &h.CreatedAt); err != nil {
			return nil, false, err
		}
		if eventData.Valid {
			h.EventData = []byte(eventData.String)
		}
		h.EventDataBinary = eventDataBinary
		history = append(history, &h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	// Check if there are more events
	hasMore := len(history) > limit
	if hasMore {
		history = history[:limit] // Remove the extra event
	}

	return history, hasMore, nil
}

// GetHistoryCount returns the total number of history events for an instance.
func (s *SQLiteStorage) GetHistoryCount(ctx context.Context, instanceID string) (int64, error) {
	conn := s.getConn(ctx)
	var count int64
	err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_history WHERE instance_id = ?
	`, instanceID).Scan(&count)
	return count, err
}

// --- Timer Subscription Manager ---

// RegisterTimerSubscription registers a timer for a workflow.
func (s *SQLiteStorage) RegisterTimerSubscription(ctx context.Context, sub *TimerSubscription) error {
	conn := s.getConn(ctx)
	expiresStr := sub.ExpiresAt.UTC().Format("2006-01-02 15:04:05")
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, step)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (instance_id, timer_id) DO NOTHING
	`, sub.InstanceID, sub.TimerID, expiresStr, sub.Step)
	return err
}

// RegisterTimerSubscriptionAndReleaseLock atomically registers timer and releases lock.
func (s *SQLiteStorage) RegisterTimerSubscriptionAndReleaseLock(ctx context.Context, sub *TimerSubscription, instanceID, workerID string) error {
	var needTx bool
	if !s.InTransaction(ctx) {
		var err error
		ctx, err = s.BeginTransaction(ctx)
		if err != nil {
			return err
		}
		needTx = true
		defer func() {
			if needTx {
				_ = s.RollbackTransaction(ctx)
			}
		}()
	}

	conn := s.getConn(ctx)
	expiresStr := sub.ExpiresAt.UTC().Format("2006-01-02 15:04:05")

	// Register timer
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, step)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (instance_id, timer_id) DO NOTHING
	`, sub.InstanceID, sub.TimerID, expiresStr, sub.Step)
	if err != nil {
		return err
	}

	// Update status
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'waiting_for_timer', updated_at = datetime('now')
		WHERE instance_id = ?
	`, instanceID)
	if err != nil {
		return err
	}

	// Release lock
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL
		WHERE instance_id = ? AND locked_by = ?
	`, instanceID, workerID)
	if err != nil {
		return err
	}

	if needTx {
		if err := s.CommitTransaction(ctx); err != nil {
			return err
		}
		needTx = false
	}

	return nil
}

// RemoveTimerSubscription removes a timer subscription.
func (s *SQLiteStorage) RemoveTimerSubscription(ctx context.Context, instanceID, timerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_timer_subscriptions WHERE instance_id = ? AND timer_id = ?
	`, instanceID, timerID)
	return err
}

// FindExpiredTimers finds expired timers.
func (s *SQLiteStorage) FindExpiredTimers(ctx context.Context) ([]*TimerSubscription, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, timer_id, expires_at, step, created_at
		FROM workflow_timer_subscriptions
		WHERE datetime(expires_at) <= datetime('now')
		ORDER BY expires_at ASC
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var timers []*TimerSubscription
	for rows.Next() {
		var t TimerSubscription
		if err := rows.Scan(&t.ID, &t.InstanceID, &t.TimerID, &t.ExpiresAt, &t.Step, &t.CreatedAt); err != nil {
			return nil, err
		}
		timers = append(timers, &t)
	}
	return timers, rows.Err()
}

// --- Outbox Manager ---

// AddOutboxEvent adds an event to the outbox.
func (s *SQLiteStorage) AddOutboxEvent(ctx context.Context, event *OutboxEvent) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_outbox (event_id, event_type, event_source, event_data, data_type, content_type, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, event.EventID, event.EventType, event.EventSource, string(event.EventData), event.DataType, event.ContentType, event.Status)
	return err
}

// GetPendingOutboxEvents retrieves pending outbox events.
func (s *SQLiteStorage) GetPendingOutboxEvents(ctx context.Context, limit int) ([]*OutboxEvent, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, event_id, event_type, event_source, event_data, data_type, content_type, status, attempts, created_at
		FROM workflow_outbox
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []*OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		var eventData sql.NullString

		if err := rows.Scan(&e.ID, &e.EventID, &e.EventType, &e.EventSource, &eventData, &e.DataType, &e.ContentType, &e.Status, &e.Attempts, &e.CreatedAt); err != nil {
			return nil, err
		}
		if eventData.Valid {
			e.EventData = []byte(eventData.String)
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// MarkOutboxEventSent marks an outbox event as sent.
func (s *SQLiteStorage) MarkOutboxEventSent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_outbox
		SET status = 'sent', updated_at = datetime('now')
		WHERE event_id = ?
	`, eventID)
	return err
}

// MarkOutboxEventFailed marks an outbox event as failed.
func (s *SQLiteStorage) MarkOutboxEventFailed(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_outbox
		SET status = 'failed', updated_at = datetime('now')
		WHERE event_id = ?
	`, eventID)
	return err
}

// IncrementOutboxAttempts increments the attempt count for an event.
func (s *SQLiteStorage) IncrementOutboxAttempts(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_outbox
		SET attempts = attempts + 1, updated_at = datetime('now')
		WHERE event_id = ?
	`, eventID)
	return err
}

// CleanupOldOutboxEvents removes old sent events.
func (s *SQLiteStorage) CleanupOldOutboxEvents(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan).Format("2006-01-02 15:04:05")
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_outbox
		WHERE status = 'sent' AND datetime(created_at) < datetime(?)
	`, threshold)
	return err
}

// --- Compensation Manager ---

// AddCompensation adds a compensation entry.
func (s *SQLiteStorage) AddCompensation(ctx context.Context, entry *CompensationEntry) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_compensations (instance_id, activity_id, compensation_fn, compensation_arg, comp_order, status)
		VALUES (?, ?, ?, ?, ?, ?)
	`, entry.InstanceID, entry.ActivityID, entry.CompensationFn, string(entry.CompensationArg), entry.Order, entry.Status)
	return err
}

// GetCompensations retrieves compensations for a workflow in LIFO order.
func (s *SQLiteStorage) GetCompensations(ctx context.Context, instanceID string) ([]*CompensationEntry, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, activity_id, compensation_fn, compensation_arg, comp_order, status, created_at
		FROM workflow_compensations
		WHERE instance_id = ?
		ORDER BY comp_order DESC
	`, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var comps []*CompensationEntry
	for rows.Next() {
		var c CompensationEntry
		var compArg sql.NullString
		if err := rows.Scan(&c.ID, &c.InstanceID, &c.ActivityID, &c.CompensationFn, &compArg, &c.Order, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		if compArg.Valid {
			c.CompensationArg = []byte(compArg.String)
		}
		comps = append(comps, &c)
	}
	return comps, rows.Err()
}

// MarkCompensationExecuted marks a compensation as executed.
func (s *SQLiteStorage) MarkCompensationExecuted(ctx context.Context, id int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_compensations SET status = 'executed' WHERE id = ?
	`, id)
	return err
}

// MarkCompensationFailed marks a compensation as failed.
func (s *SQLiteStorage) MarkCompensationFailed(ctx context.Context, id int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_compensations SET status = 'failed' WHERE id = ?
	`, id)
	return err
}

// ========================================
// Channel Manager
// ========================================

// PublishToChannel publishes a message to a channel.
func (s *SQLiteStorage) PublishToChannel(ctx context.Context, channelName string, dataJSON, metadata []byte, targetInstanceID string) (int64, error) {
	conn := s.getConn(ctx)
	var dataJSONStr, metadataStr, targetStr sql.NullString
	if dataJSON != nil {
		dataJSONStr = sql.NullString{String: string(dataJSON), Valid: true}
	}
	if metadata != nil {
		metadataStr = sql.NullString{String: string(metadata), Valid: true}
	}
	if targetInstanceID != "" {
		targetStr = sql.NullString{String: targetInstanceID, Valid: true}
	}

	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_messages (channel_name, data_json, metadata, target_instance_id)
		VALUES (?, ?, ?, ?)
	`, channelName, dataJSONStr, metadataStr, targetStr)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SubscribeToChannel subscribes an instance to a channel.
// For broadcast mode, it initializes the delivery cursor to the current max message ID
// so that only messages published after subscription are received.
func (s *SQLiteStorage) SubscribeToChannel(ctx context.Context, instanceID, channelName string, mode ChannelMode) error {
	conn := s.getConn(ctx)

	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_subscriptions (instance_id, channel_name, mode)
		VALUES (?, ?, ?)
		ON CONFLICT (instance_id, channel_name) DO UPDATE SET mode = excluded.mode
	`, instanceID, channelName, string(mode))
	if err != nil {
		return err
	}

	// For broadcast mode, initialize the delivery cursor to current max message ID
	// This ensures new subscribers only receive messages published after subscription
	if mode == ChannelModeBroadcast {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel_name, last_message_id)
			SELECT ?, ?, COALESCE(MAX(id), 0)
			FROM channel_messages
			WHERE channel_name = ?
			ON CONFLICT (instance_id, channel_name) DO NOTHING
		`, instanceID, channelName, channelName)
		if err != nil {
			return err
		}
	}

	return nil
}

// UnsubscribeFromChannel unsubscribes an instance from a channel.
func (s *SQLiteStorage) UnsubscribeFromChannel(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_subscriptions WHERE instance_id = ? AND channel_name = ?
	`, instanceID, channelName)
	return err
}

// GetChannelSubscription retrieves a subscription for an instance and channel.
func (s *SQLiteStorage) GetChannelSubscription(ctx context.Context, instanceID, channelName string) (*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT id, instance_id, channel_name, mode, waiting, timeout_at, COALESCE(activity_id, ''), created_at
		FROM channel_subscriptions
		WHERE instance_id = ? AND channel_name = ?
	`, instanceID, channelName)

	var sub ChannelSubscription
	var modeStr string
	var waiting int
	var timeoutAt sql.NullTime
	err := row.Scan(&sub.ID, &sub.InstanceID, &sub.ChannelName, &modeStr, &waiting, &timeoutAt, &sub.ActivityID, &sub.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sub.Mode = ChannelMode(modeStr)
	sub.Waiting = waiting != 0
	if timeoutAt.Valid {
		sub.TimeoutAt = &timeoutAt.Time
	}
	return &sub, nil
}

// RegisterChannelReceiveAndReleaseLock atomically registers a channel receive wait and releases the lock.
func (s *SQLiteStorage) RegisterChannelReceiveAndReleaseLock(ctx context.Context, instanceID, channelName, workerID, activityID string, timeoutAt *time.Time) error {
	conn := s.getConn(ctx)

	// Update subscription to waiting state and store activity ID
	var timeoutStr sql.NullString
	if timeoutAt != nil {
		timeoutStr = sql.NullString{String: timeoutAt.UTC().Format("2006-01-02 15:04:05"), Valid: true}
	}
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET waiting = 1, timeout_at = ?, activity_id = ?
		WHERE instance_id = ? AND channel_name = ?
	`, timeoutStr, activityID, instanceID, channelName)
	if err != nil {
		return err
	}

	// Update instance status
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, updated_at = datetime('now')
		WHERE instance_id = ?
	`, StatusWaitingForMessage, instanceID)
	if err != nil {
		return err
	}

	// Release lock
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
	`, instanceID, workerID)
	return err
}

// GetPendingChannelMessages retrieves pending messages for a channel after a given ID.
func (s *SQLiteStorage) GetPendingChannelMessages(ctx context.Context, channelName string, afterID int64, limit int) ([]*ChannelMessage, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, channel_name, data_json, data_binary, metadata, target_instance_id, created_at
		FROM channel_messages
		WHERE channel_name = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?
	`, channelName, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []*ChannelMessage
	for rows.Next() {
		var msg ChannelMessage
		var dataJSON, metadata, targetID sql.NullString
		var dataBinary []byte
		err := rows.Scan(&msg.ID, &msg.ChannelName, &dataJSON, &dataBinary, &metadata, &targetID, &msg.CreatedAt)
		if err != nil {
			return nil, err
		}
		if dataJSON.Valid {
			msg.DataJSON = []byte(dataJSON.String)
		}
		msg.DataBinary = dataBinary
		if metadata.Valid {
			msg.Metadata = []byte(metadata.String)
		}
		if targetID.Valid {
			msg.TargetInstanceID = targetID.String
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

// GetPendingChannelMessagesForInstance gets pending messages for a specific subscriber.
// For broadcast mode: Returns messages with id > cursor (messages not yet seen by this instance)
// For competing mode: Returns unclaimed messages (not yet claimed by any instance)
func (s *SQLiteStorage) GetPendingChannelMessagesForInstance(ctx context.Context, instanceID, channelName string) ([]*ChannelMessage, error) {
	conn := s.getConn(ctx)

	// First, get the subscription to determine mode
	var mode string
	err := conn.QueryRowContext(ctx, `
		SELECT mode FROM channel_subscriptions
		WHERE instance_id = ? AND channel_name = ?
	`, instanceID, channelName).Scan(&mode)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Not subscribed
		}
		return nil, err
	}

	var query string
	var args []any

	if mode == string(ChannelModeBroadcast) {
		// Broadcast mode: Get messages after the cursor
		var cursorID int64
		err := conn.QueryRowContext(ctx, `
			SELECT COALESCE(last_message_id, 0) FROM channel_delivery_cursors
			WHERE instance_id = ? AND channel_name = ?
		`, instanceID, channelName).Scan(&cursorID)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}

		query = `
			SELECT id, channel_name, data_json, data_binary, metadata, target_instance_id, created_at
			FROM channel_messages
			WHERE channel_name = ? AND id > ?
			ORDER BY id ASC
			LIMIT 10
		`
		args = []any{channelName, cursorID}
	} else {
		// Competing mode: Get unclaimed messages
		query = `
			SELECT m.id, m.channel_name, m.data_json, m.data_binary, m.metadata, m.target_instance_id, m.created_at
			FROM channel_messages m
			LEFT JOIN channel_message_claims c ON m.id = c.message_id
			WHERE m.channel_name = ? AND c.message_id IS NULL
			ORDER BY m.id ASC
			LIMIT 10
		`
		args = []any{channelName}
	}

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []*ChannelMessage
	for rows.Next() {
		var msg ChannelMessage
		var dataJSON, metadata, targetID sql.NullString
		var dataBinary []byte
		err := rows.Scan(&msg.ID, &msg.ChannelName, &dataJSON, &dataBinary, &metadata, &targetID, &msg.CreatedAt)
		if err != nil {
			return nil, err
		}
		if dataJSON.Valid {
			msg.DataJSON = []byte(dataJSON.String)
		}
		msg.DataBinary = dataBinary
		if metadata.Valid {
			msg.Metadata = []byte(metadata.String)
		}
		if targetID.Valid {
			msg.TargetInstanceID = targetID.String
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

// ClaimChannelMessage claims a message for competing mode.
func (s *SQLiteStorage) ClaimChannelMessage(ctx context.Context, messageID int64, instanceID string) (bool, error) {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_message_claims (message_id, instance_id)
		VALUES (?, ?)
		ON CONFLICT (message_id) DO NOTHING
	`, messageID, instanceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// DeleteChannelMessage deletes a message from the channel.
func (s *SQLiteStorage) DeleteChannelMessage(ctx context.Context, messageID int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `DELETE FROM channel_messages WHERE id = ?`, messageID)
	return err
}

// UpdateDeliveryCursor updates the delivery cursor for broadcast mode.
func (s *SQLiteStorage) UpdateDeliveryCursor(ctx context.Context, instanceID, channelName string, lastMessageID int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_delivery_cursors (instance_id, channel_name, last_message_id)
		VALUES (?, ?, ?)
		ON CONFLICT (instance_id, channel_name) DO UPDATE SET last_message_id = excluded.last_message_id, updated_at = datetime('now')
	`, instanceID, channelName, lastMessageID)
	return err
}

// GetDeliveryCursor gets the current delivery cursor for an instance and channel.
func (s *SQLiteStorage) GetDeliveryCursor(ctx context.Context, instanceID, channelName string) (int64, error) {
	conn := s.getConn(ctx)
	var lastMessageID int64
	err := conn.QueryRowContext(ctx, `
		SELECT last_message_id FROM channel_delivery_cursors
		WHERE instance_id = ? AND channel_name = ?
	`, instanceID, channelName).Scan(&lastMessageID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return lastMessageID, err
}

// GetChannelSubscribersWaiting finds subscribers waiting for messages on a channel.
func (s *SQLiteStorage) GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, channel_name, mode, waiting, timeout_at, COALESCE(activity_id, ''), created_at
		FROM channel_subscriptions
		WHERE channel_name = ? AND waiting = 1
	`, channelName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []*ChannelSubscription
	for rows.Next() {
		var sub ChannelSubscription
		var modeStr string
		var waiting int
		var timeoutAt sql.NullTime
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.ChannelName, &modeStr, &waiting, &timeoutAt, &sub.ActivityID, &sub.CreatedAt)
		if err != nil {
			return nil, err
		}
		sub.Mode = ChannelMode(modeStr)
		sub.Waiting = waiting != 0
		if timeoutAt.Valid {
			sub.TimeoutAt = &timeoutAt.Time
		}
		subs = append(subs, &sub)
	}
	return subs, rows.Err()
}

// ClearChannelWaitingState clears the waiting state for an instance's channel subscription.
func (s *SQLiteStorage) ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET waiting = 0, timeout_at = NULL
		WHERE instance_id = ? AND channel_name = ?
	`, instanceID, channelName)
	return err
}

// DeliverChannelMessage delivers a message to a waiting subscriber.
func (s *SQLiteStorage) DeliverChannelMessage(ctx context.Context, instanceID string, message *ChannelMessage) error {
	conn := s.getConn(ctx)

	// Record message receipt in history
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, data_type)
		VALUES (?, ?, 'channel_message_received', ?, 'json')
	`, instanceID, fmt.Sprintf("channel:%s:%d", message.ChannelName, message.ID), string(message.DataJSON))
	if err != nil {
		return err
	}

	// Clear waiting state
	return s.ClearChannelWaitingState(ctx, instanceID, message.ChannelName)
}

// DeliverChannelMessageWithLock delivers a message using Lock-First pattern.
// Returns nil result if lock could not be acquired (another worker will handle it).
func (s *SQLiteStorage) DeliverChannelMessageWithLock(
	ctx context.Context,
	instanceID string,
	channelName string,
	message *ChannelMessage,
	workerID string,
	lockTimeoutSec int,
) (*ChannelDeliveryResult, error) {
	conn := s.getConn(ctx)

	// Step 1: Try to acquire lock (Lock-First pattern)
	acquired, err := s.TryAcquireLock(ctx, instanceID, workerID, lockTimeoutSec)
	if err != nil {
		return nil, err
	}
	if !acquired {
		// Another worker has the lock, skip this delivery
		return nil, nil
	}

	// Step 2: Get workflow info and subscription activity ID
	var workflowName string
	err = conn.QueryRowContext(ctx, `
		SELECT workflow_name FROM workflow_instances WHERE instance_id = ?
	`, instanceID).Scan(&workflowName)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get workflow info: %w", err)
	}

	// Get the activity ID and mode from the subscription (stored when Receive was called)
	var historyActivityID, subscriptionMode string
	err = conn.QueryRowContext(ctx, `
		SELECT COALESCE(activity_id, ''), mode FROM channel_subscriptions
		WHERE instance_id = ? AND channel_name = ?
	`, instanceID, channelName).Scan(&historyActivityID, &subscriptionMode)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get subscription info: %w", err)
	}
	// Fallback to legacy format if no activity ID stored
	if historyActivityID == "" {
		historyActivityID = fmt.Sprintf("channel:%s:%d", channelName, message.ID)
	}

	// Step 3: Record message in history using the correct activity ID
	// Wrap the message in ReceivedMessage format so Receive can unmarshal it correctly
	wrappedData := map[string]any{
		"id":           message.ID,
		"channel_name": channelName,
		"created_at":   message.CreatedAt,
	}
	// The message.DataJSON contains the actual data - unmarshal and re-wrap
	var msgData any
	if len(message.DataJSON) > 0 {
		_ = json.Unmarshal(message.DataJSON, &msgData)
	}
	wrappedData["data"] = msgData
	if len(message.Metadata) > 0 {
		var metadata map[string]any
		_ = json.Unmarshal(message.Metadata, &metadata)
		wrappedData["metadata"] = metadata
	}
	wrappedJSON, _ := json.Marshal(wrappedData)

	_, err = conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, data_type)
		VALUES (?, ?, 'activity_completed', ?, 'json')
	`, instanceID, historyActivityID, string(wrappedJSON))
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to record history: %w", err)
	}

	// Step 3.5: Update delivery cursor for broadcast mode
	// This prevents duplicate message delivery when workflow resumes
	if subscriptionMode == string(ChannelModeBroadcast) {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel_name, last_message_id)
			VALUES (?, ?, ?)
			ON CONFLICT (instance_id, channel_name)
			DO UPDATE SET last_message_id = excluded.last_message_id, updated_at = datetime('now')
		`, instanceID, channelName, message.ID)
		if err != nil {
			_ = s.ReleaseLock(ctx, instanceID, workerID)
			return nil, fmt.Errorf("failed to update delivery cursor: %w", err)
		}
	}

	// Step 4: Clear waiting state
	if err := s.ClearChannelWaitingState(ctx, instanceID, channelName); err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to clear waiting state: %w", err)
	}

	// Step 5: Update status to 'running'
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances SET status = ?, updated_at = datetime('now')
		WHERE instance_id = ?
	`, StatusRunning, instanceID)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to update status: %w", err)
	}

	// Step 6: Release lock (workflow will be picked up by resumption task)
	if err := s.ReleaseLock(ctx, instanceID, workerID); err != nil {
		return nil, fmt.Errorf("failed to release lock: %w", err)
	}

	return &ChannelDeliveryResult{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		ActivityID:   historyActivityID,
	}, nil
}

// CleanupOldChannelMessages removes old channel messages.
func (s *SQLiteStorage) CleanupOldChannelMessages(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan).Format("2006-01-02 15:04:05")
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_messages WHERE datetime(created_at) < datetime(?)
	`, threshold)
	return err
}

// FindExpiredChannelSubscriptions finds channel subscriptions that have timed out.
func (s *SQLiteStorage) FindExpiredChannelSubscriptions(ctx context.Context) ([]*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, channel_name, mode, waiting, timeout_at, COALESCE(activity_id, ''), created_at
		FROM channel_subscriptions
		WHERE waiting = 1 AND timeout_at IS NOT NULL AND datetime(timeout_at) < datetime('now')
		ORDER BY timeout_at ASC
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []*ChannelSubscription
	for rows.Next() {
		var sub ChannelSubscription
		var modeStr string
		var waiting int
		var timeoutAt sql.NullTime
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.ChannelName, &modeStr, &waiting, &timeoutAt, &sub.ActivityID, &sub.CreatedAt)
		if err != nil {
			return nil, err
		}
		sub.Mode = ChannelMode(modeStr)
		sub.Waiting = waiting != 0
		if timeoutAt.Valid {
			sub.TimeoutAt = &timeoutAt.Time
		}
		subs = append(subs, &sub)
	}
	return subs, rows.Err()
}

// ========================================
// Group Manager
// ========================================

// JoinGroup adds an instance to a group.
func (s *SQLiteStorage) JoinGroup(ctx context.Context, instanceID, groupName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_group_memberships (instance_id, group_name)
		VALUES (?, ?)
		ON CONFLICT (instance_id, group_name) DO NOTHING
	`, instanceID, groupName)
	return err
}

// LeaveGroup removes an instance from a group.
func (s *SQLiteStorage) LeaveGroup(ctx context.Context, instanceID, groupName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_group_memberships WHERE instance_id = ? AND group_name = ?
	`, instanceID, groupName)
	return err
}

// GetGroupMembers retrieves all instance IDs in a group.
func (s *SQLiteStorage) GetGroupMembers(ctx context.Context, groupName string) ([]string, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT instance_id FROM workflow_group_memberships WHERE group_name = ?
	`, groupName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var members []string
	for rows.Next() {
		var instanceID string
		if err := rows.Scan(&instanceID); err != nil {
			return nil, err
		}
		members = append(members, instanceID)
	}
	return members, rows.Err()
}

// LeaveAllGroups removes an instance from all groups.
func (s *SQLiteStorage) LeaveAllGroups(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_group_memberships WHERE instance_id = ?
	`, instanceID)
	return err
}

// ========================================
// System Lock Manager
// ========================================

// TryAcquireSystemLock attempts to acquire a system lock.
func (s *SQLiteStorage) TryAcquireSystemLock(ctx context.Context, lockName, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	nowStr := now.Format("2006-01-02 15:04:05")
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)
	expiresStr := expiresAt.Format("2006-01-02 15:04:05")

	// Try to insert or update if expired or same worker
	result, err := conn.ExecContext(ctx, `
		INSERT INTO system_locks (lock_name, locked_by, locked_at, expires_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (lock_name) DO UPDATE SET
			locked_by = excluded.locked_by,
			locked_at = excluded.locked_at,
			expires_at = excluded.expires_at
		WHERE datetime(system_locks.expires_at) < datetime('now') OR system_locks.locked_by = excluded.locked_by
	`, lockName, workerID, nowStr, expiresStr)
	if err != nil {
		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ReleaseSystemLock releases a system lock.
func (s *SQLiteStorage) ReleaseSystemLock(ctx context.Context, lockName, workerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM system_locks WHERE lock_name = ? AND locked_by = ?
	`, lockName, workerID)
	return err
}

// CleanupExpiredSystemLocks removes expired system locks.
func (s *SQLiteStorage) CleanupExpiredSystemLocks(ctx context.Context) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM system_locks WHERE datetime(expires_at) < datetime('now')
	`)
	return err
}

// ========================================
// History Archive Manager
// ========================================

// ArchiveHistory moves all history events for an instance to the archive table.
func (s *SQLiteStorage) ArchiveHistory(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)

	// Copy to archive
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history_archive (original_id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, original_created_at)
		SELECT id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at
		FROM workflow_history
		WHERE instance_id = ?
	`, instanceID)
	if err != nil {
		return err
	}

	// Delete from original
	_, err = conn.ExecContext(ctx, `
		DELETE FROM workflow_history WHERE instance_id = ?
	`, instanceID)
	return err
}

// GetArchivedHistory retrieves archived history events for an instance.
func (s *SQLiteStorage) GetArchivedHistory(ctx context.Context, instanceID string) ([]*ArchivedHistoryEvent, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, original_id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, original_created_at, archived_at
		FROM workflow_history_archive
		WHERE instance_id = ?
		ORDER BY original_id ASC
	`, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []*ArchivedHistoryEvent
	for rows.Next() {
		var e ArchivedHistoryEvent
		var eventData sql.NullString
		var originalID sql.NullInt64
		var originalCreatedAt sql.NullTime
		err := rows.Scan(&e.ID, &originalID, &e.InstanceID, &e.ActivityID, &e.EventType, &eventData, &e.EventDataBinary, &e.DataType, &originalCreatedAt, &e.ArchivedAt)
		if err != nil {
			return nil, err
		}
		if originalID.Valid {
			e.OriginalID = originalID.Int64
		}
		if eventData.Valid {
			e.EventData = []byte(eventData.String)
		}
		if originalCreatedAt.Valid {
			e.OriginalCreatedAt = originalCreatedAt.Time
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// CleanupInstanceSubscriptions removes all subscriptions for an instance.
func (s *SQLiteStorage) CleanupInstanceSubscriptions(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)

	// Remove event subscriptions
	_, err := conn.ExecContext(ctx, `DELETE FROM workflow_event_subscriptions WHERE instance_id = ?`, instanceID)
	if err != nil {
		return err
	}

	// Remove timer subscriptions
	_, err = conn.ExecContext(ctx, `DELETE FROM workflow_timer_subscriptions WHERE instance_id = ?`, instanceID)
	if err != nil {
		return err
	}

	// Remove channel subscriptions
	_, err = conn.ExecContext(ctx, `DELETE FROM channel_subscriptions WHERE instance_id = ?`, instanceID)
	if err != nil {
		return err
	}

	// Remove group memberships
	_, err = conn.ExecContext(ctx, `DELETE FROM workflow_group_memberships WHERE instance_id = ?`, instanceID)
	return err
}
