// Package storage provides the storage layer for Romancy.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	sqlite3 "modernc.org/sqlite"
)

// txKey is the context key for transactions.
type txKey struct{}

// WithoutTransaction returns ctx detached from the transaction it carries,
// if any. Post-commit callbacks use it: the transaction they were registered
// in has ended by the time they run.
func WithoutTransaction(ctx context.Context) context.Context {
	if _, ok := ctx.Value(txKey{}).(*txState); !ok {
		return ctx
	}
	return context.WithValue(ctx, txKey{}, nil)
}

// txState holds transaction state including post-commit callbacks.
type txState struct {
	tx        *sql.Tx
	callbacks []func() error
}

// SQLiteStorage implements the Storage interface using SQLite.
type SQLiteStorage struct {
	db *sql.DB
	// immediate is a second pool on the same database whose transactions
	// begin IMMEDIATE (see beginImmediate).
	immediate *sql.DB
	driver    Driver
}

// NewSQLiteStorage creates a new SQLite storage.
func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	// For in-memory databases, use shared cache mode so all connections share the same database.
	// This is required because database/sql uses connection pooling, and without shared cache,
	// each connection to ":memory:" would get its own separate database.
	connStr := dbPath
	if dbPath == ":memory:" {
		connStr = "file::memory:?cache=shared"
	}
	const sqliteParams = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	connStr += "?" + sqliteParams

	// For shared memory, the connection string already has query params, so use & instead of ?
	if dbPath == ":memory:" {
		connStr = "file::memory:?cache=shared&" + sqliteParams
	}

	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	immediate, err := sql.Open("sqlite", connStr+"&_txlock=immediate")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return &SQLiteStorage{
		db:        db,
		immediate: immediate,
		driver:    &SQLiteDriver{},
	}, nil
}

// Initialize is deprecated. Use dbmate for migrations:
// dbmate -d schema/db/migrations/sqlite up
func (s *SQLiteStorage) Initialize(ctx context.Context) error {
	slog.Warn("Initialize() is deprecated. Use dbmate for migrations: dbmate -d schema/db/migrations/sqlite up")
	return nil
}

// DB returns the underlying database connection.
func (s *SQLiteStorage) DB() *sql.DB {
	return s.db
}

// Close closes the database connection.
func (s *SQLiteStorage) Close() error {
	return errors.Join(s.immediate.Close(), s.db.Close())
}

// parseSQLiteTime parses a SQLite datetime TEXT value into time.Time.
// Handles both "2006-01-02 15:04:05" and "2006-01-02T15:04:05Z" formats.
func parseSQLiteTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	// Try SQLite format first
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err == nil {
		return t, nil
	}
	// Try RFC3339 format
	t, err = time.Parse(time.RFC3339, s)
	if err == nil {
		return t, nil
	}
	// Try RFC3339 without timezone
	t, err = time.Parse("2006-01-02T15:04:05", s)
	if err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time: %s", s)
}

// getConn returns the appropriate database handle based on context (internal use).
func (s *SQLiteStorage) getConn(ctx context.Context) Executor {
	if state, ok := ctx.Value(txKey{}).(*txState); ok {
		return state.tx
	}
	return s.db
}

// --- Transaction Manager ---

// sqliteBusy is SQLITE_BUSY (5), the primary code of SQLITE_BUSY_SNAPSHOT
// (517) and the other extended busy codes.
const sqliteBusy = 5

// IsWriteConflict reports whether err is SQLite's SQLITE_BUSY or one of its
// extended codes: the transaction lost the race for the write lock.
//
// A deferred transaction that reads before it writes, such as an activity
// whose body reads first (FSRead, a query through ctx.Session()), holds a
// read snapshot when it first writes. In WAL mode that write fails at once,
// without the busy timeout: with SQLITE_BUSY while another connection holds
// the write lock, and with SQLITE_BUSY_SNAPSHOT once another connection has
// committed since the snapshot. Waiting inside the transaction cannot help;
// it can only be rolled back and run again from the start.
func IsWriteConflict(err error) bool {
	var sqErr *sqlite3.Error
	return errors.As(err, &sqErr) && sqErr.Code()&0xff == sqliteBusy
}

// BeginTransaction starts a new transaction.
func (s *SQLiteStorage) BeginTransaction(ctx context.Context) (context.Context, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ctx, err
	}
	state := &txState{tx: tx}
	return context.WithValue(ctx, txKey{}, state), nil
}

// beginImmediate begins a transaction that takes the write lock at BEGIN.
// The kernel's own transactions read before they write (a pending re-check
// before a wait registration, a candidate scan before a requeue), and a
// deferred SQLite transaction cannot upgrade to a write once another
// connection has committed since its read: WAL answers SQLITE_BUSY_SNAPSHOT
// at once, without the busy timeout. Holding the write lock from BEGIN also
// keeps a publish from committing between the re-check and the registration.
// Activity transactions (BeginTransaction) stay deferred: their bodies run
// user code of any length, and holding the write lock across it would stall
// every other writer. An activity that hits a snapshot conflict is run again
// in a new transaction instead (see IsWriteConflict).
func (s *SQLiteStorage) beginImmediate(ctx context.Context) (context.Context, error) {
	tx, err := s.immediate.BeginTx(ctx, nil)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, txKey{}, &txState{tx: tx}), nil
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
	framework := instance.Framework
	if framework == "" {
		framework = "go" // Default framework
	}

	// Ensure workflow definition exists (required by foreign key constraint)
	// Uses INSERT OR IGNORE for idempotency
	_, err := conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO workflow_definitions (workflow_name, source_hash, source_code)
		VALUES (?, ?, ?)
	`, instance.WorkflowName, instance.SourceHash, "")
	if err != nil {
		return fmt.Errorf("failed to ensure workflow definition: %w", err)
	}

	sandboxed, err := resolveSandboxed(ctx, conn, s.driver, instance)
	if err != nil {
		return err
	}
	instance.Sandboxed = sandboxed

	_, err = conn.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			instance_id, workflow_name, framework, status, input_data, source_hash, owner_service, sandboxed, started_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, instance.InstanceID, instance.WorkflowName, framework, instance.Status,
		string(instance.InputData), instance.SourceHash, instance.OwnerService,
		sandboxedInt(sandboxed),
		instance.StartedAt.UTC().Format("2006-01-02 15:04:05"),
		instance.UpdatedAt.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		var sqErr *sqlite3.Error
		// SQLITE_CONSTRAINT (19): the insert's only constraint is the instance
		// ID's uniqueness, so a violation means the instance exists.
		if errors.As(err, &sqErr) && sqErr.Code()&0xff == 19 {
			return fmt.Errorf("%w: %w", ErrInstanceAlreadyExists, err)
		}
	}
	return err
}

// GetInstance retrieves a workflow instance by ID.
func (s *SQLiteStorage) GetInstance(ctx context.Context, instanceID string) (*WorkflowInstance, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, framework, status, input_data, output_data,
			   current_activity_id, source_hash, owner_service, sandboxed,
			   locked_by, locked_at, lock_timeout_seconds, lock_expires_at,
			   started_at, updated_at
		FROM workflow_instances WHERE instance_id = ?
	`, instanceID)

	var inst WorkflowInstance
	var inputData, outputData, framework, activityID, sourceHash, ownerService sql.NullString
	var lockedBy sql.NullString
	var sandboxed int
	var lockedAt, lockExpiresAt sql.NullString // SQLite stores datetime as TEXT
	var startedAt, updatedAt sql.NullString    // SQLite stores datetime as TEXT
	var lockTimeout sql.NullInt64

	err := row.Scan(
		&inst.InstanceID, &inst.WorkflowName, &framework, &inst.Status,
		&inputData, &outputData, &activityID, &sourceHash, &ownerService, &sandboxed,
		&lockedBy, &lockedAt, &lockTimeout, &lockExpiresAt,
		&startedAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inst.Sandboxed = sandboxed != 0

	if inputData.Valid {
		inst.InputData = []byte(inputData.String)
	}
	if outputData.Valid {
		inst.OutputData = []byte(outputData.String)
	}
	if framework.Valid {
		inst.Framework = framework.String
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
	if lockedBy.Valid {
		inst.LockedBy = lockedBy.String
	}
	if lockedAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", lockedAt.String); err == nil {
			inst.LockedAt = &t
		}
	}
	if lockTimeout.Valid {
		timeout := int(lockTimeout.Int64)
		inst.LockTimeoutSec = &timeout
	}
	if lockExpiresAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", lockExpiresAt.String); err == nil {
			inst.LockExpiresAt = &t
		}
	}
	if startedAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", startedAt.String); err == nil {
			inst.StartedAt = t
		}
	}
	if updatedAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", updatedAt.String); err == nil {
			inst.UpdatedAt = t
		}
	}

	return &inst, nil
}

// UpdateInstanceStatus updates the status of a workflow instance.
// Note: errorMsg parameter is kept for interface compatibility but is ignored.
// Error messages are tracked via history events (WorkflowFailed).
func (s *SQLiteStorage) UpdateInstanceStatus(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, updated_at = datetime('now')
		WHERE instance_id = ?
	`, status, instanceID)
	return err
}

// lockHolder answers the worker that holds the instance's lock, or "" when it
// is unlocked or the instance does not exist.
func (s *SQLiteStorage) lockHolder(ctx context.Context, instanceID string) (string, error) {
	var lockedBy sql.NullString
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT locked_by FROM workflow_instances WHERE instance_id = ?
	`, instanceID).Scan(&lockedBy)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return lockedBy.String, nil
}

// lockLost checks, after a fenced write affected no row, whether the worker
// still holds the lock: it answers ErrLockNotHeld when the lock is gone, and
// nil when the lock is held (the write was a no-op for another reason).
func (s *SQLiteStorage) lockLost(ctx context.Context, instanceID, workerID string) error {
	holder, err := s.lockHolder(ctx, instanceID)
	if err != nil {
		return err
	}
	if holder == workerID {
		return nil
	}
	return ErrLockNotHeld
}

// UpdateInstanceStatusIfLocked updates the status only while workerID holds
// the instance's lock; it answers ErrLockNotHeld otherwise.
func (s *SQLiteStorage) UpdateInstanceStatusIfLocked(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
		AND (status = ? OR status NOT IN `+terminalStatusList+`)
	`, status, instanceID, workerID, status)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return s.statusWriteRefused(ctx, instanceID, workerID)
	}
	return nil
}

// statusWriteRefused explains a fenced status write that matched no row:
// ErrLockNotHeld when the caller lost the lock, ErrInstanceTerminated when
// the instance already terminated, nil when the write was a no-op.
func (s *SQLiteStorage) statusWriteRefused(ctx context.Context, instanceID, workerID string) error {
	var lockedBy sql.NullString
	var status WorkflowStatus
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT locked_by, status FROM workflow_instances WHERE instance_id = ?
	`, instanceID).Scan(&lockedBy, &status)
	if err == sql.ErrNoRows {
		return ErrLockNotHeld
	}
	if err != nil {
		return err
	}
	if lockedBy.String != workerID {
		return ErrLockNotHeld
	}
	if IsTerminalStatus(status) {
		return ErrInstanceTerminated
	}
	return nil
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

// UpdateInstanceOutputIfLocked updates the output only while workerID holds
// the instance's lock; it answers ErrLockNotHeld otherwise.
func (s *SQLiteStorage) UpdateInstanceOutputIfLocked(ctx context.Context, instanceID string, outputData []byte, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET output_data = ?, status = 'completed', updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
		AND (status = 'completed' OR status NOT IN `+terminalStatusList+`)
	`, string(outputData), instanceID, workerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return s.statusWriteRefused(ctx, instanceID, workerID)
	}
	return nil
}

func (s *SQLiteStorage) UpdateInstanceInputIfLocked(ctx context.Context, instanceID string, inputData []byte, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET input_data = ?, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
	`, string(inputData), instanceID, workerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return s.lockLost(ctx, instanceID, workerID)
	}
	return nil
}

// CancelInstance cancels a workflow instance.
// Returns ErrWorkflowNotCancellable if the workflow is already completed, cancelled, failed, or does not exist.
// Note: reason parameter is kept for interface compatibility but is ignored.
// Cancellation reasons are tracked via history events.
func (s *SQLiteStorage) CancelInstance(ctx context.Context, instanceID, reason string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'cancelled', updated_at = datetime('now')
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
	// A cancelled instance never waits again: drop its timers and channel
	// waits so nothing fires for it later.
	if _, err := conn.ExecContext(ctx, `DELETE FROM workflow_timer_subscriptions WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("failed to delete timer subscriptions: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions SET activity_id = NULL, timeout_at = NULL
		WHERE instance_id = ?
	`, instanceID); err != nil {
		return fmt.Errorf("failed to clear channel waits: %w", err)
	}
	return nil
}

// ListInstances lists workflow instances with cursor-based pagination and filters.
func (s *SQLiteStorage) ListInstances(ctx context.Context, opts ListInstancesOptions) (*PaginationResult, error) {
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
		query += " AND datetime(started_at) > datetime(?)"
		args = append(args, opts.StartedAfter.UTC().Format("2006-01-02 15:04:05"))
	}
	if opts.StartedBefore != nil {
		query += " AND datetime(started_at) < datetime(?)"
		args = append(args, opts.StartedBefore.UTC().Format("2006-01-02 15:04:05"))
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
				query += " AND (datetime(started_at) < datetime(?) OR (datetime(started_at) = datetime(?) AND instance_id < ?))"
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
		var startedAt, updatedAt sql.NullString
		if err := rows.Scan(&inst.InstanceID, &inst.WorkflowName, &inst.Status, &startedAt, &updatedAt); err != nil {
			return nil, err
		}
		if startedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", startedAt.String); err == nil {
				inst.StartedAt = t
			}
		}
		if updatedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", updatedAt.String); err == nil {
				inst.UpdatedAt = t
			}
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
func (s *SQLiteStorage) FindResumableWorkflows(ctx context.Context, workflowNames []string, limit int) ([]*ResumableWorkflow, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	query := `
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE status = ?
		AND (locked_by IS NULL OR locked_by = '')
		AND framework = 'go'
	`
	args := []any{StatusRunning}
	if len(workflowNames) > 0 {
		placeholders := make([]string, len(workflowNames))
		for i, name := range workflowNames {
			placeholders[i] = "?"
			args = append(args, name)
		}
		query += fmt.Sprintf(" AND workflow_name IN (%s)", strings.Join(placeholders, ","))
	}
	query += " ORDER BY updated_at ASC LIMIT ?"
	args = append(args, limit)

	rows, err := conn.QueryContext(ctx, query, args...)
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

// ClaimResumableWorkflows atomically selects claimable instances and installs
// the lease in one statement. On SQLite the UPDATE itself is atomic under the
// write lock, so no FOR UPDATE is needed; the IN-subquery's row set is
// evaluated as of the statement, and two concurrent claimants take disjoint
// sets because a row claimed by the first no longer matches.
func (s *SQLiteStorage) ClaimResumableWorkflows(ctx context.Context, workerID string, versions []WorkflowVersion, leaseSec, limit int) ([]*ResumableWorkflow, error) {
	if limit <= 0 {
		limit = 100
	}
	if len(versions) == 0 {
		return nil, nil
	}
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	nowStr := now.Format("2006-01-02 15:04:05")
	expiresStr := now.Add(time.Duration(leaseSec) * time.Second).Format("2006-01-02 15:04:05")

	filter, filterArgs := versionFilter(versions, questionMarks)
	args := []any{workerID, nowStr, expiresStr, nowStr}
	args = append(args, filterArgs...)
	args = append(args, limit)

	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET locked_by = ?, locked_at = ?, lock_expires_at = ?, updated_at = ?
		WHERE instance_id IN (
			SELECT instance_id FROM workflow_instances
			WHERE status = '%s'
			AND framework = 'go'
			AND %s
			AND (locked_by IS NULL OR locked_by = '' OR datetime(lock_expires_at) < datetime('now'))
			ORDER BY updated_at ASC
			LIMIT ?
		)
		RETURNING instance_id, workflow_name
	`, StatusRunning, filter), args...)
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
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET lock_expires_at = ?, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
	`, expiresStr, instanceID, workerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrLockNotHeld
	}
	return nil
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
		SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = datetime('now')
		WHERE locked_by IS NOT NULL AND datetime(lock_expires_at) < datetime('now') AND framework = 'go'
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

// AppendHistoryIfLocked appends the event only while workerID holds the
// instance's lock; it answers ErrLockNotHeld otherwise. A duplicate event is
// a no-op, as for AppendHistory.
func (s *SQLiteStorage) AppendHistoryIfLocked(ctx context.Context, event *HistoryEvent, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, event_data_binary, data_type)
		SELECT ?, ?, ?, ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM workflow_instances
			WHERE instance_id = ? AND locked_by = ?
		)
		ON CONFLICT (instance_id, activity_id) DO NOTHING
	`, event.InstanceID, event.ActivityID, event.EventType, string(event.EventData), event.EventDataBinary, event.DataType, event.InstanceID, workerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return s.lockLost(ctx, event.InstanceID, workerID)
	}
	return nil
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
		var createdAtStr string

		if err := rows.Scan(&h.ID, &h.InstanceID, &h.ActivityID, &h.EventType, &eventData, &eventDataBinary, &h.DataType, &createdAtStr); err != nil {
			return nil, false, err
		}
		h.CreatedAt, _ = parseSQLiteTime(createdAtStr)
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
		INSERT INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, activity_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (instance_id, timer_id) DO NOTHING
	`, sub.InstanceID, sub.TimerID, expiresStr, sub.ActivityID)
	return err
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
func (s *SQLiteStorage) FindExpiredTimers(ctx context.Context, limit int) ([]*TimerSubscription, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT t.id, t.instance_id, t.timer_id, t.expires_at, t.activity_id, t.created_at
		FROM workflow_timer_subscriptions t
		JOIN workflow_instances w ON t.instance_id = w.instance_id
		WHERE datetime(t.expires_at) <= datetime('now') AND w.framework = 'go'
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
		var expiresAt, createdAt sql.NullString // SQLite stores datetime as TEXT
		var activityID sql.NullString
		if err := rows.Scan(&t.ID, &t.InstanceID, &t.TimerID, &expiresAt, &activityID, &createdAt); err != nil {
			return nil, err
		}
		if activityID.Valid {
			t.ActivityID = activityID.String
		}
		if expiresAt.Valid {
			if parsed, err := time.Parse("2006-01-02 15:04:05", expiresAt.String); err == nil {
				t.ExpiresAt = parsed
			}
		}
		if createdAt.Valid {
			if parsed, err := time.Parse("2006-01-02 15:04:05", createdAt.String); err == nil {
				t.CreatedAt = parsed
			}
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
		INSERT INTO outbox_events (event_id, event_type, event_source, event_data, data_type, content_type, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, event.EventID, event.EventType, event.EventSource, string(event.EventData), event.DataType, event.ContentType, event.Status)
	return err
}

// GetPendingOutboxEvents retrieves pending outbox events.
func (s *SQLiteStorage) GetPendingOutboxEvents(ctx context.Context, limit int) ([]*OutboxEvent, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT event_id, event_type, event_source, event_data, data_type, content_type, status, retry_count, created_at
		FROM outbox_events
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanOutboxEvents(rows)
}

// ClaimOutboxEvents atomically moves a batch of events to 'processing' and
// returns them. claimed_at is stamped with the claim time so a claim left
// behind by a crashed relayer becomes claimable again past staleBefore.
// SQLite serializes writers, so the single UPDATE is atomic without
// FOR UPDATE; concurrent claimants take disjoint sets.
func (s *SQLiteStorage) ClaimOutboxEvents(ctx context.Context, staleBefore time.Time, limit int) ([]*OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")
	staleStr := staleBefore.UTC().Format("2006-01-02 15:04:05")

	rows, err := conn.QueryContext(ctx, `
		UPDATE outbox_events
		SET status = 'processing', claimed_at = ?
		WHERE event_id IN (
			SELECT event_id FROM outbox_events
			WHERE status = 'pending'
			OR (status = 'processing' AND datetime(COALESCE(claimed_at, published_at)) < datetime(?))
			ORDER BY created_at ASC
			LIMIT ?
		)
		RETURNING event_id, event_type, event_source, event_data, data_type, content_type, status, retry_count, created_at
	`, nowStr, staleStr, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanOutboxEvents(rows)
}

// ReleaseOutboxEvent returns a claimed event to 'pending'.
func (s *SQLiteStorage) ReleaseOutboxEvent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'pending', claimed_at = NULL
		WHERE event_id = ? AND status = 'processing'
	`, eventID)
	return err
}

func scanOutboxEvents(rows *sql.Rows) ([]*OutboxEvent, error) {
	var events []*OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		var eventData sql.NullString
		var createdAtStr string
		if err := rows.Scan(&e.EventID, &e.EventType, &e.EventSource, &eventData, &e.DataType, &e.ContentType, &e.Status, &e.RetryCount, &createdAtStr); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = parseSQLiteTime(createdAtStr)
		if eventData.Valid {
			e.EventData = []byte(eventData.String)
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// MarkOutboxEventSent marks an outbox event as published.
func (s *SQLiteStorage) MarkOutboxEventSent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'published', published_at = datetime('now'), claimed_at = NULL
		WHERE event_id = ?
	`, eventID)
	return err
}

// MarkOutboxEventFailed marks an outbox event as failed.
func (s *SQLiteStorage) MarkOutboxEventFailed(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'failed', claimed_at = NULL
		WHERE event_id = ?
	`, eventID)
	return err
}

// IncrementOutboxAttempts increments the attempt count for an event.
func (s *SQLiteStorage) IncrementOutboxAttempts(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET retry_count = retry_count + 1
		WHERE event_id = ?
	`, eventID)
	return err
}

// CleanupOldOutboxEvents removes old published events.
func (s *SQLiteStorage) CleanupOldOutboxEvents(ctx context.Context, olderThan time.Duration) error {
	conn := s.getConn(ctx)
	threshold := time.Now().UTC().Add(-olderThan).Format("2006-01-02 15:04:05")
	_, err := conn.ExecContext(ctx, `
		DELETE FROM outbox_events
		WHERE status = 'published' AND datetime(created_at) < datetime(?)
	`, threshold)
	return err
}

// --- Compensation Manager ---

// AddCompensation adds a compensation entry.
// Order is determined by created_at DESC (LIFO).
// Uses millisecond-precision timestamp for reliable ordering.
func (s *SQLiteStorage) AddCompensation(ctx context.Context, entry *CompensationEntry) error {
	conn := s.getConn(ctx)
	// Use Go's time.Now() with millisecond precision for reliable LIFO ordering
	// SQLite's datetime('now') only has second precision
	createdAt := time.Now().Format("2006-01-02 15:04:05.000")
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_compensations (instance_id, activity_id, activity_name, args, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, entry.InstanceID, entry.ActivityID, entry.ActivityName, string(entry.Args), createdAt)
	return err
}

// ClearCompensations removes every compensation registered for the instance.
func (s *SQLiteStorage) ClearCompensations(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `DELETE FROM workflow_compensations WHERE instance_id = ?`, instanceID)
	return err
}

// GetCompensations retrieves compensations for a workflow in LIFO order (by created_at DESC).
// Uses millisecond-precision timestamps set by AddCompensation for reliable ordering.
// Status tracking is done via history events (CompensationExecuted, CompensationFailed).
func (s *SQLiteStorage) GetCompensations(ctx context.Context, instanceID string) ([]*CompensationEntry, error) {
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
		var createdAtStr string
		if err := rows.Scan(&c.ID, &c.InstanceID, &c.ActivityID, &c.ActivityName, &args, &createdAtStr); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = parseSQLiteTime(createdAtStr)
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
// For direct messages, use dynamic channel names (e.g., "channel:instance_id").
func (s *SQLiteStorage) PublishToChannel(ctx context.Context, channelName string, dataJSON, metadata []byte) (int64, error) {
	conn := s.getConn(ctx)
	var dataStr, metadataStr sql.NullString
	if dataJSON != nil {
		dataStr = sql.NullString{String: string(dataJSON), Valid: true}
	}
	if metadata != nil {
		metadataStr = sql.NullString{String: string(metadata), Valid: true}
	}

	// Generate UUID for message_id (UNIQUE; a clock-derived id can collide)
	msgID := uuid.New().String()

	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_messages (channel, data, metadata, message_id, data_type)
		VALUES (?, ?, ?, ?, 'json')
	`, channelName, dataStr, metadataStr, msgID)
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
		INSERT INTO channel_subscriptions (instance_id, channel, mode)
		VALUES (?, ?, ?)
		ON CONFLICT (instance_id, channel) DO UPDATE SET mode = excluded.mode
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
			ON CONFLICT (instance_id, channel) DO NOTHING
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
		DELETE FROM channel_subscriptions WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName)
	return err
}

// GetChannelSubscription retrieves a subscription for an instance and channel.
func (s *SQLiteStorage) GetChannelSubscription(ctx context.Context, instanceID, channelName string) (*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT id, instance_id, channel, mode, timeout_at, COALESCE(activity_id, ''), subscribed_at
		FROM channel_subscriptions
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName)

	var sub ChannelSubscription
	var modeStr string
	var timeoutAt, subscribedAt sql.NullString // SQLite stores datetime as TEXT
	err := row.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &subscribedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sub.Mode = ChannelMode(modeStr)
	if timeoutAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", timeoutAt.String); err == nil {
			sub.TimeoutAt = &t
		}
	}
	if subscribedAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", subscribedAt.String); err == nil {
			sub.SubscribedAt = t
		}
	}
	return &sub, nil
}

// GetChannelMode retrieves the mode for a channel (from any existing subscription).
// Returns empty string if no subscriptions exist.
func (s *SQLiteStorage) GetChannelMode(ctx context.Context, channelName string) (ChannelMode, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT mode FROM channel_subscriptions WHERE channel = ? LIMIT 1
	`, channelName)

	var modeStr string
	if err := row.Scan(&modeStr); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return ChannelMode(modeStr), nil
}

// RegisterChannelReceiveAndReleaseLock atomically registers a channel receive wait and releases the lock.
// Waiting state is indicated by activity_id being non-null.
func (s *SQLiteStorage) RegisterChannelReceiveAndReleaseLock(ctx context.Context, instanceID, channelName, workerID, activityID string, timeoutAt *time.Time) (bool, error) {
	var needTx bool
	if !s.InTransaction(ctx) {
		var err error
		ctx, err = s.beginImmediate(ctx)
		if err != nil {
			return false, err
		}
		needTx = true
		defer func() {
			if needTx {
				_ = s.RollbackTransaction(ctx)
			}
		}()
	}

	conn := s.getConn(ctx)

	// A publish committed between the caller's pending check and this
	// registration would never be delivered to a freshly registered wait;
	// detect it here and keep the instance running instead of suspending.
	pending, err := s.hasPendingChannelMessage(ctx, instanceID, channelName)
	if err != nil {
		return false, err
	}
	if pending {
		res, err := conn.ExecContext(ctx, `
			UPDATE workflow_instances
			SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = datetime('now')
			WHERE instance_id = ? AND locked_by = ?
		`, instanceID, workerID)
		if err != nil {
			return false, err
		}
		if err := heldByCaller(res); err != nil {
			return false, err
		}
		if needTx {
			if err := s.CommitTransaction(ctx); err != nil {
				return false, err
			}
			needTx = false
		}
		return false, nil
	}

	// Suspend and release first, fenced on the caller's lock: a run that lost
	// its lease must not overwrite the live run's wait registration.
	res, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = datetime('now')
		WHERE instance_id = ? AND locked_by = ?
		AND status NOT IN `+terminalStatusList+`
	`, StatusWaitingForMessage, instanceID, workerID)
	if err != nil {
		return false, err
	}
	if err := heldByCaller(res); err != nil {
		if refused := s.statusWriteRefused(ctx, instanceID, workerID); refused != nil {
			return false, refused
		}
		return false, err
	}

	// Update subscription to set activity_id (non-null = waiting state)
	var timeoutStr sql.NullString
	if timeoutAt != nil {
		timeoutStr = sql.NullString{String: timeoutAt.UTC().Format("2006-01-02 15:04:05"), Valid: true}
	}
	_, err = conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET timeout_at = ?, activity_id = ?
		WHERE instance_id = ? AND channel = ?
	`, timeoutStr, activityID, instanceID, channelName)
	if err != nil {
		return false, err
	}

	if needTx {
		if err := s.CommitTransaction(ctx); err != nil {
			return false, err
		}
		needTx = false
	}
	return true, nil
}

// hasPendingChannelMessage reports whether the instance has an undelivered
// message on the channel, using the same semantics as
// GetPendingChannelMessagesForInstance. It must be called on a connection or
// transaction that sees a consistent snapshot (inside the registration tx).
func (s *SQLiteStorage) hasPendingChannelMessage(ctx context.Context, instanceID, channelName string) (bool, error) {
	conn := s.getConn(ctx)

	var mode string
	err := conn.QueryRowContext(ctx, `
		SELECT mode FROM channel_subscriptions
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName).Scan(&mode)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}

	var exists int
	if mode == string(ChannelModeCompeting) {
		staleBefore := time.Now().UTC().Add(-ChannelClaimStaleSeconds * time.Second).Format("2006-01-02 15:04:05")
		err = conn.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM channel_messages m
				LEFT JOIN channel_message_claims c ON m.message_id = c.message_id
				LEFT JOIN workflow_instances ci ON c.instance_id = ci.instance_id
				WHERE m.channel = ? AND (
					c.message_id IS NULL
					OR c.instance_id = ?
					OR c.claimed_at < ?
					OR ci.status IN ('completed', 'failed', 'cancelled')
				)
			)
		`, channelName, instanceID, staleBefore).Scan(&exists)
	} else {
		err = conn.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM channel_messages m
				LEFT JOIN channel_delivery_cursors dc
					ON dc.instance_id = ? AND dc.channel = ?
				WHERE m.channel = ? AND m.id > COALESCE(dc.last_delivered_id, 0)
			)
		`, instanceID, channelName, channelName).Scan(&exists)
	}
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

func (s *SQLiteStorage) RequeueChannelWaitsWithPending(ctx context.Context) (int, error) {
	var needTx bool
	if !s.InTransaction(ctx) {
		var err error
		ctx, err = s.beginImmediate(ctx)
		if err != nil {
			return 0, err
		}
		needTx = true
		defer func() {
			if needTx {
				_ = s.RollbackTransaction(ctx)
			}
		}()
	}
	conn := s.getConn(ctx)

	// Candidates first, then a conditional flip per instance: an UPDATE of
	// workflow_instances cannot read workflow_instances in its own subquery
	// on MySQL (error 1093), and the flipped instance's wait is cleared with
	// it so its stale activity id and deadline cannot fire later.
	stale := time.Now().UTC().Add(-ChannelClaimStaleSeconds * time.Second).Format("2006-01-02 15:04:05")
	rows, err := conn.QueryContext(ctx, `
		SELECT DISTINCT w.instance_id
		FROM workflow_instances w
		JOIN channel_subscriptions s ON s.instance_id = w.instance_id
		WHERE w.status = ? AND w.locked_by IS NULL
		AND s.activity_id IS NOT NULL AND s.activity_id != ''
		AND (
			(s.mode = ? AND EXISTS (
				SELECT 1 FROM channel_messages m
				LEFT JOIN channel_message_claims c ON m.message_id = c.message_id
				LEFT JOIN workflow_instances ci ON c.instance_id = ci.instance_id
				WHERE m.channel = s.channel AND (
					c.message_id IS NULL
					OR c.instance_id = s.instance_id
					OR c.claimed_at < ?
					OR ci.status IN `+terminalStatusList+`
				)
			))
			OR
			(s.mode != ? AND EXISTS (
				SELECT 1 FROM channel_messages m
				LEFT JOIN channel_delivery_cursors dc
					ON dc.instance_id = s.instance_id AND dc.channel = s.channel
				WHERE m.channel = s.channel AND m.id > COALESCE(dc.last_delivered_id, 0)
			))
		)
	`, StatusWaitingForMessage, string(ChannelModeCompeting), stale, string(ChannelModeCompeting))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	requeued := 0
	for _, id := range ids {
		res, err := conn.ExecContext(ctx, `
			UPDATE workflow_instances SET status = ?, updated_at = datetime('now')
			WHERE instance_id = ? AND status = ? AND locked_by IS NULL
		`, StatusRunning, id, StatusWaitingForMessage)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if n == 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE channel_subscriptions SET activity_id = NULL, timeout_at = NULL
			WHERE instance_id = ?
		`, id); err != nil {
			return 0, err
		}
		requeued++
	}

	if needTx {
		if err := s.CommitTransaction(ctx); err != nil {
			return 0, err
		}
		needTx = false
	}
	return requeued, nil
}

// GetPendingChannelMessages retrieves pending messages for a channel after a given ID.
func (s *SQLiteStorage) GetPendingChannelMessages(ctx context.Context, channelName string, afterID int64, limit int) ([]*ChannelMessage, error) {
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
		var data, metadata, publishedAt sql.NullString
		var dataBinary []byte
		err := rows.Scan(&msg.ID, &msg.Channel, &data, &dataBinary, &metadata, &publishedAt)
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
		if publishedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", publishedAt.String); err == nil {
				msg.PublishedAt = t
			}
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
		// Competing mode: messages no live other instance has claimed. Claims by
		// this instance stay visible so a replay can drain them after a crash;
		// stale claims and claims by terminal owners are void.
		staleBefore := time.Now().UTC().Add(-ChannelClaimStaleSeconds * time.Second).Format("2006-01-02 15:04:05")
		query = `
			SELECT m.id, m.channel, m.data, m.data_binary, m.metadata, m.published_at
			FROM channel_messages m
			LEFT JOIN channel_message_claims c ON m.message_id = c.message_id
			LEFT JOIN workflow_instances ci ON c.instance_id = ci.instance_id
			WHERE m.channel = ? AND (
				c.message_id IS NULL
				OR c.instance_id = ?
				OR c.claimed_at < ?
				OR ci.status IN ('completed', 'failed', 'cancelled')
			)
			ORDER BY m.id ASC
			LIMIT 10
		`
		args = []any{channelName, instanceID, staleBefore}
	}

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []*ChannelMessage
	for rows.Next() {
		var msg ChannelMessage
		var data, metadata, publishedAt sql.NullString
		var dataBinary []byte
		err := rows.Scan(&msg.ID, &msg.Channel, &data, &dataBinary, &metadata, &publishedAt)
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
		if publishedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", publishedAt.String); err == nil {
				msg.PublishedAt = t
			}
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

// ClaimChannelMessage claims a message for competing mode.
func (s *SQLiteStorage) ClaimChannelMessage(ctx context.Context, messageID int64, instanceID string) (bool, error) {
	conn := s.getConn(ctx)

	// Insert resolves the numeric row id to the message_id UUID the claims
	// table references; a missing message simply inserts nothing.
	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_message_claims (message_id, instance_id)
		SELECT m.message_id, ? FROM channel_messages m WHERE m.id = ?
		ON CONFLICT (message_id) DO NOTHING
	`, instanceID, messageID)
	if err != nil {
		return false, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return false, err
	} else if affected > 0 {
		return true, nil
	}

	// Take over our own claim (idempotent), a stale claim, or one whose owner
	// reached a terminal status without consuming the message.
	staleBefore := time.Now().UTC().Add(-ChannelClaimStaleSeconds * time.Second).Format("2006-01-02 15:04:05")
	result, err = conn.ExecContext(ctx, `
		UPDATE channel_message_claims
		SET instance_id = ?, claimed_at = datetime('now')
		WHERE message_id = (SELECT message_id FROM channel_messages WHERE id = ?)
			AND (instance_id = ?
				OR claimed_at < ?
				OR instance_id IN (
					SELECT instance_id FROM workflow_instances
					WHERE status IN ('completed', 'failed', 'cancelled')
				))
	`, instanceID, messageID, instanceID, staleBefore)
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
	_, err := conn.ExecContext(ctx, `
		DELETE FROM channel_message_claims
		WHERE message_id = (SELECT message_id FROM channel_messages WHERE id = ?)
	`, messageID)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `DELETE FROM channel_messages WHERE id = ?`, messageID)
	return err
}

// UpdateDeliveryCursor updates the delivery cursor for broadcast mode.
func (s *SQLiteStorage) UpdateDeliveryCursor(ctx context.Context, instanceID, channelName string, lastMessageID int64) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
		VALUES (?, ?, ?)
		ON CONFLICT (instance_id, channel) DO UPDATE SET last_delivered_id = excluded.last_delivered_id, updated_at = datetime('now')
	`, instanceID, channelName, lastMessageID)
	return err
}

// GetDeliveryCursor gets the current delivery cursor for an instance and channel.
func (s *SQLiteStorage) GetDeliveryCursor(ctx context.Context, instanceID, channelName string) (int64, error) {
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
// Waiting state is determined by activity_id IS NOT NULL.
// Returns all waiting subscribers regardless of framework - delivery is handled by Lock-First pattern.
func (s *SQLiteStorage) GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*ChannelSubscription, error) {
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT id, instance_id, channel, mode, timeout_at, activity_id, subscribed_at
		FROM channel_subscriptions
		WHERE channel = ? AND activity_id IS NOT NULL AND activity_id != ''
	`, channelName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []*ChannelSubscription
	for rows.Next() {
		var sub ChannelSubscription
		var modeStr string
		var timeoutAt, subscribedAt sql.NullString // SQLite stores datetime as TEXT
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &subscribedAt)
		if err != nil {
			return nil, err
		}
		sub.Mode = ChannelMode(modeStr)
		if timeoutAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", timeoutAt.String); err == nil {
				sub.TimeoutAt = &t
			}
		}
		if subscribedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", subscribedAt.String); err == nil {
				sub.SubscribedAt = t
			}
		}
		subs = append(subs, &sub)
	}
	return subs, rows.Err()
}

// ClearChannelWaitingState clears the waiting state for an instance's channel subscription.
// Waiting state is cleared by setting activity_id to NULL.
func (s *SQLiteStorage) ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET activity_id = NULL, timeout_at = NULL
		WHERE instance_id = ? AND channel = ?
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
	`, instanceID, fmt.Sprintf("channel:%s:%d", message.Channel, message.ID), string(message.Data))
	if err != nil {
		return err
	}

	// Clear waiting state
	return s.ClearChannelWaitingState(ctx, instanceID, message.Channel)
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
	// One transaction: the claim, the receive record, the consume-delete, the
	// cursor and the status flip land together or not at all. A crash in
	// between must not leave a consumed message without its receive, or a
	// recorded receive whose wait is still open to a second message.
	if !s.InTransaction(ctx) {
		txCtx, err := s.beginImmediate(ctx)
		if err != nil {
			return nil, err
		}
		res, err := s.DeliverChannelMessageWithLock(txCtx, instanceID, channelName, message, workerID, lockTimeoutSec)
		if err != nil || res == nil {
			_ = s.RollbackTransaction(txCtx)
			return res, err
		}
		if err := s.CommitTransaction(txCtx); err != nil {
			return nil, err
		}
		return res, nil
	}
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
	var status WorkflowStatus
	err = conn.QueryRowContext(ctx, `
		SELECT workflow_name, status FROM workflow_instances WHERE instance_id = ?
	`, instanceID).Scan(&workflowName, &status)
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get workflow info: %w", err)
	}

	// Get the activity ID and mode from the subscription (stored when Receive was called)
	var historyActivityID, subscriptionMode string
	err = conn.QueryRowContext(ctx, `
		SELECT COALESCE(activity_id, ''), mode FROM channel_subscriptions
		WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName).Scan(&historyActivityID, &subscriptionMode)
	if err != nil && err != sql.ErrNoRows {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to get subscription info: %w", err)
	}
	// Only a subscriber that is waiting on this channel takes the message:
	// the history slot it fills is that wait's activity id. Anyone else —
	// resumed in the meantime, terminated, never waiting — leaves the message
	// pending for its next Receive instead of having it recorded where no
	// Receive will ever read it (and, in competing mode, deleted).
	if status != StatusWaitingForMessage || historyActivityID == "" {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, nil
	}

	// For competing mode, claim the message for this instance first so other
	// competing subscribers cannot also consume it (exactly-once).
	if subscriptionMode == string(ChannelModeCompeting) {
		claimed, err := s.ClaimChannelMessage(ctx, message.ID, instanceID)
		if err != nil {
			_ = s.ReleaseLock(ctx, instanceID, workerID)
			return nil, fmt.Errorf("failed to claim message: %w", err)
		}
		if !claimed {
			_ = s.ReleaseLock(ctx, instanceID, workerID)
			return nil, nil
		}
	}

	// Step 3: Record message in history using the correct activity ID
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
	`, instanceID, historyActivityID, string(wrappedJSON))
	if err != nil {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, fmt.Errorf("failed to record history: %w", err)
	}

	// Competing mode consumes the message: delete it (cascading the claim)
	// once history is durable so it cannot be delivered again.
	if subscriptionMode == string(ChannelModeCompeting) {
		if _, err := conn.ExecContext(ctx, `DELETE FROM channel_message_claims WHERE message_id = (SELECT message_id FROM channel_messages WHERE id = ?)`, message.ID); err != nil {
			_ = s.ReleaseLock(ctx, instanceID, workerID)
			return nil, fmt.Errorf("failed to delete consumed claim: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM channel_messages WHERE id = ?`, message.ID); err != nil {
			_ = s.ReleaseLock(ctx, instanceID, workerID)
			return nil, fmt.Errorf("failed to delete consumed message: %w", err)
		}
	}

	// Step 3.5: Update delivery cursor for broadcast mode
	// This prevents duplicate message delivery when workflow resumes
	if subscriptionMode == string(ChannelModeBroadcast) {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
			VALUES (?, ?, ?)
			ON CONFLICT (instance_id, channel)
			DO UPDATE SET last_delivered_id = excluded.last_delivered_id, updated_at = datetime('now')
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
		WHERE instance_id = ? AND status = ?
	`, StatusRunning, instanceID, StatusWaitingForMessage)
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
		DELETE FROM channel_messages WHERE datetime(published_at) < datetime(?)
	`, threshold)
	return err
}

// FindExpiredChannelSubscriptions finds channel subscriptions that have timed out.
// Waiting state is determined by activity_id IS NOT NULL.
func (s *SQLiteStorage) FindExpiredChannelSubscriptions(ctx context.Context, limit int) ([]*ChannelSubscription, error) {
	if limit <= 0 {
		limit = 100
	}
	conn := s.getConn(ctx)
	rows, err := conn.QueryContext(ctx, `
		SELECT cs.id, cs.instance_id, cs.channel, cs.mode, cs.timeout_at, cs.activity_id, COALESCE(cs.cursor_message_id, 0), cs.subscribed_at
		FROM channel_subscriptions cs
		JOIN workflow_instances w ON cs.instance_id = w.instance_id
		WHERE cs.activity_id IS NOT NULL AND cs.activity_id != '' AND cs.timeout_at IS NOT NULL AND datetime(cs.timeout_at) < datetime('now')
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
		var timeoutAt, subscribedAt sql.NullString // SQLite stores datetime as TEXT
		err := rows.Scan(&sub.ID, &sub.InstanceID, &sub.Channel, &modeStr, &timeoutAt, &sub.ActivityID, &sub.CursorMessageID, &subscribedAt)
		if err != nil {
			return nil, err
		}
		sub.Mode = ChannelMode(modeStr)
		if timeoutAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", timeoutAt.String); err == nil {
				sub.TimeoutAt = &t
			}
		}
		if subscribedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", subscribedAt.String); err == nil {
				sub.SubscribedAt = t
			}
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
		INSERT INTO system_locks (lock_name, locked_by, locked_at, lock_expires_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (lock_name) DO UPDATE SET
			locked_by = excluded.locked_by,
			locked_at = excluded.locked_at,
			lock_expires_at = excluded.lock_expires_at
		WHERE datetime(system_locks.lock_expires_at) < datetime('now') OR system_locks.locked_by = excluded.locked_by
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
		DELETE FROM system_locks WHERE datetime(lock_expires_at) < datetime('now')
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
		INSERT INTO workflow_history_archive (instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at)
		SELECT instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at
		FROM workflow_history
		WHERE instance_id = ?
		ORDER BY id
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
		SELECT id, instance_id, activity_id, event_type, event_data, event_data_binary, data_type, created_at, archived_at
		FROM workflow_history_archive
		WHERE instance_id = ?
		ORDER BY id ASC
	`, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []*ArchivedHistoryEvent
	for rows.Next() {
		var e ArchivedHistoryEvent
		var eventData sql.NullString
		var originalCreatedAtStr, archivedAtStr sql.NullString
		err := rows.Scan(&e.ID, &e.InstanceID, &e.ActivityID, &e.EventType, &eventData, &e.EventDataBinary, &e.DataType, &originalCreatedAtStr, &archivedAtStr)
		if err != nil {
			return nil, err
		}
		if eventData.Valid {
			e.EventData = []byte(eventData.String)
		}
		if originalCreatedAtStr.Valid {
			e.OriginalCreatedAt, _ = parseSQLiteTime(originalCreatedAtStr.String)
		}
		if archivedAtStr.Valid {
			e.ArchivedAt, _ = parseSQLiteTime(archivedAtStr.String)
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// CleanupInstanceSubscriptions removes all subscriptions for an instance.
// Note: workflow_event_subscriptions was removed in schema unification;
// events now use channel subscriptions.
func (s *SQLiteStorage) CleanupInstanceSubscriptions(ctx context.Context, instanceID string) error {
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

// ========================================
// Actor Registry
// ========================================

// RegisterActorName binds name to instanceID. Registering a name the
// instance already holds succeeds (SEMANTICS §6: registration is
// idempotent); a name bound to another instance answers ErrActorNameTaken.
func (s *SQLiteStorage) RegisterActorName(ctx context.Context, name, instanceID, framework string) error {
	conn := s.getConn(ctx)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO actor_names (name, instance_id, framework) VALUES (?, ?, ?)
		ON CONFLICT (name) DO NOTHING
	`, name, instanceID, framework); err != nil {
		return err
	}
	var holder string
	err := conn.QueryRowContext(ctx, `SELECT instance_id FROM actor_names WHERE name = ?`, name).Scan(&holder)
	if err != nil {
		return err
	}
	if holder != instanceID {
		return fmt.Errorf("%w: %q is bound to %s", ErrActorNameTaken, name, holder)
	}
	return nil
}

func (s *SQLiteStorage) UnregisterActorName(ctx context.Context, name string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `DELETE FROM actor_names WHERE name = ?`, name)
	return err
}

func (s *SQLiteStorage) UnregisterOwnActorName(ctx context.Context, name, owner string) error {
	return deleteOwned(ctx, s.getConn(ctx), s.driver.Placeholder, "actor_names", "name", "instance_id", name, owner)
}

func (s *SQLiteStorage) RevokeOwnCapability(ctx context.Context, capabilityID, owner string) error {
	return deleteOwned(ctx, s.getConn(ctx), s.driver.Placeholder, "actor_capabilities", "id", "granted_by", capabilityID, owner)
}

func (s *SQLiteStorage) WhereisActor(ctx context.Context, name string) (string, error) {
	var id string
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT instance_id FROM actor_names WHERE name = ?
	`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *SQLiteStorage) MonitorActor(ctx context.Context, watcherInstanceID, targetInstanceID, channel string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT OR IGNORE INTO actor_monitors (watcher_instance_id, target_instance_id, channel)
		VALUES (?, ?, ?)
	`, watcherInstanceID, targetInstanceID, channel)
	return err
}

func (s *SQLiteStorage) UnmonitorActor(ctx context.Context, watcherInstanceID, targetInstanceID string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		DELETE FROM actor_monitors WHERE watcher_instance_id = ? AND target_instance_id = ?
	`, watcherInstanceID, targetInstanceID)
	return err
}

func (s *SQLiteStorage) EmitActorDownForTerminated(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.target_instance_id, m.channel, w.status
		FROM actor_monitors m
		JOIN workflow_instances w ON w.instance_id = m.target_instance_id
		WHERE w.status IN `+terminalStatusList)
	if err != nil {
		return 0, err
	}
	type monitorRow struct {
		id       int64
		targetID string
		channel  string
		status   string
	}
	var monitors []monitorRow
	for rows.Next() {
		var m monitorRow
		if err := rows.Scan(&m.id, &m.targetID, &m.channel, &m.status); err != nil {
			_ = rows.Close()
			return 0, err
		}
		monitors = append(monitors, m)
	}
	_ = rows.Close()

	emitted := 0
	for _, m := range monitors {
		// Consume the monitor first: of two overlapping sweeps only the one
		// whose delete takes the row emits the DOWN.
		res, err := tx.ExecContext(ctx, `DELETE FROM actor_monitors WHERE id = ?`, m.id)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_messages (channel, data, message_id, data_type)
			VALUES (?, ?, ?, 'json')
		`, m.channel, downPayload(m.targetID, m.status), "down-"+uuid.New().String()); err != nil {
			return 0, err
		}
		emitted++
	}
	// A terminated actor's names are freed whether or not anyone monitored it.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM actor_names WHERE instance_id IN (
			SELECT instance_id FROM workflow_instances WHERE status IN `+terminalStatusList+`)`); err != nil {
		return 0, err
	}
	return emitted, tx.Commit()
}

// ========================================
// Capability Manager
// ========================================

func (s *SQLiteStorage) GrantCapability(ctx context.Context, capability *ActorCapability) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO actor_capabilities (id, kind, target, secret_hash, granted_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, capability.ID, capability.Kind, capability.Target, capability.SecretHash, capability.GrantedBy,
		expiryParam(capability.ExpiresAt))
	return err
}

func (s *SQLiteStorage) RevokeCapability(ctx context.Context, capabilityID string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		DELETE FROM actor_capabilities WHERE id = ?
	`, capabilityID)
	return err
}

func (s *SQLiteStorage) HasCapabilityGrants(ctx context.Context, kind, target string) (bool, error) {
	conn := s.getConn(ctx)
	var count int
	err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM actor_capabilities WHERE kind = ? AND target = ?", kind, target).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *SQLiteStorage) CheckCapability(ctx context.Context, kind, target, secretHash string) (bool, error) {
	var expiresAt any
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT expires_at FROM actor_capabilities
		WHERE kind = ?1 AND target = ?2 AND secret_hash = ?3
	`, kind, target, secretHash).Scan(&expiresAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if expiresAt == nil {
		return true, nil
	}
	return canonicalTSFrom(expiresAt) > canonicalTS(time.Now()), nil
}

func (s *SQLiteStorage) PutBlob(ctx context.Context, hash string, data []byte, createdBy string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT OR IGNORE INTO payload_blobs (hash, data, size_bytes, created_by)
		VALUES (?, ?, ?, ?)`, hash, data, len(data), createdBy)
	return err
}

func (s *SQLiteStorage) GetBlob(ctx context.Context, hash string) ([]byte, error) {
	var data []byte
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT data FROM payload_blobs WHERE hash = ?1`, hash).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *SQLiteStorage) FSGet(ctx context.Context, path string) (*FSObject, error) {
	var obj FSObject
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT path, COALESCE(owner, ''), data, version FROM actor_fs WHERE path = ?1`,
		path).Scan(&obj.Path, &obj.Owner, &obj.Data, &obj.Version)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &obj, nil
}

func (s *SQLiteStorage) FSPut(ctx context.Context, path string, data []byte, owner string, expectedVersion int64) error {
	var res sql.Result
	var err error
	now := time.Now().UTC()
	if expectedVersion == 0 {
		res, err = s.getConn(ctx).ExecContext(ctx, `
			INSERT INTO actor_fs (path, owner, data, version, created_at, updated_at)
			SELECT ?1, ?2, ?3, 1, ?4, ?4
			WHERE NOT EXISTS (SELECT 1 FROM actor_fs WHERE path = ?5)`,
			path, owner, data, now, path)
	} else {
		res, err = s.getConn(ctx).ExecContext(ctx, `
			UPDATE actor_fs SET data = ?1, owner = ?2, version = version + 1, updated_at = ?3
			WHERE path = ?4 AND version = ?5`,
			data, owner, now, path, expectedVersion)
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrFSConflict
	}
	return nil
}

func (s *SQLiteStorage) FSDelete(ctx context.Context, path string, expectedVersion int64) error {
	var res sql.Result
	var err error
	if expectedVersion == 0 {
		res, err = s.getConn(ctx).ExecContext(ctx,
			`DELETE FROM actor_fs WHERE path = ?1`, path)
	} else {
		res, err = s.getConn(ctx).ExecContext(ctx,
			`DELETE FROM actor_fs WHERE path = ?1 AND version = ?2`, path, expectedVersion)
	}
	if err != nil {
		return err
	}
	if expectedVersion == 0 {
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrFSConflict
	}
	return nil
}

func (s *SQLiteStorage) FSList(ctx context.Context, prefix string) ([]*FSObject, error) {
	rows, err := s.getConn(ctx).QueryContext(ctx,
		`SELECT path, COALESCE(owner, ''), version FROM actor_fs
			WHERE path LIKE ?1 ESCAPE '\' ORDER BY path`,
		likeEscape(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var objs []*FSObject
	for rows.Next() {
		var obj FSObject
		if err := rows.Scan(&obj.Path, &obj.Owner, &obj.Version); err != nil {
			return nil, err
		}
		objs = append(objs, &obj)
	}
	return objs, rows.Err()
}

func (s *SQLiteStorage) GetBlobInfo(ctx context.Context, hash string) (*BlobInfo, error) {
	var info BlobInfo
	var rawCreatedAt any
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT hash, size_bytes, COALESCE(created_by, ''), created_at FROM payload_blobs WHERE hash = ?1`,
		hash).Scan(&info.Hash, &info.SizeBytes, &info.CreatedBy, &rawCreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info.CreatedAt = canonicalTSFrom(rawCreatedAt)
	return &info, nil
}

func (s *SQLiteStorage) DeleteExpiredCapabilities(ctx context.Context) (int64, error) {
	res, err := s.getConn(ctx).ExecContext(ctx, `
		DELETE FROM actor_capabilities
		WHERE expires_at IS NOT NULL AND expires_at <= ?1
		  AND EXISTS (
			SELECT 1 FROM actor_capabilities o
			WHERE o.kind = actor_capabilities.kind AND o.target = actor_capabilities.target
			  AND o.id <> actor_capabilities.id
			  AND (o.expires_at IS NULL OR o.expires_at > ?1
			       OR o.created_at > actor_capabilities.created_at
			       OR (o.created_at = actor_capabilities.created_at AND o.id > actor_capabilities.id)))`,
		canonicalTS(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStorage) PostTask(ctx context.Context, task *ActorTask) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO actor_tasks (id, channel, data, state)
		VALUES (?1, ?2, ?3, 'open')`,
		task.ID, task.Channel, string(task.Data))
	if err != nil {
		return err
	}
	return s.publishTaskNotice(ctx, task.Channel, taskPostedNotice(task.ID, task.Channel))
}

// publishTaskNotice appends an event to the channel's task-event stream
// ({channel}:tasks). Runs on the caller's conn so it joins an enclosing
// transaction when present.
func (s *SQLiteStorage) publishTaskNotice(ctx context.Context, channel, dataJSON string) error {
	msgID := "task-notice-" + uuid.New().String()
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO channel_messages (channel, data, message_id, data_type)
		VALUES (?1, ?2, ?3, 'json')`,
		channel+":tasks", dataJSON, msgID)
	return err
}

func (s *SQLiteStorage) GetTask(ctx context.Context, taskID string) (*ActorTask, error) {
	var t ActorTask
	var result sql.NullString
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT id, channel, COALESCE(data, ''), state,
			COALESCE(claimed_by, ''), COALESCE(result, ''), attempts
		FROM actor_tasks WHERE id = ?1`, taskID).Scan(
		&t.ID, &t.Channel, &t.Data, &t.State, &t.ClaimedBy, &result, &t.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Result = []byte(result.String)
	return &t, nil
}

func (s *SQLiteStorage) ClaimTask(ctx context.Context, channel, claimant string) (*ActorTask, error) {
	conn := s.getConn(ctx)
	// Optimistic claim: pick the oldest open task, then take it with a
	// conditional UPDATE — exactly one claimant wins (spec/TaskClaim.tla) and
	// the winner knows which row it took. A claimant that lost the race
	// retries against the next oldest task.
	for range 3 {
		var id string
		err := conn.QueryRowContext(ctx, `
			SELECT id FROM actor_tasks
			WHERE channel = ? AND state = 'open'
			ORDER BY created_at, id LIMIT 1`, channel).Scan(&id)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		res, err := conn.ExecContext(ctx, `
			UPDATE actor_tasks SET state = 'claimed', claimed_by = ?, claimed_at = ?,
				attempts = attempts + 1, updated_at = ?
			WHERE id = ? AND state = 'open'`,
			claimant, now, now, id)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue // another claimant took it first
		}
		t, err := s.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, fmt.Errorf("claimed task %s vanished", id)
		}
		if err := s.publishTaskNotice(ctx, channel, taskClaimedNotice(id, claimant)); err != nil {
			return nil, err
		}
		return t, nil
	}
	return nil, nil
}

func (s *SQLiteStorage) CompleteTask(ctx context.Context, taskID, claimant string, result []byte, failed bool) error {
	state := "done"
	if failed {
		state = "failed"
	}
	res, err := s.getConn(ctx).ExecContext(ctx, `
		UPDATE actor_tasks SET state = ?1, result = ?2, updated_at = ?3
		WHERE id = ?4 AND claimed_by = ?5 AND state = 'claimed'`,
		state, string(result), time.Now().UTC(), taskID, claimant)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if n == 0 {
		if task == nil {
			return ErrTaskNotFound
		}
		if task.ClaimedBy == claimant && task.State == state {
			return nil // already settled by this claimant: a re-executed step
		}
		return ErrTaskConflict
	}
	return s.publishTaskNotice(ctx, task.Channel, taskStateNotice(taskID, state))
}

func (s *SQLiteStorage) ReclaimStaleTasks(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge)
	res, err := s.getConn(ctx).ExecContext(ctx, `
		UPDATE actor_tasks SET state = 'open', claimed_by = NULL,
			claimed_at = NULL, updated_at = ?1
		WHERE state = 'claimed' AND claimed_at < ?2`,
		time.Now().UTC(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStorage) ListTasks(ctx context.Context, channel, state string) ([]*ActorTask, error) {
	query := `SELECT id, channel, COALESCE(data, ''), state,
		COALESCE(claimed_by, ''), COALESCE(result, ''), attempts
		FROM actor_tasks WHERE channel = ?1`
	args := []any{channel}
	if state != "" {
		query += ` AND state = ?2`
		args = append(args, state)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.getConn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []*ActorTask
	for rows.Next() {
		var t ActorTask
		if err := rows.Scan(&t.ID, &t.Channel, &t.Data, &t.State, &t.ClaimedBy, &t.Result, &t.Attempts); err != nil {
			return nil, err
		}
		tasks = append(tasks, &t)
	}
	return tasks, rows.Err()
}

func (s *SQLiteStorage) ChannelMessagesSince(ctx context.Context, channel, messageID string) ([]*ChannelMessage, error) {
	if messageID == "" {
		return s.ChannelMessagesAfter(ctx, channel, 0)
	}
	var cursor int64
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT id FROM channel_messages WHERE channel = ?1 AND message_id = ?2`, channel, messageID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s on %s", ErrCursorNotFound, messageID, channel)
	}
	if err != nil {
		return nil, err
	}
	return s.ChannelMessagesAfter(ctx, channel, cursor)
}

func (s *SQLiteStorage) ChannelMessagesAfter(ctx context.Context, channel string, afterID int64) ([]*ChannelMessage, error) {
	rows, err := s.getConn(ctx).QueryContext(ctx, `
		SELECT id, channel, message_id, data_type, COALESCE(data, ''), data_binary, metadata, published_at
		FROM channel_messages
		WHERE channel = ?1 AND id > ?2
		ORDER BY id`,
		channel, afterID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var msgs []*ChannelMessage
	for rows.Next() {
		var m ChannelMessage
		var pub any
		if err := rows.Scan(&m.ID, &m.Channel, &m.MessageID, &m.DataType,
			&m.Data, &m.DataBinary, &m.Metadata, &pub); err != nil {
			return nil, err
		}
		switch v := pub.(type) {
		case time.Time:
			m.PublishedAt = v
		case string:
			if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
				m.PublishedAt = t
			}
		case []byte:
			if t, err := time.Parse("2006-01-02 15:04:05", string(v)); err == nil {
				m.PublishedAt = t
			}
		}
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}
