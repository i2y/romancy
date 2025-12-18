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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/i2y/romancy/internal/notify"
)

// PostgresStorage implements the Storage interface using PostgreSQL.
type PostgresStorage struct {
	db            *sql.DB
	driver        Driver
	notifyEnabled bool
}

// NewPostgresStorage creates a new PostgreSQL storage.
// The connStr should be a PostgreSQL connection string:
// "postgres://user:password@localhost:5432/dbname?sslmode=disable"
func NewPostgresStorage(connStr string) (*PostgresStorage, error) {
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &PostgresStorage{
		db:     db,
		driver: &PostgresDriver{},
	}, nil
}

// Initialize is deprecated. Use dbmate for migrations:
// dbmate -d schema/db/migrations/postgresql up
func (s *PostgresStorage) Initialize(ctx context.Context) error {
	slog.Warn("Initialize() is deprecated. Use dbmate for migrations: dbmate -d schema/db/migrations/postgresql up")
	return nil
}

// DB returns the underlying database connection.
func (s *PostgresStorage) DB() *sql.DB {
	return s.db
}

// Close closes the database connection.
func (s *PostgresStorage) Close() error {
	return s.db.Close()
}

// SetNotifyEnabled enables or disables PostgreSQL LISTEN/NOTIFY.
// When enabled, the storage will send pg_notify() calls after key operations.
func (s *PostgresStorage) SetNotifyEnabled(enabled bool) {
	s.notifyEnabled = enabled
}

// IsNotifyEnabled returns whether PostgreSQL LISTEN/NOTIFY is enabled.
func (s *PostgresStorage) IsNotifyEnabled() bool {
	return s.notifyEnabled
}

// sendNotify sends a PostgreSQL notification if enabled.
// Errors are logged but do not interrupt the main operation.
func (s *PostgresStorage) sendNotify(ctx context.Context, channel notify.NotifyChannel, payload string) {
	if !s.notifyEnabled {
		return
	}
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, "SELECT pg_notify($1, $2)", string(channel), payload)
	if err != nil {
		slog.Debug("failed to send pg_notify", "channel", channel, "error", err)
	}
}

// getConn returns the appropriate database handle based on context.
func (s *PostgresStorage) getConn(ctx context.Context) Executor {
	if state, ok := ctx.Value(txKey{}).(*txState); ok {
		return state.tx
	}
	return s.db
}

// --- Transaction Manager ---

// BeginTransaction starts a new transaction.
func (s *PostgresStorage) BeginTransaction(ctx context.Context) (context.Context, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ctx, err
	}
	state := &txState{tx: tx}
	return context.WithValue(ctx, txKey{}, state), nil
}

// CommitTransaction commits the current transaction.
func (s *PostgresStorage) CommitTransaction(ctx context.Context) error {
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
func (s *PostgresStorage) RollbackTransaction(ctx context.Context) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return nil
	}
	return state.tx.Rollback()
}

// InTransaction returns whether a transaction is in progress.
func (s *PostgresStorage) InTransaction(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{}).(*txState)
	return ok
}

// Conn returns the database executor for the current context.
func (s *PostgresStorage) Conn(ctx context.Context) Executor {
	return s.getConn(ctx)
}

// RegisterPostCommitCallback registers a callback to be executed after a successful commit.
// Returns an error if not currently in a transaction.
func (s *PostgresStorage) RegisterPostCommitCallback(ctx context.Context, cb func() error) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return fmt.Errorf("not in a transaction")
	}
	state.callbacks = append(state.callbacks, cb)
	return nil
}

// --- Instance Manager ---

// CreateInstance creates a new workflow instance.
func (s *PostgresStorage) CreateInstance(ctx context.Context, instance *WorkflowInstance) error {
	conn := s.getConn(ctx)
	framework := "go"

	// Ensure workflow definition exists (required by foreign key constraint)
	// Uses ON CONFLICT DO NOTHING for idempotency
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_definitions (workflow_name, source_hash, source_code)
		VALUES ($1, $2, $3)
		ON CONFLICT (workflow_name, source_hash) DO NOTHING
	`, instance.WorkflowName, instance.SourceHash, "")
	if err != nil {
		return fmt.Errorf("failed to ensure workflow definition: %w", err)
	}

	_, err = conn.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			instance_id, workflow_name, status, input_data, source_hash, owner_service, framework, started_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, instance.InstanceID, instance.WorkflowName, instance.Status,
		string(instance.InputData), instance.SourceHash, instance.OwnerService,
		framework, instance.StartedAt.UTC(), instance.UpdatedAt.UTC())
	return err
}

// GetInstance retrieves a workflow instance by ID.
func (s *PostgresStorage) GetInstance(ctx context.Context, instanceID string) (*WorkflowInstance, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, status, input_data, output_data,
			   current_activity_id, source_hash, owner_service, framework,
			   locked_by, locked_at, lock_timeout_seconds, lock_expires_at,
			   started_at, updated_at
		FROM workflow_instances WHERE instance_id = $1
	`, instanceID)

	var inst WorkflowInstance
	var inputData, outputData, activityID, sourceHash, ownerService, framework sql.NullString
	var lockedBy sql.NullString
	var lockedAt, lockExpiresAt sql.NullTime
	var lockTimeout sql.NullInt64

	err := row.Scan(
		&inst.InstanceID, &inst.WorkflowName, &inst.Status,
		&inputData, &outputData, &activityID, &sourceHash, &ownerService, &framework,
		&lockedBy, &lockedAt, &lockTimeout, &lockExpiresAt,
		&inst.StartedAt, &inst.UpdatedAt,
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
	if activityID.Valid {
		inst.CurrentActivityID = activityID.String
	}
	if sourceHash.Valid {
		inst.SourceHash = sourceHash.String
	}
	if ownerService.Valid {
		inst.OwnerService = ownerService.String
	}
	if framework.Valid {
		inst.Framework = framework.String
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
func (s *PostgresStorage) UpdateInstanceStatus(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg string) error {
	conn := s.getConn(ctx)
	// Note: errorMsg parameter kept for interface compatibility but ignored
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = $1, updated_at = NOW()
		WHERE instance_id = $2
	`, status, instanceID)
	return err
}

// UpdateInstanceActivity updates the current activity ID.
func (s *PostgresStorage) UpdateInstanceActivity(ctx context.Context, instanceID, activityID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET current_activity_id = $1, updated_at = NOW()
		WHERE instance_id = $2
	`, activityID, instanceID)
	return err
}

// UpdateInstanceOutput updates the output data.
func (s *PostgresStorage) UpdateInstanceOutput(ctx context.Context, instanceID string, outputData []byte) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET output_data = $1, status = 'completed', updated_at = NOW()
		WHERE instance_id = $2
	`, string(outputData), instanceID)
	return err
}

// CancelInstance cancels a workflow instance.
// Returns ErrWorkflowNotCancellable if the workflow is already completed, cancelled, failed, or does not exist.
func (s *PostgresStorage) CancelInstance(ctx context.Context, instanceID, reason string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'cancelled', updated_at = NOW()
		WHERE instance_id = $1 AND status IN ('pending', 'running', 'waiting_for_event', 'waiting_for_timer', 'waiting_for_message', 'recurred')
	`, instanceID)
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
func (s *PostgresStorage) ListInstances(ctx context.Context, opts ListInstancesOptions) (*PaginationResult, error) {
	conn := s.getConn(ctx)
	query := "SELECT instance_id, workflow_name, status, started_at, updated_at FROM workflow_instances WHERE framework = 'go'"
	args := []any{}
	argN := 1

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
		query += fmt.Sprintf(" AND LOWER(workflow_name) LIKE LOWER($%d)", argN)
		args = append(args, "%"+workflowFilter+"%")
		argN++
	}
	if statusFilter != "" {
		query += fmt.Sprintf(" AND status = $%d", argN)
		args = append(args, statusFilter)
		argN++
	}
	if opts.InstanceIDFilter != "" {
		query += fmt.Sprintf(" AND LOWER(instance_id) LIKE LOWER($%d)", argN)
		args = append(args, "%"+opts.InstanceIDFilter+"%")
		argN++
	}
	if opts.StartedAfter != nil {
		query += fmt.Sprintf(" AND started_at > $%d", argN)
		args = append(args, opts.StartedAfter.UTC())
		argN++
	}
	if opts.StartedBefore != nil {
		query += fmt.Sprintf(" AND started_at < $%d", argN)
		args = append(args, opts.StartedBefore.UTC())
		argN++
	}

	// Handle input filters
	if len(opts.InputFilters) > 0 {
		filterBuilder := NewInputFilterBuilder(s.driver)
		filterConditions, filterArgs, err := filterBuilder.BuildFilterQuery(opts.InputFilters, argN)
		if err != nil {
			return nil, fmt.Errorf("invalid input filter: %w", err)
		}
		for _, cond := range filterConditions {
			query += " AND " + cond
		}
		args = append(args, filterArgs...)
		argN += len(filterArgs)
	}

	// Parse cursor token (format: "ISO_DATETIME||INSTANCE_ID")
	if opts.PageToken != "" {
		parts := strings.SplitN(opts.PageToken, "||", 2)
		if len(parts) == 2 {
			cursorTime, err := time.Parse(time.RFC3339Nano, parts[0])
			if err == nil {
				cursorID := parts[1]
				// For descending order: get rows where (started_at, instance_id) < (cursor_time, cursor_id)
				query += fmt.Sprintf(" AND (started_at < $%d OR (started_at = $%d AND instance_id < $%d))", argN, argN+1, argN+2)
				args = append(args, cursorTime.UTC(), cursorTime.UTC(), cursorID)
				argN += 3
			}
		}
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50 // Default page size
	}
	// Fetch one extra to determine if there are more pages
	query += fmt.Sprintf(" ORDER BY started_at DESC, instance_id DESC LIMIT $%d", argN)
	args = append(args, limit+1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var instances []*WorkflowInstance
	for rows.Next() {
		var inst WorkflowInstance
		if err := rows.Scan(&inst.InstanceID, &inst.WorkflowName, &inst.Status, &inst.StartedAt, &inst.UpdatedAt); err != nil {
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
		nextPageToken = lastInst.StartedAt.UTC().Format(time.RFC3339Nano) + "||" + lastInst.InstanceID
	}

	return &PaginationResult{
		Instances:     instances,
		NextPageToken: nextPageToken,
		HasMore:       hasMore,
	}, nil
}

// FindResumableWorkflows finds workflows with status='running' that don't have an active lock.
// These are workflows that had a message delivered and are waiting for a worker to resume them.
func (s *PostgresStorage) FindResumableWorkflows(ctx context.Context, limit int) ([]*ResumableWorkflow, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	query := `
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE status = $1
		AND (locked_by IS NULL OR locked_by = '')
		ORDER BY updated_at ASC
		LIMIT $2
	`

	rows, err := conn.QueryContext(ctx, query, StatusRunning, limit)
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
func (s *PostgresStorage) TryAcquireLock(ctx context.Context, instanceID, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)

	// Allow acquiring if:
	// 1. No lock exists (locked_by IS NULL)
	// 2. Lock is expired (lock_expires_at < now)
	// 3. Same worker already holds the lock (re-entrant)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = $1, locked_at = $2, lock_expires_at = $3, updated_at = $2
		WHERE instance_id = $4
		AND (locked_by IS NULL OR lock_expires_at < NOW() OR locked_by = $1)
	`, workerID, now, expiresAt, instanceID)
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
func (s *PostgresStorage) ReleaseLock(ctx context.Context, instanceID, workerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
		WHERE instance_id = $1 AND locked_by = $2
	`, instanceID, workerID)
	return err
}

// RefreshLock extends the lock expiration time.
func (s *PostgresStorage) RefreshLock(ctx context.Context, instanceID, workerID string, timeoutSec int) error {
	conn := s.getConn(ctx)
	expiresAt := time.Now().UTC().Add(time.Duration(timeoutSec) * time.Second)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET lock_expires_at = $1, updated_at = NOW()
		WHERE instance_id = $2 AND locked_by = $3
	`, expiresAt, instanceID, workerID)
	return err
}

// CleanupStaleLocks cleans up expired locks and returns workflows to resume.
func (s *PostgresStorage) CleanupStaleLocks(ctx context.Context, timeoutSec int) ([]StaleWorkflowInfo, error) {
	conn := s.getConn(ctx)

	// Find stale locks (limit to 100 to prevent memory spikes)
	rows, err := conn.QueryContext(ctx, `
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE locked_by IS NOT NULL
		AND lock_expires_at < NOW()
		AND status IN ('running', 'waiting_for_event', 'waiting_for_timer')
		AND framework = 'go'
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

	// Clear stale locks (only for 'go' framework)
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
		WHERE locked_by IS NOT NULL AND lock_expires_at < NOW() AND framework = 'go'
	`)
	if err != nil {
		return nil, err
	}

	return stale, nil
}

// --- History Manager ---

// AppendHistory appends a history event.
func (s *PostgresStorage) AppendHistory(ctx context.Context, event *HistoryEvent) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, event_data_binary, data_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (instance_id, activity_id) DO NOTHING
	`, event.InstanceID, event.ActivityID, event.EventType, string(event.EventData), event.EventDataBinary, event.DataType)
	return err
}

// GetHistoryPaginated retrieves history events with pagination.
// Returns events with id > afterID, up to limit events.
// Returns (events, hasMore, error).
func (s *PostgresStorage) GetHistoryPaginated(ctx context.Context, instanceID string, afterID int64, limit int) ([]*HistoryEvent, bool, error) {
	conn := s.getConn(ctx)
	// Fetch one extra to check if there are more events
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at
		FROM workflow_history
		WHERE instance_id = $1 AND id > $2
		ORDER BY id ASC
		LIMIT $3
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
func (s *PostgresStorage) GetHistoryCount(ctx context.Context, instanceID string) (int64, error) {
	conn := s.getConn(ctx)
	var count int64
	err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_history WHERE instance_id = $1
	`, instanceID).Scan(&count)
	return count, err
}

// --- Timer Subscription Manager ---

// RegisterTimerSubscription registers a timer for a workflow.
func (s *PostgresStorage) RegisterTimerSubscription(ctx context.Context, sub *TimerSubscription) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, activity_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, timer_id) DO NOTHING
	`, sub.InstanceID, sub.TimerID, sub.ExpiresAt.UTC(), sub.ActivityID)
	if err != nil {
		return err
	}

	// Send notification for timer registered
	timerPayload := map[string]string{
		"instance_id": sub.InstanceID,
		"timer_id":    sub.TimerID,
		"expires_at":  sub.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if payloadBytes, err := json.Marshal(timerPayload); err == nil {
		s.sendNotify(ctx, notify.ChannelTimerExpired, string(payloadBytes))
	}

	return nil
}

// RegisterTimerSubscriptionAndReleaseLock atomically registers timer and releases lock.
func (s *PostgresStorage) RegisterTimerSubscriptionAndReleaseLock(ctx context.Context, sub *TimerSubscription, instanceID, workerID string) error {
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

	// Register timer
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, activity_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, timer_id) DO NOTHING
	`, sub.InstanceID, sub.TimerID, sub.ExpiresAt.UTC(), sub.ActivityID)
	if err != nil {
		return err
	}

	// Update status
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'waiting_for_timer', updated_at = NOW()
		WHERE instance_id = $1
	`, instanceID)
	if err != nil {
		return err
	}

	// Release lock
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL
		WHERE instance_id = $1 AND locked_by = $2
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

	// Send notification for timer registered (after commit)
	timerPayload := map[string]string{
		"instance_id": sub.InstanceID,
		"timer_id":    sub.TimerID,
		"expires_at":  sub.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if payloadBytes, err := json.Marshal(timerPayload); err == nil {
		s.sendNotify(ctx, notify.ChannelTimerExpired, string(payloadBytes))
	}

	return nil
}

// RemoveTimerSubscription removes a timer subscription.
func (s *PostgresStorage) RemoveTimerSubscription(ctx context.Context, instanceID, timerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_timer_subscriptions WHERE instance_id = $1 AND timer_id = $2
	`, instanceID, timerID)
	return err
}

// FindExpiredTimers finds expired timers.
func (s *PostgresStorage) FindExpiredTimers(ctx context.Context, limit int) ([]*TimerSubscription, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT t.id, t.instance_id, t.timer_id, t.expires_at, t.activity_id, t.created_at
		FROM workflow_timer_subscriptions t
		JOIN workflow_instances w ON t.instance_id = w.instance_id
		WHERE t.expires_at <= NOW() AND w.framework = 'go'
		ORDER BY t.expires_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var timers []*TimerSubscription
	for rows.Next() {
		var t TimerSubscription
		var activityID sql.NullString
		if err := rows.Scan(&t.ID, &t.InstanceID, &t.TimerID, &t.ExpiresAt, &activityID, &t.CreatedAt); err != nil {
			return nil, err
		}
		if activityID.Valid {
			t.ActivityID = activityID.String
		}
		timers = append(timers, &t)
	}
	return timers, rows.Err()
}

// --- Outbox Manager ---

// AddOutboxEvent adds an event to the outbox.
func (s *PostgresStorage) AddOutboxEvent(ctx context.Context, event *OutboxEvent) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO outbox_events (event_id, event_type, event_source, event_data, data_type, content_type, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, event.EventID, event.EventType, event.EventSource, string(event.EventData), event.DataType, event.ContentType, event.Status)
	if err != nil {
		return err
	}

	// Send notification for outbox pending
	outboxPayload := map[string]string{"event_id": event.EventID}
	if payloadBytes, err := json.Marshal(outboxPayload); err == nil {
		s.sendNotify(ctx, notify.ChannelOutboxPending, string(payloadBytes))
	}

	return nil
}

// GetPendingOutboxEvents retrieves pending outbox events.
// Uses SELECT FOR UPDATE SKIP LOCKED for concurrent workers.
func (s *PostgresStorage) GetPendingOutboxEvents(ctx context.Context, limit int) ([]*OutboxEvent, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT event_id, event_type, event_source, event_data, data_type, content_type, status, retry_count, created_at
		FROM outbox_events
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []*OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		var eventData sql.NullString

		if err := rows.Scan(&e.EventID, &e.EventType, &e.EventSource, &eventData, &e.DataType, &e.ContentType, &e.Status, &e.RetryCount, &e.CreatedAt); err != nil {
			return nil, err
		}
		if eventData.Valid {
			e.EventData = []byte(eventData.String)
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// MarkOutboxEventSent marks an outbox event as published.
func (s *PostgresStorage) MarkOutboxEventSent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'published', published_at = NOW()
		WHERE event_id = $1
	`, eventID)
	return err
}

// MarkOutboxEventFailed marks an outbox event as failed.
func (s *PostgresStorage) MarkOutboxEventFailed(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'failed'
		WHERE event_id = $1
	`, eventID)
	return err
}

// IncrementOutboxAttempts increments the attempt count for an event.
func (s *PostgresStorage) IncrementOutboxAttempts(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET retry_count = retry_count + 1
		WHERE event_id = $1
	`, eventID)
	return err
}

// CleanupOldOutboxEvents removes old published events.
func (s *PostgresStorage) CleanupOldOutboxEvents(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM outbox_events
		WHERE status = 'published' AND created_at < $1
	`, threshold)
	return err
}

// --- Compensation Manager ---

// AddCompensation adds a compensation entry.
func (s *PostgresStorage) AddCompensation(ctx context.Context, entry *CompensationEntry) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_compensations (instance_id, activity_id, activity_name, args)
		VALUES ($1, $2, $3, $4)
	`, entry.InstanceID, entry.ActivityID, entry.ActivityName, string(entry.Args))
	return err
}

// GetCompensations retrieves compensations for a workflow in LIFO order.
func (s *PostgresStorage) GetCompensations(ctx context.Context, instanceID string) ([]*CompensationEntry, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, activity_id, activity_name, args, created_at
		FROM workflow_compensations
		WHERE instance_id = $1
		ORDER BY created_at DESC, id DESC
	`, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var comps []*CompensationEntry
	for rows.Next() {
		var c CompensationEntry
		var args sql.NullString
		if err := rows.Scan(&c.ID, &c.InstanceID, &c.ActivityID, &c.ActivityName, &args, &c.CreatedAt); err != nil {
			return nil, err
		}
		if args.Valid {
			c.Args = []byte(args.String)
		}
		comps = append(comps, &c)
	}
	return comps, rows.Err()
}

// ========================================
// Channel Manager
// ========================================

// PublishToChannel publishes a message to a channel.
func (s *PostgresStorage) PublishToChannel(ctx context.Context, channelName string, dataJSON, metadata []byte) (int64, error) {
	conn := s.getConn(ctx)
	var dataStr, metadataStr sql.NullString
	if dataJSON != nil {
		dataStr = sql.NullString{String: string(dataJSON), Valid: true}
	}
	if metadata != nil {
		metadataStr = sql.NullString{String: string(metadata), Valid: true}
	}

	var id int64
	err := conn.QueryRowContext(ctx, `
		INSERT INTO channel_messages (channel, data, metadata, message_id, data_type)
		VALUES ($1, $2, $3, gen_random_uuid()::text, 'json')
		RETURNING id
	`, channelName, dataStr, metadataStr).Scan(&id)
	if err != nil {
		return 0, err
	}

	// Send notification for channel message
	msgPayload := map[string]any{
		"channel":    channelName,
		"message_id": id,
	}
	if payloadBytes, err := json.Marshal(msgPayload); err == nil {
		s.sendNotify(ctx, notify.ChannelChannelMessage, string(payloadBytes))
	}

	return id, nil
}

// SubscribeToChannel subscribes an instance to a channel.
// For broadcast mode, it initializes the delivery cursor to the current max message ID
// so that only messages published after subscription are received.
func (s *PostgresStorage) SubscribeToChannel(ctx context.Context, instanceID, channelName string, mode ChannelMode) error {
	conn := s.getConn(ctx)

	// Insert subscription
	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_subscriptions (instance_id, channel, mode)
		VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, channel) DO UPDATE SET mode = EXCLUDED.mode
	`, instanceID, channelName, string(mode))
	if err != nil {
		return err
	}

	// For broadcast mode, initialize the delivery cursor to current max message ID
	// This ensures new subscribers only receive messages published after subscription
	if mode == ChannelModeBroadcast {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
			SELECT $1, $2, COALESCE(MAX(id), 0)
			FROM channel_messages
			WHERE channel = $2
			ON CONFLICT (instance_id, channel) DO NOTHING
		`, instanceID, channelName)
		if err != nil {
			return err
		}
	}

	return nil
}

// UnsubscribeFromChannel unsubscribes an instance from a channel.
func (s *PostgresStorage) UnsubscribeFromChannel(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_subscriptions WHERE instance_id = $1 AND channel = $2
	`, instanceID, channelName)
	return err
}

// GetChannelSubscription retrieves a subscription for an instance and channel.
func (s *PostgresStorage) GetChannelSubscription(ctx context.Context, instanceID, channelName string) (*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT id, instance_id, channel, mode, timeout_at, COALESCE(activity_id, ''), subscribed_at
		FROM channel_subscriptions
		WHERE instance_id = $1 AND channel = $2
	`, instanceID, channelName)

	var sub ChannelSubscription
	var modeStr string
	var timeoutAt sql.NullTime
	err := row.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.SubscribedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sub.Mode = ChannelMode(modeStr)
	if timeoutAt.Valid {
		sub.TimeoutAt = &timeoutAt.Time
	}
	return &sub, nil
}

// RegisterChannelReceiveAndReleaseLock atomically registers a channel receive wait and releases the lock.
func (s *PostgresStorage) RegisterChannelReceiveAndReleaseLock(ctx context.Context, instanceID, channelName, workerID, activityID string, timeoutAt *time.Time) error {
	conn := s.getConn(ctx)

	// Update subscription to set timeout and activity ID
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET timeout_at = $1, activity_id = $2
		WHERE instance_id = $3 AND channel = $4
	`, timeoutAt, activityID, instanceID, channelName)
	if err != nil {
		return err
	}

	// Update instance status
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = $1, updated_at = NOW()
		WHERE instance_id = $2
	`, StatusWaitingForMessage, instanceID)
	if err != nil {
		return err
	}

	// Release lock
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
		WHERE instance_id = $1 AND locked_by = $2
	`, instanceID, workerID)
	return err
}

// GetPendingChannelMessages retrieves pending messages for a channel after a given ID.
func (s *PostgresStorage) GetPendingChannelMessages(ctx context.Context, channelName string, afterID int64, limit int) ([]*ChannelMessage, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, channel, data, data_binary, metadata, published_at
		FROM channel_messages
		WHERE channel = $1 AND id > $2
		ORDER BY id ASC
		LIMIT $3
	`, channelName, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []*ChannelMessage
	for rows.Next() {
		var msg ChannelMessage
		var data, metadata sql.NullString
		var dataBinary []byte
		err := rows.Scan(&msg.ID, &msg.Channel, &data, &dataBinary, &metadata, &msg.PublishedAt)
		if err != nil {
			return nil, err
		}
		if data.Valid {
			msg.Data = []byte(data.String)
		}
		msg.DataBinary = dataBinary
		if metadata.Valid {
			msg.Metadata = []byte(metadata.String)
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

// GetPendingChannelMessagesForInstance gets pending messages for a specific subscriber.
// For broadcast mode: Returns messages with id > cursor (messages not yet seen by this instance)
// For competing mode: Returns unclaimed messages (not yet claimed by any instance)
func (s *PostgresStorage) GetPendingChannelMessagesForInstance(ctx context.Context, instanceID, channelName string) ([]*ChannelMessage, error) {
	conn := s.getConn(ctx)

	// First, get the subscription to determine mode
	var mode string
	err := conn.QueryRowContext(ctx, `
		SELECT mode FROM channel_subscriptions
		WHERE instance_id = $1 AND channel = $2
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
			SELECT COALESCE(last_delivered_id, 0) FROM channel_delivery_cursors
			WHERE instance_id = $1 AND channel = $2
		`, instanceID, channelName).Scan(&cursorID)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}

		query = `
			SELECT id, channel, data, data_binary, metadata, published_at
			FROM channel_messages
			WHERE channel = $1 AND id > $2
			ORDER BY id ASC
			LIMIT 10
		`
		args = []any{channelName, cursorID}
	} else {
		// Competing mode: Get unclaimed messages
		query = `
			SELECT m.id, m.channel, m.data, m.data_binary, m.metadata, m.published_at
			FROM channel_messages m
			LEFT JOIN channel_message_claims c ON m.message_id = c.message_id
			WHERE m.channel = $1 AND c.message_id IS NULL
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
		var data, metadata sql.NullString
		var dataBinary []byte
		err := rows.Scan(&msg.ID, &msg.Channel, &data, &dataBinary, &metadata, &msg.PublishedAt)
		if err != nil {
			return nil, err
		}
		if data.Valid {
			msg.Data = []byte(data.String)
		}
		msg.DataBinary = dataBinary
		if metadata.Valid {
			msg.Metadata = []byte(metadata.String)
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

// ClaimChannelMessage claims a message for competing mode.
func (s *PostgresStorage) ClaimChannelMessage(ctx context.Context, messageID int64, instanceID string) (bool, error) {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_message_claims (message_id, instance_id)
		VALUES ($1, $2)
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
func (s *PostgresStorage) DeleteChannelMessage(ctx context.Context, messageID int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `DELETE FROM channel_messages WHERE id = $1`, messageID)
	return err
}

// UpdateDeliveryCursor updates the delivery cursor for broadcast mode.
func (s *PostgresStorage) UpdateDeliveryCursor(ctx context.Context, instanceID, channelName string, lastMessageID int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, channel) DO UPDATE SET last_delivered_id = EXCLUDED.last_delivered_id, updated_at = NOW()
	`, instanceID, channelName, lastMessageID)
	return err
}

// GetDeliveryCursor gets the current delivery cursor for an instance and channel.
func (s *PostgresStorage) GetDeliveryCursor(ctx context.Context, instanceID, channelName string) (int64, error) {
	conn := s.getConn(ctx)
	var lastDeliveredID int64
	err := conn.QueryRowContext(ctx, `
		SELECT last_delivered_id FROM channel_delivery_cursors
		WHERE instance_id = $1 AND channel = $2
	`, instanceID, channelName).Scan(&lastDeliveredID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return lastDeliveredID, err
}

// GetChannelSubscribersWaiting finds subscribers waiting for messages on a channel.
func (s *PostgresStorage) GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, channel, mode, timeout_at, COALESCE(activity_id, ''), subscribed_at
		FROM channel_subscriptions
		WHERE channel = $1 AND activity_id IS NOT NULL
	`, channelName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []*ChannelSubscription
	for rows.Next() {
		var sub ChannelSubscription
		var modeStr string
		var timeoutAt sql.NullTime
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.SubscribedAt)
		if err != nil {
			return nil, err
		}
		sub.Mode = ChannelMode(modeStr)
		if timeoutAt.Valid {
			sub.TimeoutAt = &timeoutAt.Time
		}
		subs = append(subs, &sub)
	}
	return subs, rows.Err()
}

// ClearChannelWaitingState clears the waiting state for an instance's channel subscription.
func (s *PostgresStorage) ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET activity_id = NULL, timeout_at = NULL
		WHERE instance_id = $1 AND channel = $2
	`, instanceID, channelName)
	return err
}

// DeliverChannelMessage delivers a message to a waiting subscriber.
func (s *PostgresStorage) DeliverChannelMessage(ctx context.Context, instanceID string, message *ChannelMessage) error {
	conn := s.getConn(ctx)

	// Record message receipt in history
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, data_type)
		VALUES ($1, $2, 'channel_message_received', $3, 'json')
	`, instanceID, fmt.Sprintf("channel:%s:%d", message.Channel, message.ID), string(message.Data))
	if err != nil {
		return err
	}

	// Clear waiting state
	return s.ClearChannelWaitingState(ctx, instanceID, message.Channel)
}

// DeliverChannelMessageWithLock delivers a message using Lock-First pattern.
// Returns nil result if lock could not be acquired (another worker will handle it).
func (s *PostgresStorage) DeliverChannelMessageWithLock(
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
		SELECT workflow_name FROM workflow_instances WHERE instance_id = $1
	`, instanceID).Scan(&workflowName)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get workflow info: %w", err)
	}

	// Get the activity ID and mode from the subscription (stored when Receive was called)
	var historyActivityID, subscriptionMode string
	err = conn.QueryRowContext(ctx, `
		SELECT COALESCE(activity_id, ''), mode FROM channel_subscriptions
		WHERE instance_id = $1 AND channel = $2
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
		"channel":      channelName,
		"published_at": message.PublishedAt,
	}
	// The message.Data contains the actual data - unmarshal and re-wrap
	var msgData any
	if len(message.Data) > 0 {
		_ = json.Unmarshal(message.Data, &msgData)
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
		VALUES ($1, $2, 'activity_completed', $3, 'json')
	`, instanceID, historyActivityID, string(wrappedJSON))
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to record history: %w", err)
	}

	// Step 3.5: Update delivery cursor for broadcast mode
	// This prevents duplicate message delivery when workflow resumes
	if subscriptionMode == string(ChannelModeBroadcast) {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (instance_id, channel)
			DO UPDATE SET last_delivered_id = EXCLUDED.last_delivered_id, updated_at = NOW()
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
		UPDATE workflow_instances SET status = $1, updated_at = NOW()
		WHERE instance_id = $2
	`, StatusRunning, instanceID)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to update status: %w", err)
	}

	// Step 6: Release lock (workflow will be picked up by resumption task)
	if err := s.ReleaseLock(ctx, instanceID, workerID); err != nil {
		return nil, fmt.Errorf("failed to release lock: %w", err)
	}

	// Send notification that workflow is resumable
	wfPayload := map[string]string{
		"instance_id":   instanceID,
		"workflow_name": workflowName,
	}
	if payloadBytes, err := json.Marshal(wfPayload); err == nil {
		s.sendNotify(ctx, notify.ChannelWorkflowResumable, string(payloadBytes))
	}

	return &ChannelDeliveryResult{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		ActivityID:   historyActivityID,
	}, nil
}

// CleanupOldChannelMessages removes old channel messages.
func (s *PostgresStorage) CleanupOldChannelMessages(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_messages WHERE published_at < $1
	`, threshold)
	return err
}

// FindExpiredChannelSubscriptions finds channel subscriptions that have timed out.
func (s *PostgresStorage) FindExpiredChannelSubscriptions(ctx context.Context, limit int) ([]*ChannelSubscription, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT cs.id, cs.instance_id, cs.channel, cs.mode, cs.timeout_at, COALESCE(cs.activity_id, ''), cs.subscribed_at
		FROM channel_subscriptions cs
		JOIN workflow_instances w ON cs.instance_id = w.instance_id
		WHERE cs.activity_id IS NOT NULL AND cs.timeout_at IS NOT NULL AND cs.timeout_at < NOW()
		AND w.framework = 'go'
		ORDER BY cs.timeout_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []*ChannelSubscription
	for rows.Next() {
		var sub ChannelSubscription
		var modeStr string
		var timeoutAt sql.NullTime
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.SubscribedAt)
		if err != nil {
			return nil, err
		}
		sub.Mode = ChannelMode(modeStr)
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
func (s *PostgresStorage) JoinGroup(ctx context.Context, instanceID, groupName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_group_memberships (instance_id, group_name)
		VALUES ($1, $2)
		ON CONFLICT (instance_id, group_name) DO NOTHING
	`, instanceID, groupName)
	return err
}

// LeaveGroup removes an instance from a group.
func (s *PostgresStorage) LeaveGroup(ctx context.Context, instanceID, groupName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_group_memberships WHERE instance_id = $1 AND group_name = $2
	`, instanceID, groupName)
	return err
}

// GetGroupMembers retrieves all instance IDs in a group.
func (s *PostgresStorage) GetGroupMembers(ctx context.Context, groupName string) ([]string, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT instance_id FROM workflow_group_memberships WHERE group_name = $1
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
func (s *PostgresStorage) LeaveAllGroups(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_group_memberships WHERE instance_id = $1
	`, instanceID)
	return err
}

// ========================================
// System Lock Manager
// ========================================

// TryAcquireSystemLock attempts to acquire a system lock.
func (s *PostgresStorage) TryAcquireSystemLock(ctx context.Context, lockName, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)

	// Try to insert or update if expired or same worker
	result, err := conn.ExecContext(ctx, `
		INSERT INTO system_locks (lock_name, locked_by, locked_at, lock_expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (lock_name) DO UPDATE SET
			locked_by = EXCLUDED.locked_by,
			locked_at = EXCLUDED.locked_at,
			lock_expires_at = EXCLUDED.lock_expires_at
		WHERE system_locks.lock_expires_at < NOW() OR system_locks.locked_by = EXCLUDED.locked_by
	`, lockName, workerID, now, expiresAt)
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
func (s *PostgresStorage) ReleaseSystemLock(ctx context.Context, lockName, workerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM system_locks WHERE lock_name = $1 AND locked_by = $2
	`, lockName, workerID)
	return err
}

// CleanupExpiredSystemLocks removes expired system locks.
func (s *PostgresStorage) CleanupExpiredSystemLocks(ctx context.Context) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM system_locks WHERE lock_expires_at < NOW()
	`)
	return err
}

// ========================================
// History Archive Manager
// ========================================

// ArchiveHistory moves all history events for an instance to the archive table.
func (s *PostgresStorage) ArchiveHistory(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)

	// Copy to archive
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history_archive (original_id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, original_created_at)
		SELECT id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at
		FROM workflow_history
		WHERE instance_id = $1
	`, instanceID)
	if err != nil {
		return err
	}

	// Delete from original
	_, err = conn.ExecContext(ctx, `
		DELETE FROM workflow_history WHERE instance_id = $1
	`, instanceID)
	return err
}

// GetArchivedHistory retrieves archived history events for an instance.
func (s *PostgresStorage) GetArchivedHistory(ctx context.Context, instanceID string) ([]*ArchivedHistoryEvent, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, original_id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, original_created_at, archived_at
		FROM workflow_history_archive
		WHERE instance_id = $1
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
// Note: workflow_event_subscriptions was removed in schema unification;
// events now use channel subscriptions.
func (s *PostgresStorage) CleanupInstanceSubscriptions(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)

	// Remove timer subscriptions
	_, err := conn.ExecContext(ctx, `DELETE FROM workflow_timer_subscriptions WHERE instance_id = $1`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to delete timer subscriptions: %w", err)
	}

	// Remove channel subscriptions (includes event subscriptions since WaitEvent uses channels)
	_, err = conn.ExecContext(ctx, `DELETE FROM channel_subscriptions WHERE instance_id = $1`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to delete channel subscriptions: %w", err)
	}

	// Remove group memberships
	_, err = conn.ExecContext(ctx, `DELETE FROM workflow_group_memberships WHERE instance_id = $1`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to delete group memberships: %w", err)
	}

	return nil
}
