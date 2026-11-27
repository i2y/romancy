package romancy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// Timers and receive timeouts never fire early, although SQLite stores
// deadlines to the second: they are rounded up before they are stored.
func TestDeadlines_NeverFireEarly(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithTimerCheckInterval(50*time.Millisecond),
		WithEventTimeoutInterval(50*time.Millisecond),
	)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))

	stamp := DefineActivity("deadline_stamp", func(context.Context, string) (time.Time, error) {
		return time.Now(), nil
	})
	const wait = 300 * time.Millisecond
	wf := DefineWorkflow("deadline_waits", func(ctx *WorkflowContext, _ struct{}) ([]time.Duration, error) {
		t0, err := stamp.Execute(ctx, "t0")
		if err != nil {
			return nil, err
		}
		if err := Sleep(ctx, wait); err != nil {
			return nil, err
		}
		t1, err := stamp.Execute(ctx, "t1")
		if err != nil {
			return nil, err
		}
		if err := Subscribe(ctx, "deadline-empty", ModeBroadcast); err != nil {
			return nil, err
		}
		_, err = Receive[string](ctx, "deadline-empty", WithReceiveTimeout(wait))
		if err == nil {
			return nil, fmt.Errorf("receive: a message on an empty channel")
		}
		var timeout *ChannelMessageTimeoutError
		if !errors.As(err, &timeout) {
			return nil, err // the suspension, or a real failure
		}
		t2, err := stamp.Execute(ctx, "t2")
		if err != nil {
			return nil, err
		}
		return []time.Duration{t1.Sub(t0), t2.Sub(t1)}, nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	inst := waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status, "output %s", inst.OutputData)
	result, err := GetWorkflowResult[[]time.Duration](ctx, app, id)
	require.NoError(t, err)
	require.Len(t, result.Output, 2)
	require.GreaterOrEqual(t, result.Output[0], wait, "the sleep ended early")
	require.GreaterOrEqual(t, result.Output[1], wait, "the receive timed out early")
}
