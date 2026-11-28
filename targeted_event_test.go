package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// Targeted CloudEvents (SEMANTICS §3c): an event with the romancyinstanceid
// extension is stored on its target's direct channel "{type}:{instance}", so
// no other instance receives it, and WaitEvent in the target receives it like
// the events of its type.

type paymentDone struct {
	N int `json:"n"`
}

// postTargetedEvent posts a CloudEvent for one instance (instanceID != "")
// or for every waiter of its type.
func postTargetedEvent(t *testing.T, app *App, eventType, instanceID string, n int) {
	t.Helper()
	event := map[string]any{
		"specversion": "1.0", "type": eventType, "source": "test", "id": eventType + "-" + instanceID,
		"data": map[string]any{"n": n},
	}
	if instanceID != "" {
		event["romancyinstanceid"] = instanceID
	}
	body, err := json.Marshal(event)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
}

func startTargetedTestApp(t *testing.T, register func(app *App), opts ...Option) *App {
	t.Helper()
	app, cleanup := createTestApp(t, append([]Option{WithWorkflowResumptionInterval(20 * time.Millisecond)}, opts...)...)
	t.Cleanup(cleanup)
	register(app)
	require.NoError(t, app.Start(context.Background()))
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app
}

func requireStatus(t *testing.T, app *App, instanceID string, want storage.WorkflowStatus) {
	t.Helper()
	require.True(t, waitForStatus(t, context.Background(), app, instanceID, want, 5*time.Second),
		"instance %s did not reach %s", instanceID, want)
}

func resultOf(t *testing.T, app *App, instanceID string) []int {
	t.Helper()
	requireStatus(t, app, instanceID, storage.StatusCompleted)
	res, err := GetWorkflowResult[[]int](context.Background(), app, instanceID)
	require.NoError(t, err)
	return res.Output
}

// A targeted event reaches its target's WaitEvent and no other waiter of the
// type; an event of the type reaches the others.
func TestTargetedEvent_ReachesOnlyItsTarget(t *testing.T) {
	wf := DefineWorkflow("pay_wait", func(ctx *WorkflowContext, _ struct{}) ([]int, error) {
		ev, err := WaitEvent[paymentDone](ctx, "payment.done")
		if err != nil {
			return nil, err
		}
		return []int{ev.Data.N}, nil
	})
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) })
	ctx := context.Background()
	a, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	b, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	requireStatus(t, app, a, storage.StatusWaitingForMessage)
	requireStatus(t, app, b, storage.StatusWaitingForMessage)

	postTargetedEvent(t, app, "payment.done", a, 1)
	require.Equal(t, []int{1}, resultOf(t, app, a))
	time.Sleep(200 * time.Millisecond)
	inst, err := app.GetInstance(ctx, b)
	require.NoError(t, err)
	require.Equal(t, storage.StatusWaitingForMessage, inst.Status, "another waiter of the type received the targeted event")

	postTargetedEvent(t, app, "payment.done", "", 2)
	require.Equal(t, []int{2}, resultOf(t, app, b))
}

// A targeted event that arrives before its target calls WaitEvent waits in
// the target's direct channel.
func TestTargetedEvent_ArrivedBeforeTheWait(t *testing.T) {
	release := make(chan struct{})
	hold := DefineActivity("hold_before_wait", func(ctx context.Context, _ struct{}) (struct{}, error) {
		<-release
		return struct{}{}, nil
	}, WithTransactional[struct{}, struct{}](false))
	wf := DefineWorkflow("pay_late_wait", func(ctx *WorkflowContext, _ struct{}) ([]int, error) {
		if _, err := hold.Execute(ctx, struct{}{}); err != nil {
			return nil, err
		}
		ev, err := WaitEvent[paymentDone](ctx, "payment.late")
		if err != nil {
			return nil, err
		}
		return []int{ev.Data.N}, nil
	})
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) })
	id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
	require.NoError(t, err)

	postTargetedEvent(t, app, "payment.late", id, 7)
	close(release)
	require.Equal(t, []int{7}, resultOf(t, app, id))
}

// WaitEvent takes the oldest of the events pending on its two channels.
func TestTargetedEvent_OldestFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		targeted []bool // whether the first and the second event are targeted
	}{
		{"type first", []bool{false, true}},
		{"targeted first", []bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eventType := "payment.order." + strings.ReplaceAll(tc.name, " ", "_")
			release := make(chan struct{})
			hold := DefineActivity("hold_"+eventType, func(ctx context.Context, _ struct{}) (struct{}, error) {
				<-release
				return struct{}{}, nil
			}, WithTransactional[struct{}, struct{}](false))
			wf := DefineWorkflow("pay_twice_"+eventType, func(ctx *WorkflowContext, _ struct{}) ([]int, error) {
				// Subscribed before the events, so the untargeted one counts.
				if err := Subscribe(ctx, eventType, ModeBroadcast); err != nil {
					return nil, err
				}
				if _, err := hold.Execute(ctx, struct{}{}); err != nil {
					return nil, err
				}
				var got []int
				for range 2 {
					ev, err := WaitEvent[paymentDone](ctx, eventType)
					if err != nil {
						return nil, err
					}
					got = append(got, ev.Data.N)
				}
				return got, nil
			})
			app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) })
			id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				sub, err := app.storage.GetChannelSubscription(context.Background(), id, eventType)
				return err == nil && sub != nil
			}, 5*time.Second, 10*time.Millisecond)

			for i, targeted := range tc.targeted {
				target := ""
				if targeted {
					target = id
				}
				postTargetedEvent(t, app, eventType, target, i+1)
			}
			close(release)
			require.Equal(t, []int{1, 2}, resultOf(t, app, id))
		})
	}
}

// A timeout ends the wait on both channels: a targeted event that arrives
// later is not recorded into the wait that timed out.
func TestTargetedEvent_TimeoutEndsBothWaits(t *testing.T) {
	wf := DefineWorkflow("pay_timeout", func(ctx *WorkflowContext, _ struct{}) ([]int, error) {
		_, err := WaitEvent[paymentDone](ctx, "payment.slow", WithEventTimeout(50*time.Millisecond))
		var timedOut *ChannelMessageTimeoutError
		if !errors.As(err, &timedOut) {
			return nil, err // suspended, or no timeout: the test fails
		}
		// Timed out: wait for something else.
		ev, err := WaitEvent[paymentDone](ctx, "payment.next")
		if err != nil {
			return nil, err
		}
		return []int{-1, ev.Data.N}, nil
	})
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) },
		WithMessageCheckInterval(20*time.Millisecond), WithEventTimeoutInterval(20*time.Millisecond))
	ctx := context.Background()
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		sub, err := app.storage.GetChannelSubscription(ctx, id, "payment.next:"+id)
		return err == nil && sub != nil && sub.ActivityID != ""
	}, 5*time.Second, 10*time.Millisecond, "the workflow did not time out and wait again")

	for _, ch := range []string{"payment.slow", "payment.slow:" + id} {
		sub, err := app.storage.GetChannelSubscription(ctx, id, ch)
		require.NoError(t, err)
		require.Empty(t, sub.ActivityID, "the timed-out wait is still registered on %s", ch)
	}
	postTargetedEvent(t, app, "payment.slow", id, 1)
	postTargetedEvent(t, app, "payment.next", id, 2)
	require.Equal(t, []int{-1, 2}, resultOf(t, app, id))
}

// waitOnTypeAlone switches TestTargetedEvent_ResumesAWaitOnTheTypeAlone's
// workflow from the receive of earlier versions to WaitEvent.
var waitOnTypeAlone atomic.Bool

// An instance that began waiting before targeted events were stored on the
// direct channel waits on the event type alone. A targeted event resumes it,
// and the WaitEvent of the new version reads the event.
func TestTargetedEvent_ResumesAWaitOnTheTypeAlone(t *testing.T) {
	waitOnTypeAlone.Store(true)
	wf := DefineWorkflow("pay_upgraded", func(ctx *WorkflowContext, _ struct{}) ([]int, error) {
		var n int
		if waitOnTypeAlone.Load() {
			// What WaitEvent did before: the same steps, one channel.
			if err := Subscribe(ctx, "payment.upgraded", ModeBroadcast); err != nil {
				return nil, err
			}
			msg, err := Receive[json.RawMessage](ctx, "payment.upgraded")
			if err != nil {
				return nil, err
			}
			ev, err := convertToReceivedEvent[paymentDone](msg, "payment.upgraded")
			if err != nil {
				return nil, err
			}
			n = ev.Data.N
		} else {
			ev, err := WaitEvent[paymentDone](ctx, "payment.upgraded")
			if err != nil {
				return nil, err
			}
			n = ev.Data.N
		}
		return []int{n}, nil
	})
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) })
	id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)

	waitOnTypeAlone.Store(false) // the upgrade
	postTargetedEvent(t, app, "payment.upgraded", id, 5)
	require.Equal(t, []int{5}, resultOf(t, app, id))
}

// A direct subscription receives the targeted events sent before it too.
func TestTargetedEvent_DirectSubscriptionGetsEarlierEvents(t *testing.T) {
	release := make(chan struct{})
	hold := DefineActivity("hold_before_direct", func(ctx context.Context, _ struct{}) (struct{}, error) {
		<-release
		return struct{}{}, nil
	}, WithTransactional[struct{}, struct{}](false))
	wf := DefineWorkflow("pay_direct", func(ctx *WorkflowContext, _ struct{}) ([]int, error) {
		if _, err := hold.Execute(ctx, struct{}{}); err != nil {
			return nil, err
		}
		if err := Subscribe(ctx, "payment.direct", ModeDirect); err != nil {
			return nil, err
		}
		// The message data is the event's data.
		msg, err := Receive[paymentDone](ctx, "payment.direct")
		if err != nil {
			return nil, err
		}
		return []int{msg.Data.N}, nil
	})
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) })
	id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
	require.NoError(t, err)

	postTargetedEvent(t, app, "payment.direct", id, 3)
	close(release)
	require.Equal(t, []int{3}, resultOf(t, app, id))
}
