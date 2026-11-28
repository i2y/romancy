package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// postCloudEvent posts a structured-mode CloudEvent to the app's endpoint.
func postCloudEvent(app *App, eventType, data string) *httptest.ResponseRecorder {
	body := `{"type":"` + eventType + `","source":"test","specversion":"1.0","id":"e1","data":` + data + `}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	return w
}

// requireRetryable checks a refusal that the sender retries.
func requireRetryable(t *testing.T, w *httptest.ResponseRecorder, status int, errType string) {
	t.Helper()
	require.Equal(t, status, w.Code, w.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.Equal(t, errType, body["error_type"])
	require.Equal(t, "true", body["retryable"])
}

// acceptingApp is an app that accepts events with the storage that wrap
// makes of a fresh database, without the background tasks of Start.
func acceptingApp(t *testing.T, wrap func(storage.Storage) storage.Storage) *App {
	t.Helper()
	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "events.db"))
	require.NoError(t, err)
	require.NoError(t, storage.InitializeTestSchema(context.Background(), store))
	app := NewApp(WithWorkerID("worker-1"))
	app.storage = wrap(store)
	app.ctx, app.cancel = context.WithCancel(context.Background())
	app.running, app.initialized = true, true
	t.Cleanup(func() {
		_ = app.Shutdown(context.Background())
		_ = store.Close()
	})
	return app
}

type acceptedOrder struct {
	OrderID string `json:"order_id"`
}

// The endpoint answers 202 only once the event is stored: its channel
// message, and the instance of the workflow that handles its type
// (SEMANTICS §3c).
func TestCloudEvents_StoredBeforeAccepted(t *testing.T) {
	app, cleanup := createTestApp(t)
	defer cleanup()
	wf := DefineWorkflow("order.accepted", func(ctx *WorkflowContext, in acceptedOrder) (string, error) {
		return in.OrderID, nil
	})
	RegisterWorkflow(app, wf, WithEventHandler(true))
	require.NoError(t, app.Start(context.Background()))

	w := postCloudEvent(app, "order.accepted", `{"order_id":"o-1"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, countChannelMessages(t, app, "order.accepted"), "the event was not stored before the answer")
	var instances int
	require.NoError(t, app.Storage().DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workflow_instances WHERE workflow_name = ?`, "order.accepted").Scan(&instances))
	require.Equal(t, 1, instances, "the workflow that handles the event was not created before the answer")
}

// failingStorage fails the writes of an incoming event, like a database
// that cannot be reached when it arrives.
type failingStorage struct {
	storage.Storage
	failPublish, failCreate bool
}

var errUnreachable = errors.New("database unreachable")

func (s *failingStorage) PublishToChannel(ctx context.Context, channelName string, dataJSON, metadata []byte) (int64, error) {
	if s.failPublish {
		return 0, errUnreachable
	}
	return s.Storage.PublishToChannel(ctx, channelName, dataJSON, metadata)
}

func (s *failingStorage) CreateInstance(ctx context.Context, instance *storage.WorkflowInstance) error {
	if s.failCreate {
		return errUnreachable
	}
	return s.Storage.CreateInstance(ctx, instance)
}

// An event that cannot be stored is not accepted: the endpoint answers a
// retryable 500, and the sender sends it again.
func TestCloudEvents_StoreFailureIsRetryable(t *testing.T) {
	for name, failing := range map[string]*failingStorage{
		"publish":        {failPublish: true},
		"workflow start": {failCreate: true},
	} {
		t.Run(name, func(t *testing.T) {
			app := acceptingApp(t, func(s storage.Storage) storage.Storage {
				failing.Storage = s
				return failing
			})
			wf := DefineWorkflow("order.failing", func(ctx *WorkflowContext, in acceptedOrder) (string, error) {
				return in.OrderID, nil
			})
			RegisterWorkflow(app, wf, WithEventHandler(true))

			w := postCloudEvent(app, "order.failing", `{"order_id":"o-1"}`)
			requireRetryable(t, w, http.StatusInternalServerError, "InternalError")
			require.Contains(t, w.Body.String(), errUnreachable.Error())
		})
	}
}

// An app that is not running refuses events with a retryable 503: before
// Start, and from the beginning of Shutdown on.
func TestCloudEvents_RefusedWhileNotRunning(t *testing.T) {
	app, cleanup := createTestApp(t)
	defer cleanup()

	requireRetryable(t, postCloudEvent(app, "early.event", `{}`), http.StatusServiceUnavailable, "ServiceUnavailable")

	require.NoError(t, app.Start(context.Background()))
	w := postCloudEvent(app, "early.event", `{}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	require.NoError(t, app.Shutdown(context.Background()))
	requireRetryable(t, postCloudEvent(app, "late.event", `{}`), http.StatusServiceUnavailable, "ServiceUnavailable")
}

// blockingWakeStorage holds the wake of the subscribers waiting on channel
// until release is closed or its context is cancelled, and reports the
// error of the query it then runs.
type blockingWakeStorage struct {
	storage.Storage
	channel string
	entered chan struct{}
	release chan struct{}
	result  chan error
}

func newBlockingWakeStorage(channel string) *blockingWakeStorage {
	return &blockingWakeStorage{
		channel: channel,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		result:  make(chan error, 1),
	}
}

func (s *blockingWakeStorage) GetChannelSubscribersWaiting(ctx context.Context, channelName string) ([]*storage.ChannelSubscription, error) {
	if channelName != s.channel {
		return s.Storage.GetChannelSubscribersWaiting(ctx, channelName)
	}
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		s.result <- ctx.Err()
		return nil, ctx.Err()
	}
	subs, err := s.Storage.GetChannelSubscribersWaiting(ctx, channelName)
	s.result <- err
	return subs, err
}

// within receives from ch, failing the test if nothing comes in time.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// Shutdown waits for the events it accepted before: the wake of their
// waiting subscribers still runs against open storage.
func TestShutdown_WaitsForAcceptedEvents(t *testing.T) {
	blocking := newBlockingWakeStorage("slow.event")
	app := acceptingApp(t, func(s storage.Storage) storage.Storage {
		blocking.Storage = s
		return blocking
	})

	w := postCloudEvent(app, "slow.event", `{}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	within(t, blocking.entered, "the wake of the waiting subscribers")

	done := make(chan error, 1)
	go func() { done <- app.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned while an accepted event was being processed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	requireRetryable(t, postCloudEvent(app, "slow.event", `{}`), http.StatusServiceUnavailable, "ServiceUnavailable")

	close(blocking.release)
	require.NoError(t, within(t, done, "Shutdown after the event was processed"))
	require.NoError(t, within(t, blocking.result, "the result of the wake"), "the wake ran against closed storage")
}

// A wake that is still running at the shutdown deadline does not hold
// Shutdown: it is cancelled with the background tasks.
func TestShutdown_CancelsEventWorkAtDeadline(t *testing.T) {
	blocking := newBlockingWakeStorage("stuck.event")
	app := acceptingApp(t, func(s storage.Storage) storage.Storage {
		blocking.Storage = s
		return blocking
	})
	app.config.shutdownTimeout = 100 * time.Millisecond

	w := postCloudEvent(app, "stuck.event", `{}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	within(t, blocking.entered, "the wake of the waiting subscribers")

	done := make(chan error, 1)
	go func() { done <- app.Shutdown(context.Background()) }()
	within(t, done, "Shutdown at its deadline")
	require.ErrorIs(t, within(t, blocking.result, "the cancellation of the wake"), context.Canceled)
}

// Shutdown cancels the first run of a workflow that an event started, as it
// cancels the other runs, and waits for it before it closes storage. The
// instance stays running, for a later run.
func TestShutdown_CancelsFirstRuns(t *testing.T) {
	app, cleanup := createTestApp(t)
	defer cleanup()
	entered := make(chan struct{})
	var enter sync.Once
	stopped := make(chan error, 1)
	block := DefineActivity("block_until_cancelled", func(ctx context.Context, in acceptedOrder) (string, error) {
		enter.Do(func() { close(entered) })
		<-ctx.Done()
		select {
		case stopped <- ctx.Err():
		default:
		}
		return "", ctx.Err()
	}, WithTransactional[acceptedOrder, string](false))
	wf := DefineWorkflow("order.blocking", func(ctx *WorkflowContext, in acceptedOrder) (string, error) {
		return block.Execute(ctx, in)
	})
	RegisterWorkflow(app, wf, WithEventHandler(true))
	require.NoError(t, app.Start(context.Background()))

	w := postCloudEvent(app, "order.blocking", `{"order_id":"o-1"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	within(t, entered, "the first run")

	require.NoError(t, app.Shutdown(context.Background()))
	require.ErrorIs(t, within(t, stopped, "the cancellation of the first run"), context.Canceled)

	requireRunningAndUnlocked(t, app, "order.blocking")
}

// requireRunningAndUnlocked checks, after Shutdown, that the instance of a
// workflow whose run was cancelled is left running for a later run, and that
// the cancelled run handed its lock back instead of keeping it for the rest
// of its lease.
func requireRunningAndUnlocked(t *testing.T, app *App, workflowName string) {
	t.Helper()
	store, err := storage.NewSQLiteStorage(app.config.databaseURL)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	var status string
	var lockedBy *string
	require.NoError(t, store.DB().QueryRowContext(context.Background(),
		`SELECT status, locked_by FROM workflow_instances WHERE workflow_name = ?`, workflowName).Scan(&status, &lockedBy))
	require.Equal(t, "running", status)
	require.Nil(t, lockedBy, "the cancelled run kept its lock")
}

// A run that resumed a waiting instance is cancelled by Shutdown the same
// way, and hands its lock back too.
func TestShutdown_ReleasesTheLockOfACancelledResumedRun(t *testing.T) {
	app, cleanup := createTestApp(t, WithWorkflowResumptionInterval(20*time.Millisecond))
	defer cleanup()
	entered := make(chan struct{})
	var enter sync.Once
	block := DefineActivity("block_after_resume", func(ctx context.Context, in acceptedOrder) (string, error) {
		enter.Do(func() { close(entered) })
		<-ctx.Done()
		return "", ctx.Err()
	}, WithTransactional[acceptedOrder, string](false))
	wf := DefineWorkflow("order.resumed_blocking", func(ctx *WorkflowContext, in acceptedOrder) (string, error) {
		if _, err := WaitEvent[map[string]any](ctx, "order.go_on"); err != nil {
			return "", err
		}
		return block.Execute(ctx, in)
	})
	RegisterWorkflow(app, wf)
	require.NoError(t, app.Start(context.Background()))

	id, err := StartWorkflow(context.Background(), app, wf, acceptedOrder{OrderID: "o-1"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		inst, err := app.storage.GetInstance(context.Background(), id)
		return err == nil && inst != nil && inst.Status == storage.StatusWaitingForMessage
	}, 5*time.Second, 10*time.Millisecond)

	w := postCloudEvent(app, "order.go_on", `{}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	within(t, entered, "the resumed run")

	require.NoError(t, app.Shutdown(context.Background()))
	requireRunningAndUnlocked(t, app, "order.resumed_blocking")
}
