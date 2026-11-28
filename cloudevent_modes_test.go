package romancy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// The CloudEvents endpoint reads both content modes of the HTTP binding
// (SEMANTICS §3c): structured (the body is the event) and binary (the
// attributes are ce-* headers, the body is the data).

// postBinaryEvent posts a binary-mode CloudEvent of the given type: its
// attributes in ce-* headers (extra adds header name/value pairs), its data
// in the body.
func postBinaryEvent(app *App, eventType, contentType, body string, extra ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("ce-specversion", "1.0")
	req.Header.Set("ce-id", "evt-"+eventType)
	req.Header.Set("ce-source", "test")
	req.Header.Set("ce-type", eventType)
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	return w
}

// postStructuredEvent posts a structured-mode CloudEvent.
func postStructuredEvent(app *App, event map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	return w
}

// requireRefused checks a 400 answer: not retryable, of the given type, and
// naming what is wrong.
func requireRefused(t *testing.T, w *httptest.ResponseRecorder, errType, mention string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.Equal(t, errType, body["error_type"], body["error"])
	require.Equal(t, "false", body["retryable"])
	require.Contains(t, body["error"], mention)
}

// receivedAttributes is what a test workflow reports of the event it received.
type receivedAttributes struct {
	ID          string         `json:"id"`
	Source      string         `json:"source"`
	Subject     string         `json:"subject"`
	ContentType string         `json:"content_type"`
	Extensions  map[string]any `json:"extensions"`
	N           int            `json:"n"`
}

// waitForEventWorkflow waits for one event of the type it is given.
var waitForEventWorkflow = DefineWorkflow("wait_for_event_of_type",
	func(ctx *WorkflowContext, eventType string) (receivedAttributes, error) {
		ev, err := WaitEvent[paymentDone](ctx, eventType)
		if err != nil {
			return receivedAttributes{}, err
		}
		return receivedAttributes{
			ID: ev.ID, Source: ev.Source, Subject: ev.Subject, ContentType: ev.DataContentType,
			Extensions: ev.Extensions, N: ev.Data.N,
		}, nil
	})

func startWaiter(t *testing.T, app *App, eventType string) string {
	t.Helper()
	id, err := StartWorkflow(context.Background(), app, waitForEventWorkflow, eventType)
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)
	return id
}

func receivedBy(t *testing.T, app *App, instanceID string) receivedAttributes {
	t.Helper()
	requireStatus(t, app, instanceID, storage.StatusCompleted)
	res, err := GetWorkflowResult[receivedAttributes](context.Background(), app, instanceID)
	require.NoError(t, err)
	return res.Output
}

// A binary-mode event reaches WaitEvent with its attributes and its data.
func TestCloudEvents_BinaryModeReachesWaitEvent(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, waitForEventWorkflow) })
	id := startWaiter(t, app, "payment.binary")

	w := postBinaryEvent(app, "payment.binary", "application/json", `{"n":7}`,
		"ce-subject", "order-1", "ce-region", "eu")
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, receivedAttributes{
		ID: "evt-payment.binary", Source: "test", Subject: "order-1", ContentType: "application/json",
		Extensions: map[string]any{"region": "eu"}, N: 7,
	}, receivedBy(t, app, id))
}

// In binary mode the romancyinstanceid and capabilitytoken extensions are
// ce-* headers: the event reaches its target only, and a sealed channel takes
// it only with the token, which is not stored.
func TestCloudEvents_BinaryModeTargetAndToken(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, waitForEventWorkflow) })
	target := startWaiter(t, app, "payment.sealed")
	other := startWaiter(t, app, "payment.sealed")
	token := grantChannelCap(t, app, storage.CapabilityKindChannelPub, "payment.sealed")

	w := postBinaryEvent(app, "payment.sealed", "application/json", `{"n":1}`, "ce-romancyinstanceid", target)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	w = postBinaryEvent(app, "payment.sealed", "application/json", `{"n":2}`,
		"ce-romancyinstanceid", target, "ce-capabilitytoken", token)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	got := receivedBy(t, app, target)
	require.Equal(t, 2, got.N)
	require.Equal(t, map[string]any{"romancyinstanceid": target}, got.Extensions, "the token was stored with the event")

	time.Sleep(200 * time.Millisecond)
	inst, err := app.GetInstance(context.Background(), other)
	require.NoError(t, err)
	require.Equal(t, storage.StatusWaitingForMessage, inst.Status, "another waiter of the type received the targeted event")
}

// The CloudEvents SDK client, which the outbox relayer uses, sends binary
// mode by default; both of its modes reach WaitEvent.
func TestCloudEvents_SDKClientBothModes(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, waitForEventWorkflow) })
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	client, err := cloudevents.NewClientHTTP(cloudevents.WithTarget(server.URL))
	require.NoError(t, err)

	for _, tc := range []struct {
		mode  string
		force func(context.Context) context.Context
		n     int
	}{
		{"binary", binding.WithForceBinary, 1},
		{"structured", binding.WithForceStructured, 2},
	} {
		eventType := "payment.sdk." + tc.mode
		id := startWaiter(t, app, eventType)
		e := cloudevents.NewEvent()
		e.SetID("sdk-" + tc.mode)
		e.SetSource("test")
		e.SetType(eventType)
		require.NoError(t, e.SetData(cloudevents.ApplicationJSON, paymentDone{N: tc.n}))
		result := client.Send(tc.force(context.Background()), e)
		require.True(t, cloudevents.IsACK(result), "%s mode: %v", tc.mode, result)
		got := receivedBy(t, app, id)
		require.Equal(t, tc.n, got.N, tc.mode)
		require.Equal(t, "application/json", got.ContentType, tc.mode)
	}
}

// An event that a workflow sends through the outbox reaches the WaitEvent of
// a workflow of an app whose endpoint the relayer posts to.
func TestCloudEvents_OutboxRelayerToEndpoint(t *testing.T) {
	var endpoint atomic.Pointer[http.Handler]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*endpoint.Load()).ServeHTTP(w, r)
	}))
	defer server.Close()

	send := DefineActivity("send_relayed_payment", func(ctx context.Context, n int) (struct{}, error) {
		return struct{}{}, SendEvent(GetWorkflowContext(ctx), "payment.relayed", "test", paymentDone{N: n})
	})
	sender := DefineWorkflow("relayed_payment_sender", func(ctx *WorkflowContext, n int) (struct{}, error) {
		return send.Execute(ctx, n)
	})
	app := startTargetedTestApp(t, func(app *App) {
		RegisterWorkflow(app, waitForEventWorkflow)
		RegisterWorkflow(app, sender)
		h := app.Handler()
		endpoint.Store(&h)
	}, WithOutbox(true), WithBrokerURL(server.URL), WithOutboxInterval(20*time.Millisecond))

	id := startWaiter(t, app, "payment.relayed")
	_, err := StartWorkflow(context.Background(), app, sender, 9)
	require.NoError(t, err)
	got := receivedBy(t, app, id)
	require.Equal(t, 9, got.N)
	require.Equal(t, "application/json", got.ContentType)
}

// The data of an event is what its datacontenttype says: text is a string,
// other data its bytes, and JSON data in data_base64 decodes like data.
func TestCloudEvents_DataByContentType(t *testing.T) {
	text := DefineWorkflow("text_event_wait", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		ev, err := WaitEvent[string](ctx, "note.text")
		if err != nil {
			return "", err
		}
		return ev.DataContentType + " " + ev.Data, nil
	})
	blob := DefineWorkflow("bytes_event_wait", func(ctx *WorkflowContext, _ struct{}) ([]byte, error) {
		ev, err := WaitEvent[[]byte](ctx, "note.bytes")
		if err != nil {
			return nil, err
		}
		return ev.Data, nil
	})
	app := startTargetedTestApp(t, func(app *App) {
		RegisterWorkflow(app, text)
		RegisterWorkflow(app, blob)
		RegisterWorkflow(app, waitForEventWorkflow)
	})
	ctx := context.Background()
	textID, err := StartWorkflow(ctx, app, text, struct{}{})
	require.NoError(t, err)
	blobID, err := StartWorkflow(ctx, app, blob, struct{}{})
	require.NoError(t, err)
	encodedID := startWaiter(t, app, "payment.encoded")
	requireStatus(t, app, textID, storage.StatusWaitingForMessage)
	requireStatus(t, app, blobID, storage.StatusWaitingForMessage)

	require.Equal(t, http.StatusAccepted, postBinaryEvent(app, "note.text", "text/plain; charset=utf-8", "hello").Code)
	raw := []byte{0, 1, 2, 0xff}
	require.Equal(t, http.StatusAccepted, postBinaryEvent(app, "note.bytes", "application/octet-stream", string(raw)).Code)
	require.Equal(t, http.StatusAccepted, postStructuredEvent(app, map[string]any{
		"specversion": "1.0", "id": "b64", "source": "test", "type": "payment.encoded",
		"datacontenttype": "application/json", "data_base64": base64.StdEncoding.EncodeToString([]byte(`{"n":3}`)),
	}).Code)

	requireStatus(t, app, textID, storage.StatusCompleted)
	gotText, err := GetWorkflowResult[string](ctx, app, textID)
	require.NoError(t, err)
	require.Equal(t, "text/plain; charset=utf-8 hello", gotText.Output)
	requireStatus(t, app, blobID, storage.StatusCompleted)
	gotBytes, err := GetWorkflowResult[[]byte](ctx, app, blobID)
	require.NoError(t, err)
	require.Equal(t, raw, gotBytes.Output)
	require.Equal(t, 3, receivedBy(t, app, encodedID).N)

	// Data that is not what its datacontenttype says is refused.
	requireRefused(t, postBinaryEvent(app, "note.bad", "application/json", "not json"), "ParseError", "not JSON")
	require.Zero(t, countChannelMessages(t, app, "note.bad"))
}

// An event without one of the required attributes specversion, id, source
// and type is refused with 400, and nothing is stored.
func TestCloudEvents_RequiredAttributes(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) {})
	full := map[string]any{"specversion": "1.0", "id": "e1", "source": "test", "type": "x.required"}
	for _, missing := range []string{"specversion", "id", "source", "type"} {
		event := map[string]any{}
		for name, value := range full {
			if name != missing {
				event[name] = value
			}
		}
		requireRefused(t, postStructuredEvent(app, event), "ValidationError", missing)
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"n":1}`))
	for name, value := range map[string]string{"ce-specversion": "1.0", "ce-source": "test", "ce-type": "x.required"} {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	requireRefused(t, w, "ValidationError", "id")

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`[]`))
	req.Header.Set("Content-Type", "application/cloudevents-batch+json")
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	requireRefused(t, w, "ParseError", "batched")

	require.Zero(t, countChannelMessages(t, app, "x.required"))
}

// A targeted event is for its target only: it starts no workflow of its
// type, in either content mode. An event for every waiter starts one.
func TestTargetedEvent_StartsNoHandler(t *testing.T) {
	handler := DefineWorkflow("order.placed", func(ctx *WorkflowContext, in acceptedOrder) (string, error) {
		return in.OrderID, nil
	})
	app := startTargetedTestApp(t, func(app *App) {
		RegisterWorkflow(app, handler, WithEventHandler(true))
		RegisterWorkflow(app, waitForEventWorkflow)
	})
	handlerInstances := func() int {
		var n int
		require.NoError(t, app.Storage().DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM workflow_instances WHERE workflow_name = ?`, "order.placed").Scan(&n))
		return n
	}

	structured := startWaiter(t, app, "order.placed")
	w := postStructuredEvent(app, map[string]any{
		"specversion": "1.0", "id": "o-1", "source": "test", "type": "order.placed",
		"romancyinstanceid": structured, "data": map[string]any{"n": 1},
	})
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, receivedBy(t, app, structured).N)

	binary := startWaiter(t, app, "order.placed")
	w = postBinaryEvent(app, "order.placed", "application/json", `{"n":2}`, "ce-romancyinstanceid", binary)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 2, receivedBy(t, app, binary).N)
	require.Zero(t, handlerInstances(), "a targeted event started the workflow of its type")

	w = postCloudEvent(app, "order.placed", `{"order_id":"o-3"}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, handlerInstances())
}
