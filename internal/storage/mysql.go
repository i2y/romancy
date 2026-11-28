// Package storage provides the storage layer for Romancy.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
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
// Output: user:password@tcp(host:port)/dbname?clientFoundRows=true&loc=UTC&multiStatements=true&parseTime=true&time_zone=...&transaction_isolation=...&param=value
//
// clientFoundRows=true makes RowsAffected count matched rows, as SQLite and
// PostgreSQL do. The conditional UPDATEs behind lock acquisition, lease
// refresh, fenced writes and claims take "0 rows" to mean "lost the race";
// with MySQL's default changed-rows count, re-writing identical values (the
// same holder re-acquiring or refreshing within one second) also reports 0.
// mysqlSessionTimeZone pins the session time zone: NOW() in DATETIME
// context follows it, while the driver writes time.Time in loc (UTC). The
// two agree only when both are UTC; on a server whose default zone is not
// UTC, deadlines and lease expiries would be off by the zone offset.
const mysqlSessionTimeZone = "'+00:00'"

// mysqlSessionIsolation runs every session at READ COMMITTED, PostgreSQL's
// default and the level the kernel's protocols assume (SEMANTICS §1):
// REPEATABLE READ would serve a transaction's later reads from a stale
// snapshot and add next-key/gap locks around the conditional updates.
const mysqlSessionIsolation = "'READ-COMMITTED'"

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
		if !strings.Contains(connStr, "clientFoundRows=") {
			connStr += "&clientFoundRows=true"
		}
		if !strings.Contains(connStr, "time_zone=") {
			connStr += "&time_zone=" + url.QueryEscape(mysqlSessionTimeZone)
		}
		if !strings.Contains(connStr, "transaction_isolation=") {
			connStr += "&transaction_isolation=" + url.QueryEscape(mysqlSessionIsolation)
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
	if params.Get("clientFoundRows") == "" {
		params.Set("clientFoundRows", "true")
	}
	if params.Get("time_zone") == "" {
		params.Set("time_zone", mysqlSessionTimeZone)
	}
	if params.Get("transaction_isolation") == "" {
		params.Set("transaction_isolation", mysqlSessionIsolation)
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

	// Ensure workflow definition exists (required by foreign key constraint);
	// an existing one is kept (see isDuplicateKey)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_definitions (workflow_name, source_hash, source_code)
		VALUES (?, ?, ?)
	`, instance.WorkflowName, instance.SourceHash, "")
	if err != nil && !isDuplicateKey(err) {
		return fmt.Errorf("failed to ensure workflow definition: %w", err)
	}

	sandboxed, err := resolveSandboxed(ctx, conn, s.driver, instance)
	if err != nil {
		return err
	}
	instance.Sandboxed = sandboxed

	_, err = conn.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			instance_id, workflow_name, status, input_data, source_hash, owner_service, framework, sandboxed, started_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, instance.InstanceID, instance.WorkflowName, instance.Status,
		string(instance.InputData), instance.SourceHash, instance.OwnerService, instanceFramework(instance),
		sandboxedInt(sandboxed), instance.StartedAt.UTC(), instance.UpdatedAt.UTC())
	if isDuplicateKey(err) {
		return fmt.Errorf("%w: %w", ErrInstanceAlreadyExists, err)
	}
	return err
}

// isDuplicateKey reports MySQL's duplicate-key error (ER_DUP_ENTRY): the
// INSERT hit a unique key and inserted nothing, which is what ON CONFLICT
// DO NOTHING does on the other dialects. INSERT IGNORE is not used for
// this: it also turns every other error into a warning, inserting a value
// truncated to its column or a row whose foreign key is missing. A
// duplicate-key error does not abort the enclosing transaction.
func isDuplicateKey(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1062
}

// GetInstance retrieves a workflow instance by ID.
func (s *MySQLStorage) GetInstance(ctx context.Context, instanceID string) (*WorkflowInstance, error) {
	conn := s.getConn(ctx)
	row := conn.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, status, input_data, output_data,
			   current_activity_id, source_hash, owner_service, framework, sandboxed,
			   locked_by, locked_at, lock_timeout_seconds, lock_expires_at,
			   started_at, updated_at
		FROM workflow_instances WHERE instance_id = ?
	`, instanceID)

	var inst WorkflowInstance
	var inputData, outputData, activityID, sourceHash, ownerService, framework sql.NullString
	var lockedBy sql.NullString
	var sandboxed int
	var lockedAt, lockExpiresAt sql.NullTime
	var lockTimeout sql.NullInt64

	err := row.Scan(
		&inst.InstanceID, &inst.WorkflowName, &inst.Status,
		&inputData, &outputData, &activityID, &sourceHash, &ownerService, &framework, &sandboxed,
		&lockedBy, &lockedAt, &lockTimeout, &lockExpiresAt,
		&inst.StartedAt, &inst.UpdatedAt,
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

// lockHolder answers the worker that holds the instance's lock, or "" when it
// is unlocked or the instance does not exist.
func (s *MySQLStorage) lockHolder(ctx context.Context, instanceID string) (string, error) {
	var lockedBy sql.NullString
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT locked_by FROM workflow_instances WHERE instance_id = ? FOR SHARE
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
// nil when the lock is held (the write was a no-op for another reason, such
// as writing a value the row already had).
func (s *MySQLStorage) lockLost(ctx context.Context, instanceID, workerID string) error {
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
func (s *MySQLStorage) UpdateInstanceStatusIfLocked(ctx context.Context, instanceID string, status WorkflowStatus, errorMsg, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, updated_at = NOW()
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
func (s *MySQLStorage) statusWriteRefused(ctx context.Context, instanceID, workerID string) error {
	var lockedBy sql.NullString
	var status WorkflowStatus
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT locked_by, status FROM workflow_instances WHERE instance_id = ? FOR SHARE
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

// UpdateInstanceOutputIfLocked updates the output only while workerID holds
// the instance's lock; it answers ErrLockNotHeld otherwise.
func (s *MySQLStorage) UpdateInstanceOutputIfLocked(ctx context.Context, instanceID string, outputData []byte, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET output_data = ?, status = 'completed', updated_at = NOW()
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

func (s *MySQLStorage) UpdateInstanceInputIfLocked(ctx context.Context, instanceID string, inputData []byte, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET input_data = ?, updated_at = NOW()
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
func (s *MySQLStorage) FindResumableWorkflows(ctx context.Context, workflowNames []string, limit int) ([]*ResumableWorkflow, error) {
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
// the lease. MySQL allows FOR UPDATE only at the top level, so the selection
// and the update run inside one transaction: the SELECT FOR UPDATE
// SKIP LOCKED holds the rows until commit and concurrent workers take
// disjoint sets.
func (s *MySQLStorage) ClaimResumableWorkflows(ctx context.Context, workerID string, versions []WorkflowVersion, leaseSec, limit int) ([]*ResumableWorkflow, error) {
	if limit <= 0 {
		limit = 100
	}
	if len(versions) == 0 {
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	filter, filterArgs := versionFilter(versions, questionMarks)
	now := time.Now().UTC()
	args := make([]any, 0, len(filterArgs)+3)
	args = append(args, StatusRunning)
	args = append(args, filterArgs...)
	args = append(args, now, limit)

	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT instance_id, workflow_name
		FROM workflow_instances
		WHERE status = ?
		AND framework = 'go'
		AND %s
		AND (locked_by IS NULL OR locked_by = '' OR lock_expires_at < ?)
		ORDER BY updated_at ASC
		LIMIT ?
		FOR UPDATE SKIP LOCKED
	`, filter), args...)
	if err != nil {
		return nil, err
	}

	var workflows []*ResumableWorkflow
	var ids []string
	for rows.Next() {
		var wf ResumableWorkflow
		if err := rows.Scan(&wf.InstanceID, &wf.WorkflowName); err != nil {
			_ = rows.Close()
			return nil, err
		}
		workflows = append(workflows, &wf)
		ids = append(ids, wf.InstanceID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	expiresAt := now.Add(time.Duration(leaseSec) * time.Second)
	idPlaceholders := make([]string, len(ids))
	updateArgs := make([]any, 0, len(ids)+4)
	updateArgs = append(updateArgs, workerID, now, expiresAt, now)
	for i, id := range ids {
		idPlaceholders[i] = "?"
		updateArgs = append(updateArgs, id)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET locked_by = ?, locked_at = ?, lock_expires_at = ?, updated_at = ?
		WHERE instance_id IN (%s)
	`, strings.Join(idPlaceholders, ",")), updateArgs...); err != nil {
		return nil, err
	}

	return workflows, tx.Commit()
}

// TryAcquireLock attempts to acquire a lock on a workflow instance.
// A single conditional UPDATE makes the check and the claim one atomic
// operation (see PostgresStorage.TryAcquireLock); RowsAffected is the verdict.
func (s *MySQLStorage) TryAcquireLock(ctx context.Context, instanceID, workerID string, timeoutSec int) (bool, error) {
	conn := s.getConn(ctx)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)

	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET locked_by = ?, locked_at = ?, lock_expires_at = ?, updated_at = ?
		WHERE instance_id = ?
		AND (locked_by IS NULL OR locked_by = '' OR locked_by = ? OR lock_expires_at < ?)
	`, workerID, now, expiresAt, now, instanceID, workerID, now)
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
	result, err := conn.ExecContext(ctx, `
		UPDATE workflow_instances
		SET lock_expires_at = ?, updated_at = NOW()
		WHERE instance_id = ? AND locked_by = ?
	`, expiresAt, instanceID, workerID)
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
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, event_data_binary, data_type)
		VALUES (?, ?, ?, ?, ?, ?)
	`, event.InstanceID, event.ActivityID, event.EventType, string(event.EventData), event.EventDataBinary, event.DataType)
	if isDuplicateKey(err) {
		return nil // already recorded
	}
	return err
}

// AppendHistoryIfLocked appends the event only while workerID holds the
// instance's lock; it answers ErrLockNotHeld otherwise. A duplicate event is
// a no-op, as for AppendHistory.
// The lock check locks the instance row as an update of it would
// (FOR UPDATE), so the append is atomic with a lease steal: a steal waits
// for the append to commit, and an append that waited for a steal re-checks
// the new holder and is refused. A plain check reads a snapshot, and a stale
// run's record could land after the new holder loaded its history. (A share
// lock would deadlock a transaction that goes on to update the instance row,
// such as a timer firing, against a writer already waiting for it.)
func (s *MySQLStorage) AppendHistoryIfLocked(ctx context.Context, event *HistoryEvent, workerID string) error {
	conn := s.getConn(ctx)
	result, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_history (instance_id, activity_id, event_type, event_data, event_data_binary, data_type)
		SELECT ?, ?, ?, ?, ?, ? FROM DUAL
		WHERE EXISTS (
			SELECT 1 FROM workflow_instances
			WHERE instance_id = ? AND locked_by = ?
			FOR UPDATE
		)
	`, event.InstanceID, event.ActivityID, event.EventType, string(event.EventData), event.EventDataBinary, event.DataType, event.InstanceID, workerID)
	if isDuplicateKey(err) {
		return nil // the lock check passed (a row was selected); already recorded
	}
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
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_timer_subscriptions (instance_id, timer_id, expires_at, activity_id)
		VALUES (?, ?, ?, ?)
	`, sub.InstanceID, sub.TimerID, sub.ExpiresAt.UTC(), sub.ActivityID)
	if isDuplicateKey(err) {
		return nil // already registered
	}
	return err
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
// Peek only: concurrent relayers must use ClaimOutboxEvents, which moves the
// rows to 'processing' atomically.
func (s *MySQLStorage) GetPendingOutboxEvents(ctx context.Context, limit int) ([]*OutboxEvent, error) {
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

// ClaimOutboxEvents atomically moves a batch of events to 'processing' and
// returns them. The SELECT FOR UPDATE SKIP LOCKED inside the transaction
// gives concurrent relayers disjoint batches. published_at is stamped with
// the claim time so a claim left behind by a crashed relayer becomes
// claimable again past staleBefore.
func (s *MySQLStorage) ClaimOutboxEvents(ctx context.Context, staleBefore time.Time, limit int) ([]*OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, event_type, event_source, event_data, data_type, content_type, status, retry_count, created_at
		FROM outbox_events
		WHERE status = 'pending'
		OR (status = 'processing' AND COALESCE(claimed_at, published_at) < ?)
		ORDER BY created_at ASC
		LIMIT ?
		FOR UPDATE SKIP LOCKED
	`, staleBefore.UTC(), limit)
	if err != nil {
		return nil, err
	}

	var events []*OutboxEvent
	var ids []string
	for rows.Next() {
		var e OutboxEvent
		var eventData sql.NullString
		if err := rows.Scan(&e.EventID, &e.EventType, &e.EventSource, &eventData, &e.DataType, &e.ContentType, &e.Status, &e.RetryCount, &e.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if eventData.Valid {
			e.EventData = []byte(eventData.String)
		}
		events = append(events, &e)
		ids = append(ids, e.EventID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE outbox_events
		SET status = 'processing', claimed_at = NOW()
		WHERE event_id IN (%s)
	`, strings.Join(placeholders, ",")), args...); err != nil {
		return nil, err
	}

	return events, tx.Commit()
}

// ReleaseOutboxEvent returns a claimed event to 'pending'.
func (s *MySQLStorage) ReleaseOutboxEvent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'pending', claimed_at = NULL
		WHERE event_id = ? AND status = 'processing'
	`, eventID)
	return err
}

// MarkOutboxEventSent marks an outbox event as published.
func (s *MySQLStorage) MarkOutboxEventSent(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'published', published_at = NOW(), claimed_at = NULL
		WHERE event_id = ?
	`, eventID)
	return err
}

// MarkOutboxEventFailed marks an outbox event as failed.
func (s *MySQLStorage) MarkOutboxEventFailed(ctx context.Context, eventID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'failed', claimed_at = NULL
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

// ClearCompensations removes every compensation registered for the instance.
func (s *MySQLStorage) ClearCompensations(ctx context.Context, instanceID string) error {
	conn := s.getConn(ctx)
	_, err := conn.ExecContext(ctx, `DELETE FROM workflow_compensations WHERE instance_id = ?`, instanceID)
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
		if isOwnDirectChannel(instanceID, channelName) {
			// The instance's own direct channel is a mailbox: the messages
			// sent to it before the subscription are delivered too.
			_, err = conn.ExecContext(ctx, `
				INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
				VALUES (?, ?, 0)
				ON DUPLICATE KEY UPDATE instance_id = instance_id
			`, instanceID, channelName)
		} else {
			_, err = conn.ExecContext(ctx, `
				INSERT INTO channel_delivery_cursors (instance_id, channel, last_delivered_id)
				SELECT ?, ?, COALESCE(MAX(id), 0)
				FROM channel_messages
				WHERE channel = ?
				ON DUPLICATE KEY UPDATE instance_id = instance_id
			`, instanceID, channelName, channelName)
		}
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

// GetChannelMode retrieves the mode for a channel (from any existing subscription).
// Returns empty string if no subscriptions exist.
func (s *MySQLStorage) GetChannelMode(ctx context.Context, channelName string) (ChannelMode, error) {
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
func (s *MySQLStorage) RegisterChannelReceiveAndReleaseLock(ctx context.Context, instanceID, channelName, workerID, activityID string, timeoutAt *time.Time, alsoChannels ...string) (bool, error) {
	var needTx bool
	if !s.InTransaction(ctx) {
		var err error
		ctx, err = s.BeginTransaction(ctx)
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
	channels := waitChannels(channelName, alsoChannels)
	pending := false
	for _, ch := range channels {
		p, err := s.hasPendingChannelMessage(ctx, instanceID, ch)
		if err != nil {
			return false, err
		}
		pending = pending || p
	}
	if pending {
		res, err := conn.ExecContext(ctx, `
			UPDATE workflow_instances
			SET locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
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
		SET status = ?, locked_by = NULL, locked_at = NULL, lock_expires_at = NULL, updated_at = NOW()
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

	// Update subscription with activity_id (non-null = waiting state). The
	// timeout is kept on the first channel only, so it fires once.
	for i, ch := range channels {
		deadline := utcTimePtr(timeoutAt)
		if i > 0 {
			deadline = nil
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE channel_subscriptions
			SET timeout_at = ?, activity_id = ?
			WHERE instance_id = ? AND channel = ?
		`, deadline, activityID, instanceID, ch); err != nil {
			return false, err
		}
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
func (s *MySQLStorage) hasPendingChannelMessage(ctx context.Context, instanceID, channelName string) (bool, error) {
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
		`, channelName, instanceID, time.Now().UTC().Add(-ChannelClaimStaleSeconds*time.Second)).Scan(&exists)
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

func (s *MySQLStorage) RequeueChannelWaitsWithPending(ctx context.Context) (int, error) {
	var needTx bool
	if !s.InTransaction(ctx) {
		var err error
		ctx, err = s.BeginTransaction(ctx)
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
	stale := time.Now().UTC().Add(-ChannelClaimStaleSeconds * time.Second)
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
			UPDATE workflow_instances SET status = ?, updated_at = NOW()
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
		// Competing mode: messages no live other instance has claimed. Claims
		// by this instance stay visible so a replay can drain them after a
		// crash; stale claims and claims by terminal owners are void.
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
		args = []any{channelName, instanceID, time.Now().UTC().Add(-ChannelClaimStaleSeconds * time.Second)}
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

	// Lock the message row first and resolve its message_id UUID, which the
	// claims table references. Every claimer does this, so competing claims
	// of one message queue up here: inside a transaction the loser waits for
	// the winner to commit and then finds the message consumed, instead of
	// racing it into a deadlock over the claim and the message rows.
	var messageUUID string
	err := conn.QueryRowContext(ctx, `
		SELECT message_id FROM channel_messages WHERE id = ? FOR UPDATE
	`, messageID).Scan(&messageUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := conn.ExecContext(ctx, `
		INSERT INTO channel_message_claims (message_id, instance_id)
		VALUES (?, ?)
	`, messageUUID, instanceID)
	switch {
	case isDuplicateKey(err):
		// Claimed already: see whether the claim can be taken over below.
	case err != nil:
		return false, err
	default:
		if affected, err := result.RowsAffected(); err != nil {
			return false, err
		} else if affected > 0 {
			return true, nil
		}
	}

	// Take over our own claim (idempotent), a stale claim, or one whose owner
	// reached a terminal status without consuming the message.
	now := time.Now().UTC()
	result, err = conn.ExecContext(ctx, `
		UPDATE channel_message_claims
		SET instance_id = ?, claimed_at = ?
		WHERE message_id = ?
			AND (instance_id = ?
				OR claimed_at < ?
				OR instance_id IN (
					SELECT instance_id FROM workflow_instances
					WHERE status IN ('completed', 'failed', 'cancelled')
				))
	`, instanceID, now, messageUUID, instanceID, now.Add(-ChannelClaimStaleSeconds*time.Second))
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

// ClearChannelWaitingState ends the wait registered on an instance's
// subscription to channelName, on every subscription it is registered on (a
// WaitEvent wait is registered on the event type and the instance's direct
// channel). An UPDATE cannot read its own table in a subquery on MySQL (error
// 1093), so the wait's activity id is read first.
func (s *MySQLStorage) ClearChannelWaitingState(ctx context.Context, instanceID, channelName string) error {
	conn := s.getConn(ctx)
	var activityID sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT activity_id FROM channel_subscriptions WHERE instance_id = ? AND channel = ?
	`, instanceID, channelName).Scan(&activityID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	_, err = conn.ExecContext(ctx, `
		UPDATE channel_subscriptions
		SET activity_id = NULL, timeout_at = NULL
		WHERE instance_id = ? AND (channel = ? OR (activity_id = ? AND activity_id != ''))
	`, instanceID, channelName, activityID)
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
	// One transaction: the claim, the receive record, the consume-delete, the
	// cursor and the status flip land together or not at all. A crash in
	// between must not leave a consumed message without its receive, or a
	// recorded receive whose wait is still open to a second message.
	if !s.InTransaction(ctx) {
		txCtx, err := s.BeginTransaction(ctx)
		if err != nil {
			return nil, err
		}
		res, err := s.DeliverChannelMessageWithLock(txCtx, instanceID, channelName, message, workerID, lockTimeoutSec)
		if err != nil || res == nil {
			_ = s.RollbackTransaction(txCtx)
			var myErr *mysql.MySQLError
			if errors.As(err, &myErr) && myErr.Number == 1213 {
				// Deadlock with a concurrent competing delivery of the same
				// message: one of the two goes through, the other is a skip.
				return nil, nil
			}
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

	// Step 2: Get workflow info for the result
	var workflowName string
	var status WorkflowStatus
	err = conn.QueryRowContext(ctx, `
		SELECT workflow_name, status FROM workflow_instances WHERE instance_id = ?
	`, instanceID).Scan(&workflowName, &status)
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
	// Only a subscriber that is waiting on this channel takes the message:
	// the history slot it fills is that wait's activity id. Anyone else —
	// resumed in the meantime, terminated, never waiting — leaves the message
	// pending for its next Receive instead of having it recorded where no
	// Receive will ever read it (and, in competing mode, deleted).
	if status != StatusWaitingForMessage || activityID == "" {
		_ = s.ReleaseLock(ctx, instanceID, workerID)
		return nil, nil
	}

	// Step 3.5: For competing mode, claim the message for this instance first
	// so other competing subscribers cannot also consume it (exactly-once).
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
		WHERE instance_id = ? AND status = ?
	`, StatusRunning, instanceID, StatusWaitingForMessage)
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
	_, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_group_memberships (instance_id, group_name)
		VALUES (?, ?)
	`, instanceID, groupName)
	if isDuplicateKey(err) {
		return nil // already a member
	}
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
func (s *MySQLStorage) GetArchivedHistory(ctx context.Context, instanceID string) ([]*ArchivedHistoryEvent, error) {
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
		var originalCreatedAt sql.NullTime
		err := rows.Scan(&e.ID, &e.InstanceID, &e.ActivityID, &e.EventType, &eventData, &e.EventDataBinary, &e.DataType, &originalCreatedAt, &e.ArchivedAt)
		if err != nil {
			return nil, err
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

// ========================================
// Actor Registry
// ========================================

// RegisterActorName binds name to instanceID. Registering a name the
// instance already holds succeeds (SEMANTICS §6: registration is
// idempotent); a name bound to another instance answers ErrActorNameTaken.
func (s *MySQLStorage) RegisterActorName(ctx context.Context, name, instanceID, framework string) error {
	conn := s.getConn(ctx)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO actor_names (name, instance_id, framework) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE name = name
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

func (s *MySQLStorage) UnregisterActorName(ctx context.Context, name string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `DELETE FROM actor_names WHERE name = ?`, name)
	return err
}

func (s *MySQLStorage) UnregisterOwnActorName(ctx context.Context, name, owner string) error {
	return deleteOwned(ctx, s.getConn(ctx), s.driver.Placeholder, "actor_names", "name", "instance_id", name, owner)
}

func (s *MySQLStorage) RevokeOwnCapability(ctx context.Context, capabilityID, owner string) error {
	return deleteOwned(ctx, s.getConn(ctx), s.driver.Placeholder, "actor_capabilities", "id", "granted_by", capabilityID, owner)
}

func (s *MySQLStorage) WhereisActor(ctx context.Context, name string) (string, error) {
	var id string
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT instance_id FROM actor_names WHERE name = ?
	`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *MySQLStorage) MonitorActor(ctx context.Context, watcherInstanceID, targetInstanceID, channel string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO actor_monitors (watcher_instance_id, target_instance_id, channel)
		VALUES (?, ?, ?)
	`, watcherInstanceID, targetInstanceID, channel)
	if isDuplicateKey(err) {
		return nil // already monitoring
	}
	return err
}

func (s *MySQLStorage) UnmonitorActor(ctx context.Context, watcherInstanceID, targetInstanceID string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		DELETE FROM actor_monitors WHERE watcher_instance_id = ? AND target_instance_id = ?
	`, watcherInstanceID, targetInstanceID)
	return err
}

func (s *MySQLStorage) EmitActorDownForTerminated(ctx context.Context) (int, error) {
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

func (s *MySQLStorage) GrantCapability(ctx context.Context, capability *ActorCapability) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO actor_capabilities (id, kind, target, secret_hash, granted_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, capability.ID, capability.Kind, capability.Target, capability.SecretHash, capability.GrantedBy,
		expiryParam(capability.ExpiresAt))
	return err
}

func (s *MySQLStorage) RevokeCapability(ctx context.Context, capabilityID string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		DELETE FROM actor_capabilities WHERE id = ?
	`, capabilityID)
	return err
}

func (s *MySQLStorage) HasCapabilityGrants(ctx context.Context, kind, target string) (bool, error) {
	conn := s.getConn(ctx)
	var count int
	err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM actor_capabilities WHERE kind = ? AND target = ?", kind, target).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *MySQLStorage) CheckCapability(ctx context.Context, kind, target, secretHash string) (bool, error) {
	var expiresAt any
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT expires_at FROM actor_capabilities
		WHERE kind = ? AND target = ? AND secret_hash = ?
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

func (s *MySQLStorage) PutBlob(ctx context.Context, hash string, data []byte, createdBy string) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO payload_blobs (hash, data, size_bytes, created_by)
		VALUES (?, ?, ?, ?)`, hash, data, len(data), createdBy)
	if isDuplicateKey(err) {
		return nil // content-addressed: the same bytes are stored already
	}
	return err
}

func (s *MySQLStorage) GetBlob(ctx context.Context, hash string) ([]byte, error) {
	var data []byte
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT data FROM payload_blobs WHERE hash = ?`, hash).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *MySQLStorage) FSGet(ctx context.Context, path string) (*FSObject, error) {
	var obj FSObject
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT path, COALESCE(owner, ''), data, version FROM actor_fs WHERE path = ?`,
		path).Scan(&obj.Path, &obj.Owner, &obj.Data, &obj.Version)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &obj, nil
}

func (s *MySQLStorage) FSPut(ctx context.Context, path string, data []byte, owner string, expectedVersion int64) error {
	var res sql.Result
	var err error
	now := time.Now().UTC()
	if expectedVersion == 0 {
		res, err = s.getConn(ctx).ExecContext(ctx, `
			INSERT INTO actor_fs (path, owner, data, version, created_at, updated_at)
			VALUES (?, ?, ?, 1, ?, ?)`,
			path, owner, data, now, now)
		if isDuplicateKey(err) {
			return ErrFSConflict // path already exists
		}
	} else {
		res, err = s.getConn(ctx).ExecContext(ctx, `
			UPDATE actor_fs SET data = ?, owner = ?, version = version + 1, updated_at = ?
			WHERE path = ? AND version = ?`,
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

func (s *MySQLStorage) FSDelete(ctx context.Context, path string, expectedVersion int64) error {
	var res sql.Result
	var err error
	if expectedVersion == 0 {
		res, err = s.getConn(ctx).ExecContext(ctx,
			`DELETE FROM actor_fs WHERE path = ?`, path)
	} else {
		res, err = s.getConn(ctx).ExecContext(ctx,
			`DELETE FROM actor_fs WHERE path = ? AND version = ?`, path, expectedVersion)
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

func (s *MySQLStorage) FSList(ctx context.Context, prefix string) ([]*FSObject, error) {
	rows, err := s.getConn(ctx).QueryContext(ctx,
		`SELECT path, COALESCE(owner, ''), version FROM actor_fs
			WHERE path LIKE ? ESCAPE '\\' ORDER BY path`,
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

func (s *MySQLStorage) GetBlobInfo(ctx context.Context, hash string) (*BlobInfo, error) {
	var info BlobInfo
	var rawCreatedAt any
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT hash, size_bytes, COALESCE(created_by, ''), created_at FROM payload_blobs WHERE hash = ?`,
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

func (s *MySQLStorage) DeleteExpiredCapabilities(ctx context.Context) (int64, error) {
	// MySQL cannot read the table a DELETE modifies in its subquery: the
	// doomed IDs are computed in a derived table first.
	now := canonicalTS(time.Now())
	res, err := s.getConn(ctx).ExecContext(ctx, `
		DELETE FROM actor_capabilities WHERE id IN (
			SELECT id FROM (
				SELECT c.id FROM actor_capabilities c
				WHERE c.expires_at IS NOT NULL AND c.expires_at <= ?
				  AND EXISTS (
					SELECT 1 FROM actor_capabilities o
					WHERE o.kind = c.kind AND o.target = c.target AND o.id <> c.id
					  AND (o.expires_at IS NULL OR o.expires_at > ?
					       OR o.created_at > c.created_at
					       OR (o.created_at = c.created_at AND o.id > c.id)))
			) AS doomed)`,
		now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *MySQLStorage) PostTask(ctx context.Context, task *ActorTask) error {
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO actor_tasks (id, channel, data, state)
		VALUES (?, ?, ?, 'open')`,
		task.ID, task.Channel, string(task.Data))
	if err != nil {
		return err
	}
	return s.publishTaskNotice(ctx, task.Channel, taskPostedNotice(task.ID, task.Channel))
}

// publishTaskNotice appends an event to the channel's task-event stream
// ({channel}:tasks). Runs on the caller's conn so it joins an enclosing
// transaction when present.
func (s *MySQLStorage) publishTaskNotice(ctx context.Context, channel, dataJSON string) error {
	msgID := "task-notice-" + uuid.New().String()
	_, err := s.getConn(ctx).ExecContext(ctx, `
		INSERT INTO channel_messages (channel, data, message_id, data_type)
		VALUES (?, ?, ?, 'json')`,
		channel+":tasks", dataJSON, msgID)
	return err
}

func (s *MySQLStorage) GetTask(ctx context.Context, taskID string) (*ActorTask, error) {
	var t ActorTask
	var result sql.NullString
	err := s.getConn(ctx).QueryRowContext(ctx, `
		SELECT id, channel, COALESCE(data, ''), state,
			COALESCE(claimed_by, ''), COALESCE(result, ''), attempts
		FROM actor_tasks WHERE id = ?`, taskID).Scan(
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

func (s *MySQLStorage) ClaimTask(ctx context.Context, channel, claimant string) (*ActorTask, error) {
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

func (s *MySQLStorage) CompleteTask(ctx context.Context, taskID, claimant string, result []byte, failed bool) error {
	state := "done"
	if failed {
		state = "failed"
	}
	res, err := s.getConn(ctx).ExecContext(ctx, `
		UPDATE actor_tasks SET state = ?, result = ?, updated_at = ?
		WHERE id = ? AND claimed_by = ? AND state = 'claimed'`,
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

func (s *MySQLStorage) ReclaimStaleTasks(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge)
	res, err := s.getConn(ctx).ExecContext(ctx, `
		UPDATE actor_tasks SET state = 'open', claimed_by = NULL,
			claimed_at = NULL, updated_at = ?
		WHERE state = 'claimed' AND claimed_at < ?`,
		time.Now().UTC(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *MySQLStorage) ListTasks(ctx context.Context, channel, state string) ([]*ActorTask, error) {
	query := `SELECT id, channel, COALESCE(data, ''), state,
		COALESCE(claimed_by, ''), COALESCE(result, ''), attempts
		FROM actor_tasks WHERE channel = ?`
	args := []any{channel}
	if state != "" {
		query += ` AND state = ?`
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

func (s *MySQLStorage) ChannelMessagesSince(ctx context.Context, channel, messageID string) ([]*ChannelMessage, error) {
	if messageID == "" {
		return s.ChannelMessagesAfter(ctx, channel, 0)
	}
	var cursor int64
	err := s.getConn(ctx).QueryRowContext(ctx,
		`SELECT id FROM channel_messages WHERE channel = ? AND message_id = ?`, channel, messageID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s on %s", ErrCursorNotFound, messageID, channel)
	}
	if err != nil {
		return nil, err
	}
	return s.ChannelMessagesAfter(ctx, channel, cursor)
}

func (s *MySQLStorage) ChannelMessagesAfter(ctx context.Context, channel string, afterID int64) ([]*ChannelMessage, error) {
	rows, err := s.getConn(ctx).QueryContext(ctx, `
		SELECT id, channel, message_id, data_type, COALESCE(data, ''), data_binary, metadata, published_at
		FROM channel_messages
		WHERE channel = ? AND id > ?
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
