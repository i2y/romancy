package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// sandboxedInt converts the portable integer boolean representation used
// in workflow_instances.sandboxed.
func sandboxedInt(sandboxed bool) int {
	if sandboxed {
		return 1
	}
	return 0
}

// resolveSandboxed determines the sandbox flag for a new instance. A parent
// (spawn/supervision) or continuation source that is sandboxed forces the
// child to be sandboxed; the flag is monotone and can never be cleared.
func resolveSandboxed(ctx context.Context, conn Executor, driver Driver, instance *WorkflowInstance) (bool, error) {
	if instance.Sandboxed {
		return true, nil
	}

	seen := make(map[string]bool, 2)
	for _, parentID := range []string{instance.ParentInstanceID, instance.ContinuedFrom} {
		if parentID == "" || seen[parentID] {
			continue
		}
		seen[parentID] = true

		query := fmt.Sprintf(
			"SELECT sandboxed FROM workflow_instances WHERE instance_id = %s",
			driver.Placeholder(1),
		)
		var parentSandboxed int
		err := conn.QueryRowContext(ctx, query, parentID).Scan(&parentSandboxed)
		if err == sql.ErrNoRows {
			return false, fmt.Errorf("parent instance %s not found", parentID)
		}
		if err != nil {
			return false, fmt.Errorf("failed to check parent sandbox state: %w", err)
		}
		if parentSandboxed != 0 {
			return true, nil
		}
	}
	return false, nil
}
