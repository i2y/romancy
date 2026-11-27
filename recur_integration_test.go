//go:build integration

package romancy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
)

// Recur commits its archive, cleanups and restart record as one transaction
// that first proves the lock, on PostgreSQL and MySQL as on SQLite (see
// replay's TestEngine_RecurAfterLostLeaseChangesNothing).
func TestIntegration_RecurIsOneFencedTransaction(t *testing.T) {
	backends := []struct {
		name   string
		create func(*testing.T, ...Option) (*App, func())
		open   func(string) (storage.Storage, error)
	}{
		{"postgres", createTestAppWithPostgres, func(url string) (storage.Storage, error) {
			return storage.NewPostgresStorage(url)
		}},
		{"mysql", createTestAppWithMySQL, func(url string) (storage.Storage, error) {
			return storage.NewMySQLStorage(url)
		}},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			app, cleanup := b.create(t)
			defer cleanup()
			ctx := context.Background()
			// The app is not started, so no poller resumes these instances;
			// the engine runs on the storage the app would open.
			s, err := b.open(app.config.databaseURL)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			engine := replay.NewEngine(s, &hooks.NoOpHooks{}, "worker-1")

			start := func(id string, body func(*replay.ExecutionContext)) error {
				now := time.Now().UTC()
				require.NoError(t, s.CreateInstance(ctx, &storage.WorkflowInstance{
					InstanceID:   id,
					WorkflowName: "recur_tx",
					Status:       storage.StatusRunning,
					InputData:    []byte(`{"count":1}`),
					StartedAt:    now,
					UpdatedAt:    now,
				}))
				return engine.StartWorkflow(ctx, id, "recur_tx", nil, func(execCtx *replay.ExecutionContext) (any, error) {
					require.NoError(t, engine.RecordActivityResult(ctx, id, execCtx.LockID(), "step:1", "ok", nil))
					require.NoError(t, s.SubscribeToChannel(ctx, id, "inbox", storage.ChannelModeBroadcast))
					body(execCtx)
					return nil, replay.NewRecurSuspend(id, map[string]int{"count": 2})
				})
			}

			// A run that holds its lease recurs.
			require.NoError(t, start("recur-tx-live", func(*replay.ExecutionContext) {}))
			got, err := s.GetInstance(ctx, "recur-tx-live")
			require.NoError(t, err)
			assert.Equal(t, storage.StatusRecurred, got.Status)
			assert.JSONEq(t, `{"count":2}`, string(got.InputData))
			archived, err := s.GetArchivedHistory(ctx, "recur-tx-live")
			require.NoError(t, err)
			assert.Len(t, archived, 1)
			history, _, err := s.GetHistoryPaginated(ctx, "recur-tx-live", 0, 10)
			require.NoError(t, err)
			if assert.Len(t, history, 1) {
				assert.Equal(t, "recur", history[0].ActivityID)
			}
			sub, err := s.GetChannelSubscription(ctx, "recur-tx-live", "inbox")
			require.NoError(t, err)
			assert.Nil(t, sub)

			// A run that lost its lease recurs: the new owner's state stays.
			const newOwner = "worker-2#1"
			err = start("recur-tx-stale", func(execCtx *replay.ExecutionContext) {
				require.NoError(t, s.ReleaseLock(ctx, execCtx.InstanceID(), execCtx.LockID()))
				acquired, err := s.TryAcquireLock(ctx, execCtx.InstanceID(), newOwner, replay.LockLeaseSeconds)
				require.NoError(t, err)
				require.True(t, acquired)
			})
			require.ErrorIs(t, err, storage.ErrLockNotHeld)
			got, err = s.GetInstance(ctx, "recur-tx-stale")
			require.NoError(t, err)
			assert.Equal(t, storage.StatusRunning, got.Status)
			assert.JSONEq(t, `{"count":1}`, string(got.InputData))
			assert.Equal(t, newOwner, got.LockedBy)
			archived, err = s.GetArchivedHistory(ctx, "recur-tx-stale")
			require.NoError(t, err)
			assert.Empty(t, archived)
			history, _, err = s.GetHistoryPaginated(ctx, "recur-tx-stale", 0, 10)
			require.NoError(t, err)
			if assert.Len(t, history, 1, "the new owner's history was archived") {
				assert.Equal(t, "step:1", history[0].ActivityID)
			}
			sub, err = s.GetChannelSubscription(ctx, "recur-tx-stale", "inbox")
			require.NoError(t, err)
			assert.NotNil(t, sub, "the new owner's subscription was removed")
		})
	}
}
