package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// ErrActorNameTaken is returned by RegisterActorName when the name is bound
// to a different instance.
var ErrActorNameTaken = errors.New("actor_names: name already registered")

// ErrNotOwner is returned when an instance revokes a capability it did not
// issue, or unregisters a name another instance holds.
var ErrNotOwner = errors.New("actor: not the owner")

// ErrCursorNotFound is returned by ChannelMessagesSince when no message with
// the cursor's message id exists (it was consumed or cleaned up).
var ErrCursorNotFound = errors.New("channel_messages: cursor message not found")

// Lifecycle payloads published by the kernel (DOWN, task notices). They are
// built with encoding/json so any id is encoded as a valid JSON string.

func downPayload(actor, status string) string {
	return mustJSON(struct {
		Type   string `json:"type"`
		Actor  string `json:"actor"`
		Status string `json:"status"`
	}{"DOWN", actor, status})
}

func taskPostedNotice(taskID, channel string) string {
	return mustJSON(struct {
		Type    string `json:"type"`
		TaskID  string `json:"task_id"`
		Channel string `json:"channel"`
	}{"task_posted", taskID, channel})
}

func taskClaimedNotice(taskID, claimant string) string {
	return mustJSON(struct {
		Type      string `json:"type"`
		TaskID    string `json:"task_id"`
		ClaimedBy string `json:"claimed_by"`
	}{"task_claimed", taskID, claimant})
}

func taskStateNotice(taskID, state string) string {
	return mustJSON(struct {
		Type   string `json:"type"`
		TaskID string `json:"task_id"`
		State  string `json:"state"`
	}{"task_state", taskID, state})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only fixed-shape structs of strings are encoded here
	}
	return string(b)
}

// terminalStatusList is the SQL list of statuses after which an instance
// never runs again. 'recurred' is not among them: a recurred instance is
// alive (SEMANTICS §5) — romancy continues it on the same instance id.
const terminalStatusList = `('completed', 'failed', 'cancelled')`

// IsTerminalStatus reports whether status is one of terminalStatusList.
func IsTerminalStatus(status WorkflowStatus) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// heldByCaller turns the RowsAffected of a write fenced on
// "locked_by = holder" into ErrLockNotHeld when no row matched.
func heldByCaller(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockNotHeld
	}
	return nil
}
