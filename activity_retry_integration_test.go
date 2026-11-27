//go:build integration

package romancy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// Each activity attempt runs in its own transaction on PostgreSQL and MySQL
// too. On PostgreSQL the failed first attempt aborts its transaction, so the
// retry can only succeed in a new one (see TestActivityRetryRunsInNewTransaction).
func TestIntegration_ActivityRetryRunsInNewTransaction(t *testing.T) {
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
			s, err := b.open(app.config.databaseURL)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()

			committed, attempts := runRetryScenario(t, s, b.name)
			assert.Equal(t, int32(2), attempts)
			assert.Equal(t, []int{2}, committed)
		})
	}
}
