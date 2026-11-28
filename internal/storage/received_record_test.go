package storage

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A delivery's history entry says that its data is JSON, which Edda needs to
// replay data that is not an object, and keeps the data's digits.
func TestReceivedMessageRecord(t *testing.T) {
	record := ReceivedMessageRecord("orders", &ChannelMessage{
		ID:       7,
		DataType: DataTypeJSON,
		Data:     []byte(`[12345678901234567890, "a"]`),
		Metadata: []byte(`{"ce_id": "e1"}`),
	})
	var entry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(record, &entry))
	require.JSONEq(t, `"json"`, string(entry["data_type"]))
	require.Equal(t, `[12345678901234567890,"a"]`, string(entry["data"]), "the data keeps its digits")
	require.JSONEq(t, `{"ce_id": "e1"}`, string(entry["metadata"]))
	require.JSONEq(t, `"orders"`, string(entry["channel_name"]))
	require.JSONEq(t, `7`, string(entry["id"]))

	// No metadata, and data that is not JSON, which is dropped.
	record = ReceivedMessageRecord("orders", &ChannelMessage{ID: 8, Data: []byte("not json")})
	entry = nil
	require.NoError(t, json.Unmarshal(record, &entry))
	require.JSONEq(t, `"json"`, string(entry["data_type"]))
	require.Equal(t, "null", string(entry["data"]))
	require.NotContains(t, entry, "metadata")
}

// The entry of a binary message holds the bytes in base64 and says so, the
// form that Edda writes too (SEMANTICS §3).
func TestReceivedMessageRecord_Binary(t *testing.T) {
	record := ReceivedMessageRecord("blobs", &ChannelMessage{
		ID:         9,
		DataType:   DataTypeBinary,
		DataBinary: []byte{0xff, 0x00, 'a'},
	})
	var entry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(record, &entry))
	require.JSONEq(t, `"binary"`, string(entry["data_type"]))
	require.JSONEq(t, `"/wBh"`, string(entry["data"]))
}
