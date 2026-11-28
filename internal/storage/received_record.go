package storage

import "encoding/json"

// receivedMessageRecord is the ChannelMessageReceived history entry that a
// delivery writes: the ReceivedMessage that Receive replays. The waiter scan
// delivers to the instances of every runtime, and the entry also says that
// the data is JSON: Edda reads an entry without "data_type" as one that a
// version before its 0.17 wrote, which kept only objects, and replays any
// other data as an empty object.
func receivedMessageRecord(channelName string, message *ChannelMessage) []byte {
	record := map[string]any{
		"id":           message.ID,
		"channel_name": channelName,
		"created_at":   message.PublishedAt,
		"data_type":    "json",
		"data":         rawJSONOrNil(message.Data),
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
