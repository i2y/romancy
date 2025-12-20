// Package storage provides the storage layer for Romancy.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// MySQLStorage implements the Storage interface using MySQL 8.0+.
type MySQLStorage struct {
	db     *sql.DB
	driver Driver
}

// NewMySQLStorage creates a new MySQL storage.
// The connStr can be either a MySQL DSN or a URL format:
// URL format: "mysql://user:password@localhost:3306/dbname"
// DSN format: "user:password@tcp(localhost:3306)/dbname"
func NewMySQLStorage(connStr string) (*MySQLStorage, error) {
	dsn, err := convertToMySQLDSN(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &MySQLStorage{
		db:     db,
		driver: &MySQLDriver{},
	}, nil
}

// convertToMySQLDSN converts a MySQL URL to DSN format.
// Input: mysql://user:password@host:port/dbname?param=value
// Output: user:password@tcp(host:port)/dbname?parseTime=true&loc=UTC&multiStatements=true&param=value
func convertToMySQLDSN(connStr string) (string, error) {
	// If it's already in DSN format (doesn't start with mysql://), use it directly
	if !strings.HasPrefix(connStr, "mysql://") {
		// Ensure required params are set
		if !strings.Contains(connStr, "parseTime=") {
			if strings.Contains(connStr, "?") {
				connStr += "&parseTime=true"
			} else {
				connStr += "?parseTime=true"
			}
		}
		if !strings.Contains(connStr, "loc=") {
			connStr += "&loc=UTC"
		}
		if !strings.Contains(connStr, "multiStatements=") {
			connStr += "&multiStatements=true"
		}
		return connStr, nil
	}

	// Parse URL format
	u, err := url.Parse(connStr)
	if err != nil {
		return "", err
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":3306" // Default MySQL port
	}

	var userInfo string
	if u.User != nil {
		password, _ := u.User.Password()
		userInfo = u.User.Username() + ":" + password + "@"
	}

	dbName := strings.TrimPrefix(u.Path, "/")

	// Build DSN
	dsn := fmt.Sprintf("%stcp(%s)/%s", userInfo, host, dbName)

	// Add query parameters
	params := u.Query()
	if params.Get("parseTime") == "" {
		params.Set("parseTime", "true")
	}
	if params.Get("loc") == "" {
		params.Set("loc", "UTC")
	}
	if params.Get("multiStatements") == "" {
		params.Set("multiStatements", "true")
	}
	if len(params) > 0 {
		dsn += "?" + params.Encode()
	}

	return dsn, nil
}

// Initialize is deprecated. Use dbmate for migrations:
// dbmate -d schema/db/migrations/mysql up
func (s *MySQLStorage) Initialize(ctx context.Context) error {
	slog.Warn("Initialize() is deprecated. Use dbmate for migrations: dbmate -d schema/db/migrations/mysql up")
	return nil
}

// DB returns the underlying database connection.
func (s *MySQLStorage) DB() *sql.DB {
	return s.db
}

// Close closes the database connection.
func (s *MySQLStorage) Close() error {
	return s.db.Close()
}

// getConn returns the appropriate database handle based on context.
func (s *MySQLStorage) getConn(ctx context.Context) Executor {
	if state, ok := ctx.Value(txKey{}).(*txState); ok {
		return state.tx
	}
	return s.db
}

// --- Transaction Manager ---

// BeginTransaction starts a new transaction.
func (s *MySQLStorage) BeginTransaction(ctx context.Context) (context.Context, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ctx, err
	}
	state := &txState{tx: tx}
	return context.WithValue(ctx, txKey{}, state), nil
}

// CommitTransaction commits the current transaction.
func (s *MySQLStorage) CommitTransaction(ctx context.Context) error {
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
func (s *MySQLStorage) RollbackTransaction(ctx context.Context) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return nil
	}
	return state.tx.Rollback()
}

// InTransaction returns whether a transaction is in progress.
func (s *MySQLStorage) InTransaction(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{}).(*txState)
	return ok
}

// Conn returns the database executor for the current context.
func (s *MySQLStorage) Conn(ctx context.Context) Executor {
	return s.getConn(ctx)
}

// RegisterPostCommitCallback registers a callback to be executed after a successful commit.
// Returns an error if not currently in a transaction.
func (s *MySQLStorage) RegisterPostCommitCallback(ctx context.Context, cb func() error) error {
	state, ok := ctx.Value(txKey{}).(*txState)
	if !ok {
		return fmt.Errorf("not in a transaction")
	}
	state.callbacks = append(state.callbacks, cb)
	return nil
}

// --- Instance Manager ---

// CreateInstance creates a new workflow instance.
func (s *MySQLStorage) CreateInstance(ctx context.Context, instance *WorkflowInstance) error {
	conn := s.getConn(ctx)

	// Ensure workflow definition exists (required by foreign key constraint)
	// Uses INSERT IGNORE for idempotency
	_, err := conn.ExecContext(ctx, `
		INSERT IGNORE INTO workflow_definitions (workflow_name, source_hash, source_code)
		VALUES (?, ?, ?)
	`, instance.WorkflowName, instance.SourceHash, "")
	if err != nil {
		return fmt.Errorf("failed to ensure workflow definition: %w", err)
	}

	_, err = conn.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			instance_id, workflow_name, status, input_data, source_hash, owner_service, framework, started_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, instance.InstanceID, instance.WorkflowName, instance.Status,
		string(instance.InputData), instance.SourceHash, instance.OwnerService, "go",
		instance.StartedAt.UTC(), instance.UpdatedAt.UTC())
	return err
}

// GetInstance retrieves a workflow instance by ID.
func (s *MySQLStorage) GetInstance(ctx context.Context, instanceID string) (*WorkflowInstance, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, status, input_data, output_data,
			   current_activity_id, source_hash, owner_service, framework,
			   locked_by, locked_at, lock_timeout_seconds, lock_expires_at,
			   started_at, updated_at
		FROM workflow_instances WHERE instance_id = ?
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
func (s *MySQLStorage) UpdateInstanceStatus(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, updated_at = NOW()
		WHERE instance_id = ?
	`, status, instanceID)
	return err
}

// UpdateInstanceActivity updates the current activity ID.
func (s *MySQLStorage) UpdateInstanceActivity(ctx context.Context, instanceID, activityID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET current_activity_id = ?, updated_at = NOW()
		WHERE instance_id = ?
	`, activityID, instanceID)
	return err
}

// UpdateInstanceOutput updates the output data.
func (s *MySQLStorage) UpdateInstanceOutput(ctx context.Context, instanceID string, outputData []byte) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET output_data = ?, status = 'completed', updated_at = NOW()
		WHERE instance_id = ?
	`, string(outputData), instanceID)
	return err
}

// CancelInstance cancels a workflow instance.
// Returns ErrWorkflowNotCancellable if the workflow is already completed, cancelled, failed, or does not exist.
func (s *MySQLStorage) CancelInstance(ctx context.Context, instanceID, reason string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'cancelled', updated_at = NOW()
		WHERE instance_id = ? AND status IN ('pending', 'running', 'waiting_for_event', 'waiting_for_timer', 'waiting_for_message', 'recurred')
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
func (s *MySQLStorage) ListInstances(ctx context.Context, opts ListInstancesOptions) (*PaginationResult, error) {
	conn := s.getConn(ctx)
	query := "SELECT instance_id, workflow_name, status, started_at, updated_at FROM workflow_instances WHERE framework = 'go'"
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
		query += " AND started_at > ?"
		args = append(args, opts.StartedAfter.UTC())
	}
	if opts.StartedBefore != nil {
		query += " AND started_at < ?"
		args = append(args, opts.StartedBefore.UTC())
	}

	// Handle input filters
	if len(opts.InputFilters) > 0 {
		filterBuilder := NewInputFilterBuilder(s.driver)
		filterConditions, filterArgs, err := filterBuilder.BuildFilterQuery(opts.InputFilters, len(args)+1)
		if err != nil {
			return nil, fmt.Errorf("invalid input filter: %w", err)
		}
		for _, cond := range filterConditions {
			query += " AND " + cond
		}
		args = append(args, filterArgs...)
	}

	// Parse cursor token (format: "ISO_DATETIME||INSTANCE_ID")
	if opts.PageToken != "" {
		parts := strings.SplitN(opts.PageToken, "||", 2)
		if len(parts) == 2 {
			cursorTime, err := time.Parse(time.RFC3339Nano, parts[0])
			if err == nil {
				cursorID := parts[1]
				// For descending order: get rows where (started_at, instance_id) < (cursor_time, cursor_id)
				query += " AND (started_at < ? OR (started_at = ? AND instance_id < ?))"
				args = append(args, cursorTime.UTC(), cursorTime.UTC(), cursorID)
			}
		}
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50 // Default page size
	}
	// Fetch one extra to determine if there are more pages
	query += " ORDER BY started_at DESC, instance_id DESC LIMIT ?"
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
func (s *MySQLStorage) FindResumableWorkflows(ctx context.Context, limit int) ([]*ResumableWorkflow, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	query := `
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE status = ?
		AND (locked_by IS NULL OR locked_by = '')
		ORDER BY updated_at ASC
		LIMIT ?
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
func (s *MySQLStorage) TryAcquireLock(ctx context.Context, instanceID, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)

	// Allow acquiring if:
	// 1. No lock exists (locked_by IS NULL)
	// 2. Lock is expired (lock_expires_at < now)
	// 3. Same worker already holds the lock (re-entrant)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = ?, locked_at = ?, lock_expires_at = ?, updated_at = ?
		WHERE instance_id = ?
		AND (locked_by IS NULL OR lock_expires_at < NOW() OR locked_by = ?)
	`, workerID, now, expiresAt, now, instanceID, workerID)
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
func (s *MySQLStorage) ReleaseLock(ctx context.Context, instanceID, workerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
		WHERE instance_id = ? AND locked_by = ?
	`, instanceID, workerID)
	return err
}

// RefreshLock extends the lock expiration time.
func (s *MySQLStorage) RefreshLock(ctx context.Context, instanceID, workerID string, timeoutSec int) error {
	conn := s.getConn(ctx)
	expiresAt := time.Now().UTC().Add(time.Duration(timeoutSec) * time.Second)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET lock_expires_at = ?, updated_at = NOW()
		WHERE instance_id = ? AND locked_by = ?
	`, expiresAt, instanceID, workerID)
	return err
}

// CleanupStaleLocks cleans up expired locks and returns workflows to resume.
func (s *MySQLStorage) CleanupStaleLocks(ctx context.Context, timeoutSec int) ([]StaleWorkflowInfo, error) {
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
func (s *MySQLStorage) AppendHistory(ctx context.Context, event *HistoryEvent) error {
	conn := s.getConn(ctx)
	// MySQL uses INSERT IGNORE instead of ON CONFLICT DO NOTHING
	_, err := conn.ExecContext(ctx, `
		INSERT IGNORE INTO workflow_history (instance_id, activity_id, event_type, event_data, event_data_binary, data_type)
		VALUES (?, ?, ?, ?, ?, ?)
	`, event.InstanceID, event.ActivityID, event.EventType, string(event.EventData), event.EventDataBinary, event.DataType)
	return err
}

// GetHistoryPaginated retrieves history events with pagination.
// Returns events with id > afterID, up to limit events.
// Returns (events, hasMore, error).
func (s *MySQLStorage) GetHistoryPaginated(ctx context.Context, instanceID string, afterID int64, limit int) ([]*HistoryEvent, bool, error) {
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
func (s *MySQLStorage) GetHistoryCount(ctx context.Context, instanceID string) (int64, error) {
	conn := s.getConn(ctx)
	var count int64
	err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_history WHERE instance_id = ?
	`, instanceID).Scan(&count)
	return count, err
}

// --- Timer Subscription Manager ---

// RegisterTimerSubscription registers a timer for a workflow.
func (s *MySQLStorage) RegisterTimerSubscription(ctx context.Context, sub *TimerSubscription) error {
	conn := s.getConn(ctx)
	// MySQL uses INSERT IGNORE instead of ON CONFLICT DO NOTHING
	_, err := conn.ExecContext(ctx, `
		INSERT IGNORE INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, activity_id)
		VALUES (?, ?, ?, ?)
	`, sub.InstanceID, sub.TimerID, sub.ExpiresAt.UTC(), sub.ActivityID)
	return err
}

// RegisterTimerSubscriptionAndReleaseLock atomically registers timer and releases lock.
func (s *MySQLStorage) RegisterTimerSubscriptionAndReleaseLock(ctx context.Context, sub *TimerSubscription, instanceID, workerID string) error {
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
		INSERT IGNORE INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, activity_id)
		VALUES (?, ?, ?, ?)
	`, sub.InstanceID, sub.TimerID, sub.ExpiresAt.UTC(), sub.ActivityID)
	if err != nil {
		return err
	}

	// Update status
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'waiting_for_timer', updated_at = NOW()
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
func (s *MySQLStorage) RemoveTimerSubscription(ctx context.Context, instanceID, timerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_timer_subscriptions WHERE instance_id = ? AND timer_id = ?
	`, instanceID, timerID)
	return err
}

// FindExpiredTimers finds expired timers.
func (s *MySQLStorage) FindExpiredTimers(ctx context.Context, limit int) ([]*TimerSubscription, error) {
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
		LIMIT ?
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
func (s *MySQLStorage) AddOutboxEvent(ctx context.Context, event *OutboxEvent) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO outbox_events (event_id, event_type, event_source, event_data, data_type, content_type, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, event.EventID, event.EventType, event.EventSource, string(event.EventData), event.DataType, event.ContentType, event.Status)
	return err
}

// GetPendingOutboxEvents retrieves pending outbox events.
// Uses SELECT FOR UPDATE SKIP LOCKED for concurrent workers.
func (s *MySQLStorage) GetPendingOutboxEvents(ctx context.Context, limit int) ([]*OutboxEvent, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT event_id, event_type, event_source, event_data, data_type, content_type, status, retry_count, created_at
		FROM outbox_events
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT ?
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
func (s *MySQLStorage) MarkOutboxEventSent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'published', published_at = NOW()
		WHERE event_id = ?
	`, eventID)
	return err
}

// MarkOutboxEventFailed marks an outbox event as failed.
func (s *MySQLStorage) MarkOutboxEventFailed(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'failed'
		WHERE event_id = ?
	`, eventID)
	return err
}

// IncrementOutboxAttempts increments the attempt count for an event.
func (s *MySQLStorage) IncrementOutboxAttempts(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET retry_count = retry_count + 1
		WHERE event_id = ?
	`, eventID)
	return err
}

// CleanupOldOutboxEvents removes old published events.
func (s *MySQLStorage) CleanupOldOutboxEvents(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM outbox_events
		WHERE status = 'published' AND created_at < ?
	`, threshold)
	return err
}

// --- Compensation Manager ---

// AddCompensation adds a compensation entry.
func (s *MySQLStorage) AddCompensation(ctx context.Context, entry *CompensationEntry) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_compensations (instance_id, activity_id, activity_name, args)
		VALUES (?, ?, ?, ?)
	`, entry.InstanceID, entry.ActivityID, entry.ActivityName, string(entry.Args))
	return err
}

// GetCompensations retrieves compensations for a workflow in LIFO order.
func (s *MySQLStorage) GetCompensations(ctx context.Context, instanceID string) ([]*CompensationEntry, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, activity_id, activity_name, args, created_at
		FROM workflow_compensations
		WHERE instance_id = ?
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
func (s *MySQLStorage) PublishToChannel(ctx context.Context, channelName string, dataJSON, metadata []byte) (int64, error) {
	conn := s.getConn(ctx)
	var dataStr, metadataStr sql.NullString
	if dataJSON != nil {
		dataStr = sql.NullString{String: string(dataJSON), Valid: true}
	}
	if metadata != nil {
		metadataStr = sql.NullString{String: string(metadata), Valid: true}
	}

	// MySQL doesn't support RETURNING, use LastInsertId instead
	// Must include message_id (required NOT NULL field) and data_type
	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_messages (channel, data, metadata, message_id, data_type)
		VALUES (?, ?, ?, UUID(), 'json')
	`, channelName, dataStr, metadataStr)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SubscribeToChannel subscribes an instance to a channel.
// For broadcast mode, it initializes the delivery cursor to the current max message ID
// so that only messages published after subscription are received.
func (s *MySQLStorage) SubscribeToChannel(ctx context.Context, instanceID, channelName string, mode ChannelMode) error {
	conn := s.getConn(ctx)

	// MySQL uses ON DUPLICATE KEY UPDATE instead of ON CONFLICT
	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_subscriptions (instance_id, channel, mode)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE mode = VALUES(mode)
	`, instanceID, channelName, string(mode))
	if err != nil {
		return err
	}

	// For broadcast mode, initialize the delivery cursor to current max message ID
	// This ensures new subscribers only receive messages published after subscription
	if mode == ChannelModeBroadcast {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
			SELECT ?, ?, COALESCE(MAX(id), 0)
			FROM channel_messages
			WHERE channel = ?
			ON DUPLICATE KEY UPDATE instance_id = instance_id
		`, instanceID, channelName, channelName)
		if err != nil {
			return err
		}
	}

	return nil
}

// UnsubscribeFromChannel unsubscribes an instance from a channel.
func (s *MySQLStorage) UnsubscribeFromChannel(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_subscriptions WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName)
	return err
}

// GetChannelSubscription retrieves a subscription for an instance and channel.
func (s *MySQLStorage) GetChannelSubscription(ctx context.Context, instanceID, channelName string) (*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT id, instance_id, channel, mode, timeout_at, COALESCE(activity_id, ''), COALESCE(cursor_message_id, 0), subscribed_at
		FROM channel_subscriptions
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName)

	var sub ChannelSubscription
	var modeStr string
	var timeoutAt sql.NullTime
	err := row.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.CursorMessageID, &sub.SubscribedAt)
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
func (s *MySQLStorage) RegisterChannelReceiveAndReleaseLock(ctx context.Context, instanceID, channelName, workerID, activityID string, timeoutAt *time.Time) error {
	conn := s.getConn(ctx)

	// Update subscription with activity_id
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET timeout_at = ?, activity_id = ?
		WHERE instance_id = ? AND channel = ?
	`, timeoutAt, activityID, instanceID, channelName)
	if err != nil {
		return err
	}

	// Update instance status
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, updated_at = NOW()
		WHERE instance_id = ?
	`, StatusWaitingForMessage, instanceID)
	if err != nil {
		return err
	}

	// Release lock
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
		WHERE instance_id = ? AND locked_by = ?
	`, instanceID, workerID)
	return err
}

// GetPendingChannelMessages retrieves pending messages for a channel after a given ID.
func (s *MySQLStorage) GetPendingChannelMessages(ctx context.Context, channelName string, afterID int64, limit int) ([]*ChannelMessage, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, channel, data, data_binary, metadata, published_at
		FROM channel_messages
		WHERE channel = ? AND id > ?
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
func (s *MySQLStorage) GetPendingChannelMessagesForInstance(ctx context.Context, instanceID, channelName string) ([]*ChannelMessage, error) {
	conn := s.getConn(ctx)

	// First, get the subscription to determine mode
	var mode string
	err := conn.QueryRowContext(ctx, `
		SELECT mode FROM channel_subscriptions
		WHERE instance_id = ? AND channel = ?
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
			WHERE instance_id = ? AND channel = ?
		`, instanceID, channelName).Scan(&cursorID)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}

		query = `
			SELECT id, channel, data, data_binary, metadata, published_at
			FROM channel_messages
			WHERE channel = ? AND id > ?
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
			WHERE m.channel = ? AND c.message_id IS NULL
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
func (s *MySQLStorage) ClaimChannelMessage(ctx context.Context, messageID int64, instanceID string) (bool, error) {
	conn := s.getConn(ctx)
	// MySQL uses INSERT IGNORE instead of ON CONFLICT DO NOTHING
	result, err := conn.ExecContext(ctx, `
		INSERT IGNORE INTO channel_message_claims (message_id, instance_id)
		VALUES (?, ?)
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
func (s *MySQLStorage) DeleteChannelMessage(ctx context.Context, messageID int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `DELETE FROM channel_messages WHERE id = ?`, messageID)
	return err
}

// UpdateDeliveryCursor updates the delivery cursor for broadcast mode.
func (s *MySQLStorage) UpdateDeliveryCursor(ctx context.Context, instanceID, channelName string, lastMessageID int64) error {
	conn := s.getConn(ctx)
	// MySQL uses ON DUPLICATE KEY UPDATE instead of ON CONFLICT
	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE last_delivered_id = VALUES(last_delivered_id), updated_at = NOW()
	`, instanceID, channelName, lastMessageID)
	return err
}

// GetDeliveryCursor gets the current delivery cursor for an instance and channel.
func (s *MySQLStorage) GetDeliveryCursor(ctx context.Context, instanceID, channelName string) (int64, error) {
	conn := s.getConn(ctx)
	var lastDeliveredID int64
	err := conn.QueryRowContext(ctx, `
		SELECT last_delivered_id FROM channel_delivery_cursors
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName).Scan(&lastDeliveredID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return lastDeliveredID, err
}

// GetChannelSubscribersWaiting finds subscribers waiting for messages on a channel.
// Returns all waiting subscribers regardless of framework - delivery is handled by Lock-First pattern.
func (s *MySQLStorage) GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, channel, mode, timeout_at, COALESCE(activity_id, ''), COALESCE(cursor_message_id, 0), subscribed_at
		FROM channel_subscriptions
		WHERE channel = ? AND activity_id IS NOT NULL
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
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.CursorMessageID, &sub.SubscribedAt)
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
func (s *MySQLStorage) ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET activity_id = NULL, timeout_at = NULL
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName)
	return err
}

// DeliverChannelMessage delivers a message to a waiting subscriber.
func (s *MySQLStorage) DeliverChannelMessage(ctx context.Context, instanceID string, message *ChannelMessage) error {
	conn := s.getConn(ctx)

	// Record message receipt in history
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, data_type)
		VALUES (?, ?, 'channel_message_received', ?, 'json')
	`, instanceID, fmt.Sprintf("channel:%s:%d", message.Channel, message.ID), string(message.Data))
	if err != nil {
		return err
	}

	// Clear waiting state
	return s.ClearChannelWaitingState(ctx, instanceID, message.Channel)
}

// DeliverChannelMessageWithLock delivers a message using Lock-First pattern.
// Returns nil result if lock could not be acquired (another worker will handle it).
func (s *MySQLStorage) DeliverChannelMessageWithLock(
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

	// Step 2: Get workflow info for the result
	var workflowName string
	err = conn.QueryRowContext(ctx, `
		SELECT workflow_name FROM workflow_instances WHERE instance_id = ?
	`, instanceID).Scan(&workflowName)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get workflow info: %w", err)
	}

	// Step 3: Get the activity_id and mode from the subscription for replay matching
	var activityID, subscriptionMode string
	err = conn.QueryRowContext(ctx, `
		SELECT COALESCE(activity_id, ''), mode FROM channel_subscriptions
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName).Scan(&activityID, &subscriptionMode)
	if err != nil && err != sql.ErrNoRows {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get subscription info: %w", err)
	}

	// Step 4: Record message in history using the activity_id from subscription
	// Wrap the message in ReceivedMessage format so Receive can unmarshal it correctly
	wrappedData := map[string]any{
		"id":           message.ID,
		"channel_name": channelName,
		"created_at":   message.PublishedAt,
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
		VALUES (?, ?, 'ChannelMessageReceived', ?, 'json')
	`, instanceID, activityID, string(wrappedJSON))
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to record history: %w", err)
	}

	// Step 4.5: Update delivery cursor for broadcast mode
	// This prevents duplicate message delivery when workflow resumes
	if subscriptionMode == string(ChannelModeBroadcast) {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
			VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE last_delivered_id = VALUES(last_delivered_id), updated_at = NOW()
		`, instanceID, channelName, message.ID)
		if err != nil {
			_ = s.ReleaseLock(ctx, instanceID, workerID)
			return nil, fmt.Errorf("failed to update delivery cursor: %w", err)
		}
	}

	// Step 5: Clear waiting state
	if err := s.ClearChannelWaitingState(ctx, instanceID, channelName); err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to clear waiting state: %w", err)
	}

	// Step 6: Update status to 'running'
	_, err = conn.ExecContext(ctx, `
		UPDATE workflow_instances SET status = ?, updated_at = NOW()
		WHERE instance_id = ?
	`, StatusRunning, instanceID)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to update status: %w", err)
	}

	// Step 7: Release lock (workflow will be picked up by resumption task)
	if err := s.ReleaseLock(ctx, instanceID, workerID); err != nil {
		return nil, fmt.Errorf("failed to release lock: %w", err)
	}

	return &ChannelDeliveryResult{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		ActivityID:   activityID,
	}, nil
}

// CleanupOldChannelMessages removes old channel messages.
func (s *MySQLStorage) CleanupOldChannelMessages(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_messages WHERE published_at < ?
	`, threshold)
	return err
}

// FindExpiredChannelSubscriptions finds channel subscriptions that have timed out.
func (s *MySQLStorage) FindExpiredChannelSubscriptions(ctx context.Context, limit int) ([]*ChannelSubscription, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT cs.id, cs.instance_id, cs.channel, cs.mode, cs.timeout_at, COALESCE(cs.activity_id, ''), COALESCE(cs.cursor_message_id, 0), cs.subscribed_at
		FROM channel_subscriptions cs
		JOIN workflow_instances w ON cs.instance_id = w.instance_id
		WHERE cs.activity_id IS NOT NULL AND cs.timeout_at IS NOT NULL AND cs.timeout_at < NOW()
		AND w.framework = 'go'
		ORDER BY cs.timeout_at ASC
		LIMIT ?
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
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.CursorMessageID, &sub.SubscribedAt)
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
func (s *MySQLStorage) JoinGroup(ctx context.Context, instanceID, groupName string) error {
	conn := s.getConn(ctx)
	// MySQL uses INSERT IGNORE instead of ON CONFLICT DO NOTHING
	_, err := conn.ExecContext(ctx, `
		INSERT IGNORE INTO workflow_group_memberships (instance_id, group_name)
		VALUES (?, ?)
	`, instanceID, groupName)
	return err
}

// LeaveGroup removes an instance from a group.
func (s *MySQLStorage) LeaveGroup(ctx context.Context, instanceID, groupName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM workflow_group_memberships WHERE instance_id = ? AND group_name = ?
	`, instanceID, groupName)
	return err
}

// GetGroupMembers retrieves all instance IDs in a group.
func (s *MySQLStorage) GetGroupMembers(ctx context.Context, groupName string) ([]string, error) {
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
func (s *MySQLStorage) LeaveAllGroups(ctx context.Context, instanceID string) error {
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
func (s *MySQLStorage) TryAcquireSystemLock(ctx context.Context, lockName, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)

	// MySQL uses ON DUPLICATE KEY UPDATE with conditional logic
	// The INSERT will succeed if the lock doesn't exist
	// The UPDATE will only happen if the lock is expired or owned by the same worker
	result, err := conn.ExecContext(ctx, `
		INSERT INTO system_locks (lock_name, locked_by, locked_at, lock_expires_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			locked_by = IF(lock_expires_at < NOW() OR locked_by = VALUES(locked_by), VALUES(locked_by), locked_by),
			locked_at = IF(lock_expires_at < NOW() OR locked_by = VALUES(locked_by), VALUES(locked_at), locked_at),
			lock_expires_at = IF(system_locks.lock_expires_at < NOW() OR locked_by = VALUES(locked_by), VALUES(lock_expires_at), lock_expires_at)
	`, lockName, workerID, now, expiresAt)
	if err != nil {
		return false, err
	}

	// Check if we actually own the lock now
	var currentOwner string
	err = conn.QueryRowContext(ctx, `
		SELECT locked_by FROM system_locks WHERE lock_name = ?
	`, lockName).Scan(&currentOwner)
	if err != nil {
		return false, err
	}

	// We own the lock if the current owner is us
	_ = result // result.RowsAffected() doesn't reliably indicate success for ON DUPLICATE KEY UPDATE
	return currentOwner == workerID, nil
}

// ReleaseSystemLock releases a system lock.
func (s *MySQLStorage) ReleaseSystemLock(ctx context.Context, lockName, workerID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		DELETE FROM system_locks WHERE lock_name = ? AND locked_by = ?
	`, lockName, workerID)
	return err
}

// CleanupExpiredSystemLocks removes expired system locks.
func (s *MySQLStorage) CleanupExpiredSystemLocks(ctx context.Context) error {
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
func (s *MySQLStorage) ArchiveHistory(ctx context.Context, instanceID string) error {
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
func (s *MySQLStorage) GetArchivedHistory(ctx context.Context, instanceID string) ([]*ArchivedHistoryEvent, error) {
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
// Note: workflow_event_subscriptions was removed in schema unification;
// events now use channel subscriptions.
func (s *MySQLStorage) CleanupInstanceSubscriptions(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)

	// Remove timer subscriptions
	_, err := conn.ExecContext(ctx, `DELETE FROM workflow_timer_subscriptions WHERE instance_id = ?`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to delete timer subscriptions: %w", err)
	}

	// Remove channel subscriptions (includes event subscriptions since WaitEvent uses channels)
	_, err = conn.ExecContext(ctx, `DELETE FROM channel_subscriptions WHERE instance_id = ?`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to delete channel subscriptions: %w", err)
	}

	// Remove group memberships
	_, err = conn.ExecContext(ctx, `DELETE FROM workflow_group_memberships WHERE instance_id = ?`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to delete group memberships: %w", err)
	}

	return nil
}
