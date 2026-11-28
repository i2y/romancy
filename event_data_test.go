package romancy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// A channel message's data is JSON or bytes, in a binary message (SEMANTICS
// §3). The CloudEvents endpoint stores an event's data by its
// datacontenttype, whichever content mode the event came in (SEMANTICS §3c).

// storedData is how a test reports a stored message's data.
func storedData(t *testing.T, app *App, channel string) string {
	t.Helper()
	messages := storedMessages(t, app, channel)
	require.Len(t, messages, 1, "messages on %s", channel)
	if messages[0].IsBinary() {
		require.Empty(t, messages[0].Data)
		return fmt.Sprintf("bytes %x", messages[0].DataBinary)
	}
	require.Empty(t, messages[0].DataBinary)
	return "json " + string(messages[0].Data)
}

func TestEventData_StoredByContentType(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) {})
	structured := func(channel string, members map[string]any) {
		event := map[string]any{"specversion": "1.0", "id": "e-" + channel, "source": "test", "type": channel}
		for name, value := range members {
			event[name] = value
		}
		w := postStructuredEvent(app, event)
		require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	}
	binary := func(channel, contentType, body string) {
		w := postBinaryEvent(app, channel, contentType, body)
		require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	}

	binary("b.octets", "application/octet-stream", "\xff\x00")
	binary("b.text", "text/plain", "42")
	binary("b.text.latin1", "text/plain; charset=iso-8859-1", "caf\xe9")
	binary("b.untyped.text", "", "hello")
	binary("b.untyped.json", "", `{"a": 1}`)
	binary("b.vendor.json", "application/vnd.example+json", `{"a": 1}`)
	binary("b.empty", "application/octet-stream", "")
	structured("s.vendor.json", map[string]any{"datacontenttype": "application/vnd.example+json", "data": map[string]any{"a": 1}})
	structured("s.json.string", map[string]any{"datacontenttype": "application/json", "data": `{"a": 1}`})
	structured("s.xml", map[string]any{"datacontenttype": "application/xml", "data": "<a/>"})
	structured("s.text", map[string]any{"datacontenttype": "text/plain", "data": "42"})
	structured("s.base64", map[string]any{"data_base64": base64.StdEncoding.EncodeToString([]byte{0xff, 0})})
	structured("s.base64.text", map[string]any{"datacontenttype": "text/plain", "data_base64": base64.StdEncoding.EncodeToString([]byte("hi"))})

	for channel, want := range map[string]string{
		"b.octets":       "bytes ff00",
		"b.text":         `json "42"`,
		"b.text.latin1":  "bytes 636166e9",
		"b.untyped.text": "bytes 68656c6c6f",
		"b.untyped.json": `json {"a": 1}`,
		"b.vendor.json":  `json {"a": 1}`,
		"b.empty":        "json null",
		"s.vendor.json":  `json {"a":1}`,
		"s.json.string":  `json "{\"a\": 1}"`,
		"s.xml":          "bytes 3c612f3e",
		"s.text":         `json "42"`,
		"s.base64":       "bytes ff00",
		"s.base64.text":  `json "hi"`,
	} {
		require.Equal(t, want, storedData(t, app, channel), channel)
	}

	// The data member of a type that is not JSON is the text of the data.
	requireRefused(t, postStructuredEvent(app, map[string]any{
		"specversion": "1.0", "id": "e-num", "source": "test", "type": "s.text.number",
		"datacontenttype": "text/plain", "data": 42,
	}), "ParseError", "expected a string")
	require.Zero(t, countChannelMessages(t, app, "s.text.number"))
}

// bytesReport is how a test workflow reports data that it received as T.
func bytesReport(data any) string {
	switch v := data.(type) {
	case json.RawMessage:
		return "json.RawMessage " + string(v)
	case []byte, string:
		return fmt.Sprintf("%T %x", v, v)
	default:
		encoded, _ := json.Marshal(v)
		return fmt.Sprintf("%T %s", v, encoded)
	}
}

// WaitEvent receives bytes as a []byte, a string or an any (holding a
// []byte); a type that cannot hold bytes gets an error.
func TestEventData_WaitEventReceivesBytes(t *testing.T) {
	waiter := func(name string, receive func(ctx *WorkflowContext) (string, error)) *WorkflowFunc[struct{}, string] {
		return DefineWorkflow(name, func(ctx *WorkflowContext, _ struct{}) (string, error) { return receive(ctx) })
	}
	asBytes := waiter("bytes_as_bytes", func(ctx *WorkflowContext) (string, error) {
		ev, err := WaitEvent[[]byte](ctx, "blob.sent")
		if err != nil {
			return "", err
		}
		return bytesReport(ev.Data) + " " + ev.DataContentType, nil
	})
	asString := waiter("bytes_as_string", func(ctx *WorkflowContext) (string, error) {
		ev, err := WaitEvent[string](ctx, "blob.sent")
		if err != nil {
			return "", err
		}
		return bytesReport(ev.Data), nil
	})
	asAny := waiter("bytes_as_any", func(ctx *WorkflowContext) (string, error) {
		ev, err := WaitEvent[any](ctx, "blob.sent")
		if err != nil {
			return "", err
		}
		return bytesReport(ev.Data), nil
	})
	asRaw := waiter("bytes_as_raw", func(ctx *WorkflowContext) (string, error) {
		ev, err := WaitEvent[json.RawMessage](ctx, "blob.sent")
		if err != nil {
			return "", err
		}
		return bytesReport(ev.Data), nil
	})
	asStruct := waiter("bytes_as_struct", func(ctx *WorkflowContext) (string, error) {
		_, err := WaitEvent[paymentDone](ctx, "blob.sent")
		if err != nil && !IsSuspendSignal(err) {
			return "error: " + err.Error(), nil
		}
		return "no error", err
	})
	workflows := []*WorkflowFunc[struct{}, string]{asBytes, asString, asAny, asRaw, asStruct}
	app := startTargetedTestApp(t, func(app *App) {
		for _, wf := range workflows {
			RegisterWorkflow(app, wf)
		}
	})
	ids := make([]string, len(workflows))
	for i, wf := range workflows {
		id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
		require.NoError(t, err)
		requireStatus(t, app, id, storage.StatusWaitingForMessage)
		ids[i] = id
	}

	w := postBinaryEvent(app, "blob.sent", "application/octet-stream", "\xff\x00a")
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	want := []string{
		"[]uint8 ff0061 application/octet-stream",
		"string ff0061",
		"[]uint8 ff0061",
		`json.RawMessage "/wBh"`,
		"error: failed to deserialize event data: the data is bytes, which a romancy.paymentDone cannot hold: receive it as []byte, string or any",
	}
	for i, id := range ids {
		requireStatus(t, app, id, storage.StatusCompleted)
		res, err := GetWorkflowResult[string](context.Background(), app, id)
		require.NoError(t, err)
		require.Equal(t, want[i], res.Output, workflows[i].Name())
	}
}

// receiveTwice receives two messages of a channel as any and reports both
// once the second arrives: the run that reports them replays the first
// receive from its record.
var receiveTwice = DefineWorkflow("receive_bytes_twice", func(ctx *WorkflowContext, channel string) (string, error) {
	if err := Subscribe(ctx, channel, ModeBroadcast); err != nil {
		return "", err
	}
	first, err := Receive[any](ctx, channel)
	if err != nil {
		return "", err
	}
	second, err := Receive[any](ctx, channel)
	if err != nil {
		return "", err
	}
	return bytesReport(first.Data) + " | " + bytesReport(second.Data), nil
})

// requeue returns the waiting instances with a pending message to running,
// as the leader's sweep does: the instance takes the message itself.
func requeue(t *testing.T, app *App) {
	t.Helper()
	_, err := app.storage.RequeueChannelWaitsWithPending(context.Background())
	require.NoError(t, err)
}

// Bytes that another runtime published (a binary message) reach Receive,
// both from the channel and in a delivery, and the replay of their records
// gives the same bytes.
func TestEventData_ReceiveBytes(t *testing.T) {
	app := startTargetedTestApp(t, func(app *App) { RegisterWorkflow(app, receiveTwice) })
	ctx := context.Background()
	id, err := StartWorkflow(ctx, app, receiveTwice, "blobs")
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)

	// Taken from the channel by the instance, as when it was not waiting.
	_, err = app.storage.PublishBinaryToChannel(ctx, "blobs", []byte{0xff, 0}, nil)
	require.NoError(t, err)
	requeue(t, app)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)

	// Delivered by the publisher to the waiting instance.
	message, err := messagePayload{data: []byte{1, 2}, binary: true}.publish(ctx, app.storage, "blobs", nil)
	require.NoError(t, err)
	app.wakeWaitingChannelSubscribers(ctx, message)

	requireStatus(t, app, id, storage.StatusCompleted)
	res, err := GetWorkflowResult[string](ctx, app, id)
	require.NoError(t, err)
	require.Equal(t, "[]uint8 ff00 | []uint8 0102", res.Output)

	// Both receive records hold the bytes in base64 and say so.
	history, _, err := app.storage.GetHistoryPaginated(ctx, id, 0, 100)
	require.NoError(t, err)
	var records []map[string]any
	for _, event := range history {
		var record map[string]any
		if json.Unmarshal(event.EventData, &record) == nil && record["data_type"] != nil {
			records = append(records, record)
		}
	}
	require.Len(t, records, 2)
	require.Equal(t, "binary", records[0]["data_type"])
	require.Equal(t, "/wA=", records[0]["data"])
	require.Equal(t, "binary", records[1]["data_type"])
	require.Equal(t, "AQI=", records[1]["data"])
}

// deliverAsAnotherRuntime records a receive in a waiting instance's history
// as another runtime's delivery does, in that runtime's record, and returns
// the instance to running.
func deliverAsAnotherRuntime(t *testing.T, app *App, instanceID, channel, record string) {
	t.Helper()
	ctx := context.Background()
	sub, err := app.storage.GetChannelSubscription(ctx, instanceID, channel)
	require.NoError(t, err)
	require.NotNil(t, sub)
	require.NotEmpty(t, sub.ActivityID, "the instance does not wait on %s", channel)
	lockID := storage.NewLockIdentity("another-runtime")
	acquired, err := app.storage.TryAcquireLock(ctx, instanceID, lockID, 60)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, app.storage.AppendHistory(ctx, &storage.HistoryEvent{
		InstanceID: instanceID, ActivityID: sub.ActivityID, EventType: storage.HistoryChannelMessageReceived,
		DataType: storage.DataTypeJSON, EventData: []byte(record),
	}))
	require.NoError(t, app.storage.ClearChannelWaitingState(ctx, instanceID, channel))
	require.NoError(t, app.storage.UpdateInstanceStatusIfLocked(ctx, instanceID, storage.StatusRunning, "", lockID))
	require.NoError(t, app.storage.ReleaseLock(ctx, instanceID, lockID))
}

// The records of bytes that Edda and Shikibu write replay as bytes.
func TestEventData_ReplaysAnotherRuntimesRecordOfBytes(t *testing.T) {
	asString := DefineWorkflow("replay_bytes_as_string", func(ctx *WorkflowContext, channel string) (string, error) {
		if err := Subscribe(ctx, channel, ModeBroadcast); err != nil {
			return "", err
		}
		msg, err := Receive[string](ctx, channel)
		if err != nil {
			return "", err
		}
		return msg.Data + " on " + msg.ChannelName, nil
	})
	app := startTargetedTestApp(t, func(app *App) {
		RegisterWorkflow(app, receiveTwice)
		RegisterWorkflow(app, asString)
	})
	ctx := context.Background()

	edda := `{"id": "0b9d6f3e-1", "channel": "from.edda", "data": "/wA=", "data_type": "binary", "metadata": {}}`
	id, err := StartWorkflow(ctx, app, receiveTwice, "from.edda")
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)
	deliverAsAnotherRuntime(t, app, id, "from.edda", edda)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)
	deliverAsAnotherRuntime(t, app, id, "from.edda",
		`{"id": "0b9d6f3e-2", "channel": "from.edda", "data": [1], "data_type": "json", "metadata": {}}`)
	requireStatus(t, app, id, storage.StatusCompleted)
	res, err := GetWorkflowResult[string](ctx, app, id)
	require.NoError(t, err)
	require.Equal(t, "[]uint8 ff00 | []interface {} [1]", res.Output)

	shikibu := `{"channel": "from.shikibu", "message_id": "m-1", "data": "aGk=", "data_type": "binary",
		"metadata": null, "published_at": "2026-11-28T01:02:03Z"}`
	id, err = StartWorkflow(ctx, app, asString, "from.shikibu")
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusWaitingForMessage)
	deliverAsAnotherRuntime(t, app, id, "from.shikibu", shikibu)
	requireStatus(t, app, id, storage.StatusCompleted)
	got, err := GetWorkflowResult[string](ctx, app, id)
	require.NoError(t, err)
	require.Equal(t, "hi on from.shikibu", got.Output)
}

// Publish stores a []byte as bytes, which a Receive of any runtime gets as
// bytes; other data, a string among it, is JSON. Bytes that an earlier version
// published as a JSON string in base64 still decode into a []byte.
func TestEventData_PublishBytes(t *testing.T) {
	publisher := DefineWorkflow("publish_bytes", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if err := Publish(ctx, "out.bytes", []byte{0xff, 0}); err != nil {
			return "", err
		}
		if err := Publish(ctx, "out.empty", []byte{}); err != nil {
			return "", err
		}
		if err := Publish(ctx, "out.text", "hi"); err != nil {
			return "", err
		}
		return "ok", nil
	})
	asBytes := DefineWorkflow("receive_as_bytes", func(ctx *WorkflowContext, channel string) (string, error) {
		if err := Subscribe(ctx, channel, ModeBroadcast); err != nil {
			return "", err
		}
		msg, err := Receive[[]byte](ctx, channel)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%x %t", msg.Data, msg.Data != nil), nil
	})
	app := startTargetedTestApp(t, func(app *App) {
		RegisterWorkflow(app, publisher)
		RegisterWorkflow(app, asBytes)
	})
	ctx := context.Background()
	receivers := map[string]string{}
	for _, channel := range []string{"out.bytes", "out.empty", "out.legacy"} {
		id, err := StartWorkflow(ctx, app, asBytes, channel)
		require.NoError(t, err)
		requireStatus(t, app, id, storage.StatusWaitingForMessage)
		receivers[channel] = id
	}

	id, err := StartWorkflow(ctx, app, publisher, struct{}{})
	require.NoError(t, err)
	requireStatus(t, app, id, storage.StatusCompleted)
	require.Equal(t, "bytes ff00", storedData(t, app, "out.bytes"))
	require.Equal(t, "bytes ", storedData(t, app, "out.empty"))
	require.Equal(t, `json "hi"`, storedData(t, app, "out.text"))

	storeMessage(t, app, "out.legacy", `"/wA="`, "")

	for channel, want := range map[string]string{"out.bytes": "ff00 true", "out.empty": " true", "out.legacy": "ff00 true"} {
		requireStatus(t, app, receivers[channel], storage.StatusCompleted)
		res, err := GetWorkflowResult[string](ctx, app, receivers[channel])
		require.NoError(t, err)
		require.Equal(t, want, res.Output, channel)
	}
}
