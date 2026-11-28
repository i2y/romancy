package storage

import (
	"encoding/base64"
	"encoding/json"
)

// ReceivedMessageRecord is the ChannelMessageReceived history entry of a
// receive: the message that Receive replays. A delivery writes it into the
// history of a waiting instance of any runtime, so it is in the form that
// every runtime reads (SEMANTICS §3). It says whether the data is JSON or
// bytes: JSON data is kept as it is, and bytes are in base64 with
// "data_type": "binary". Edda reads an entry without "data_type" as one that
// a version before its 0.17 wrote, which kept only objects, and replays any
// other data as an empty object.
func ReceivedMessageRecord(channelName string, message *ChannelMessage) []byte {
	record := map[string]any{
		"id":           message.ID,
		"channel_name": channelName,
		"created_at":   message.PublishedAt,
	}
	if message.IsBinary() {
		record["data_type"] = DataTypeBinary
		record["data"] = base64.StdEncoding.EncodeToString(message.DataBinary)
	} else {
		record["data_type"] = DataTypeJSON
		record["data"] = rawJSONOrNil(message.Data)
	}
	if len(message.Metadata) > 0 {
		record["metadata"] = rawJSONOrNil(message.Metadata)
	}
	recordJSON, _ := json.Marshal(record)
	return recordJSON
}

// rawJSONOrNil keeps a JSON document as it is (numbers keep their digits),
// and drops anything that is not JSON.
func rawJSONOrNil(document []byte) any {
	if len(document) == 0 || !json.Valid(document) {
		return nil
	}
	return json.RawMessage(document)
}
