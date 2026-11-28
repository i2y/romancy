package romancy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// waitForAnyEvent waits for one event of the type it is given and returns it
// whole, with its data as JSON.
var waitForAnyEvent = DefineWorkflow("wait_for_any_event",
	func(ctx *WorkflowContext, eventType string) (ReceivedEvent[json.RawMessage], error) {
		ev, err := WaitEvent[json.RawMessage](ctx, eventType)
		if err != nil {
			return ReceivedEvent[json.RawMessage]{}, err
		}
		return *ev, nil
	})

func startAnyEventWaiter(t *testing.T, app *App, eventType string) string {
	t.Helper()
	id, err := StartWorkflow(context.Background(), app, waitForAnyEvent, eventType)
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)
	return id
}

func eventReceivedBy(t *testing.T, app *App, instanceID string) ReceivedEvent[json.RawMessage] {
	t.Helper()
	requireStatus(t, app, instanceID, storage.StatusCompleted)
	res, err := GetWorkflowResult[ReceivedEvent[json.RawMessage]](context.Background(), app, instanceID)
	require.NoError(t, err)
	return res.Output
}

// storeMessage stores a channel message as another runtime (or an earlier
// version) did, and delivers it to the workflows waiting on the channel.
func storeMessage(t *testing.T, app *App, channel, data, metadata string) {
	t.Helper()
	var metadataJSON []byte
	if metadata != "" {
		metadataJSON = []byte(metadata)
	}
	id, err := app.storage.PublishToChannel(context.Background(), channel, []byte(data), metadataJSON)
	require.NoError(t, err)
	app.wakeWaitingChannelSubscribers(context.Background(), channel, id, []byte(data), metadataJSON)
}

func storedMessages(t *testing.T, app *App, channel string) []*storage.ChannelMessage {
	t.Helper()
	messages, err := app.storage.ChannelMessagesAfter(context.Background(), channel, 0)
	require.NoError(t, err)
	return messages
}

// The endpoint stores an event as its data, with its attributes under the
// ce_ keys of the metadata (SEMANTICS §3c). The capability token is not kept.
func TestCloudEventFormat_StoredAsDataAndAttributes(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) {})

	w := postStructuredEvent(app, map[string]any{
		"specversion": "1.0", "id": "evt-1", "source": "shop", "type": "order.stored",
		"time": "2026-11-28T10:00:00.5+09:00", "subject": "order-1",
		"dataschema": "https://example.com/order.json", "datacontenttype": "application/json",
		"region": "eu", "capabilitytoken": "not-kept",
		"data": map[string]any{"n": json.Number("12345678901234567890")},
	})
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	messages := storedMessages(t, app, "order.stored")
	require.Len(t, messages, 1)
	var data bytes.Buffer
	require.NoError(t, json.Compact(&data, messages[0].Data))
	require.Equal(t, `{"n":12345678901234567890}`, data.String(), "the data keeps its digits")
	require.JSONEq(t, `{
		"ce_specversion": "1.0", "ce_id": "evt-1", "ce_source": "shop", "ce_type": "order.stored",
		"ce_time": "2026-11-28T10:00:00.5+09:00", "ce_subject": "order-1",
		"ce_dataschema": "https://example.com/order.json", "ce_datacontenttype": "application/json",
		"ce_extensions": {"region": "eu"}
	}`, string(messages[0].Metadata))

	// An event without data, and with only the required attributes.
	w = postStructuredEvent(app, map[string]any{
		"specversion": "1.0", "id": "evt-2", "source": "shop", "type": "order.bare",
	})
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	messages = storedMessages(t, app, "order.bare")
	require.Len(t, messages, 1)
	require.JSONEq(t, `null`, string(messages[0].Data))
	require.JSONEq(t, `{"ce_specversion": "1.0", "ce_id": "evt-2", "ce_source": "shop", "ce_type": "order.bare"}`,
		string(messages[0].Metadata))
}

// WaitEvent reads an event that the endpoint of another runtime stored: its
// data, of any JSON type, and its attributes.
func TestCloudEventFormat_WaitEventReadsAnotherRuntimesEvent(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, waitForAnyEvent) })
	id := startAnyEventWaiter(t, app, "order.shipped")

	// As Edda stores it, with a key of its own beside the attributes.
	storeMessage(t, app, "order.shipped", `[1, 2, 3]`, `{
		"ce_specversion": "1.0", "ce_id": "py-1", "ce_source": "warehouse", "ce_type": "order.shipped",
		"ce_time": "2026-11-28T01:02:03.456789+00:00", "ce_subject": "order-9",
		"ce_datacontenttype": "application/json", "ce_dataschema": "https://example.com/s.json",
		"ce_extensions": {"region": "eu"}, "published_at": "2026-11-28T01:02:04+00:00"
	}`)

	ev := eventReceivedBy(t, app, id)
	require.JSONEq(t, `[1, 2, 3]`, string(ev.Data))
	require.Equal(t, "py-1", ev.ID)
	require.Equal(t, "order.shipped", ev.Type)
	require.Equal(t, "warehouse", ev.Source)
	require.Equal(t, "1.0", ev.SpecVersion)
	require.Equal(t, "order-9", ev.Subject)
	require.Equal(t, "application/json", ev.DataContentType)
	require.Equal(t, "https://example.com/s.json", ev.DataSchema)
	require.Equal(t, map[string]any{"region": "eu"}, ev.Extensions)
	require.NotNil(t, ev.Time)
	require.True(t, ev.Time.Equal(time.Date(2026, 11, 28, 1, 2, 3, 456789000, time.UTC)), ev.Time)
}

// A message that an earlier version of the endpoint stored (the attributes
// and the data together in the message data) reads as it did.
func TestCloudEventFormat_WaitEventReadsTheEarlierForm(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, waitForAnyEvent) })
	id := startAnyEventWaiter(t, app, "order.legacy")

	storeMessage(t, app, "order.legacy", `{
		"id": "old-1", "type": "order.legacy", "source": "legacy", "specversion": "1.0",
		"time": "2026-11-28T00:00:00Z", "subject": "s", "datacontenttype": "application/json",
		"data": {"n": 4}, "extensions": {"region": "eu"}
	}`, "")

	ev := eventReceivedBy(t, app, id)
	require.JSONEq(t, `{"n": 4}`, string(ev.Data))
	require.Equal(t, "old-1", ev.ID)
	require.Equal(t, "order.legacy", ev.Type)
	require.Equal(t, "legacy", ev.Source)
	require.Equal(t, "1.0", ev.SpecVersion)
	require.Equal(t, "s", ev.Subject)
	require.Equal(t, map[string]any{"region": "eu"}, ev.Extensions)
	require.True(t, ev.Time.Equal(time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)), ev.Time)
}

// WaitEvent on a message that workflow code published returns its data
// (before, the data was its zero value). The message has no attributes.
func TestCloudEventFormat_WaitEventGetsPublishedData(t *testing.T) {
	publish := DefineActivity("publish_for_wait_event", func(ctx context.Context, n int) (struct{}, error) {
		return struct{}{}, Publish(GetWorkflowContext(ctx), "order.published", paymentDone{N: n})
	})
	sender := DefineWorkflow("publish_for_wait_event", func(ctx *WorkflowContext, n int) (struct{}, error) {
		return publish.Execute(ctx, n)
	})
	app := startTargetedTestApp(t, func(app *App) {
		RegisterWorkflow(app, waitForAnyEvent)
		RegisterWorkflow(app, sender)
	})
	id := startAnyEventWaiter(t, app, "order.published")
	_, err := StartWorkflow(context.Background(), app, sender, 5)
	require.NoError(t, err)

	ev := eventReceivedBy(t, app, id)
	require.JSONEq(t, `{"n": 5}`, string(ev.Data))
	require.Equal(t, "order.published", ev.Type)
	require.Empty(t, ev.ID)
	require.Empty(t, ev.Source)
	require.Nil(t, ev.Extensions)
}

// A Receive of an event's channel returns the event's data, and the
// attributes in the metadata.
func TestCloudEventFormat_ReceiveGetsTheEventData(t *testing.T) {
	type received struct {
		N  int    `json:"n"`
		ID string `json:"id"`
	}
	wf := DefineWorkflow("receive_an_event", func(ctx *WorkflowContext, _ struct{}) (received, error) {
		if err := Subscribe(ctx, "order.received", ModeBroadcast); err != nil {
			return received{}, err
		}
		msg, err := Receive[paymentDone](ctx, "order.received")
		if err != nil {
			return received{}, err
		}
		id, _ := msg.Metadata["ce_id"].(string)
		return received{N: msg.Data.N, ID: id}, nil
	})
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, wf) })
	id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)

	w := postStructuredEvent(app, map[string]any{
		"specversion": "1.0", "id": "evt-9", "source": "shop", "type": "order.received",
		"data": map[string]any{"n": 9},
	})
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	requireStatus(t, app, id, storage.StatusCompleted)
	res, err := GetWorkflowResult[received](context.Background(), app, id)
	require.NoError(t, err)
	require.Equal(t, received{N: 9, ID: "evt-9"}, res.Output)
}
