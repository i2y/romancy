package romancy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// A channel message's data is JSON or bytes (SEMANTICS §3). Every runtime
// stores bytes in a binary message (data_type 'binary') and receives them as
// its byte type: Go []byte, Python bytes, a Ruby String in the binary
// encoding. A receive record holds them in base64, with "data_type":
// "binary".

// messagePayload is a message's data as it is stored: JSON, or bytes.
type messagePayload struct {
	data   []byte
	binary bool
}

// publish stores the payload as a message on channel and returns it as
// stored, for the wake of the waiting subscribers.
func (p messagePayload) publish(ctx context.Context, s storage.Storage, channel string, metadata []byte) (*storage.ChannelMessage, error) {
	publish := s.PublishToChannel
	if p.binary {
		publish = s.PublishBinaryToChannel
	}
	id, err := publish(ctx, channel, p.data, metadata)
	if err != nil {
		return nil, err
	}
	return p.message(id, channel, metadata), nil
}

// message is the payload as the message that stores it, with row id id.
func (p messagePayload) message(id int64, channel string, metadata []byte) *storage.ChannelMessage {
	message := &storage.ChannelMessage{ID: id, Channel: channel, Metadata: metadata}
	if p.binary {
		message.DataType, message.DataBinary = storage.DataTypeBinary, p.data
	} else {
		message.DataType, message.Data = storage.DataTypeJSON, p.data
	}
	return message
}

// decodeMessageData decodes a stored message's data into T: JSON data as
// JSON, bytes as decodeBytes does.
func decodeMessageData[T any](msg *storage.ChannelMessage, into *T) error {
	if msg.IsBinary() {
		return decodeBytes(msg.DataBinary, into)
	}
	return json.Unmarshal(msg.Data, into)
}

// decodeBytes stores the bytes of a binary message in into: as they are for
// a []byte (or another byte-slice type) and for an any, as a string for a
// string type, and, for a json.RawMessage, as the JSON string in base64 that
// encoding/json writes for bytes. No other type holds bytes.
func decodeBytes[T any](data []byte, into *T) error {
	switch p := any(into).(type) {
	case *json.RawMessage:
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		*p = encoded
		return nil
	case *any:
		*p = data
		return nil
	}
	v := reflect.ValueOf(into).Elem()
	switch {
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		v.Set(reflect.ValueOf(data).Convert(v.Type()))
	case v.Kind() == reflect.String:
		v.SetString(string(data))
	default:
		return fmt.Errorf("the data is bytes, which a %s cannot hold: receive it as []byte, string or any", v.Type())
	}
	return nil
}

// receiveRecord is the ChannelMessageReceived entry of a receive, as any
// runtime writes it: Romancy names the channel channel_name and the time
// created_at, Edda and Shikibu channel and published_at, and Edda's id is
// the message's UUID. DataType is "binary" for bytes, held in base64.
type receiveRecord struct {
	ID          any             `json:"id"`
	Channel     string          `json:"channel"`
	ChannelName string          `json:"channel_name"`
	DataType    string          `json:"data_type"`
	Data        json.RawMessage `json:"data"`
	Metadata    map[string]any  `json:"metadata"`
	PublishedAt string          `json:"published_at"`
	CreatedAt   *time.Time      `json:"created_at"`
}

// messageFromRecord is the message a receive record holds, without its data.
func messageFromRecord[T any](record *receiveRecord) *ReceivedMessage[T] {
	// Resolve channel name (prefer channel_name, fallback to channel)
	channel := record.ChannelName
	if channel == "" {
		channel = record.Channel
	}

	// Resolve time (prefer created_at, fallback to published_at)
	var createdAt time.Time
	if record.CreatedAt != nil {
		createdAt = *record.CreatedAt
	} else if record.PublishedAt != "" {
		// Try multiple ISO 8601 formats
		for _, layout := range []string{
			time.RFC3339,
			time.RFC3339Nano,
			"2006-01-02T15:04:05.999999",
			"2006-01-02T15:04:05",
		} {
			if t, err := time.Parse(layout, record.PublishedAt); err == nil {
				createdAt = t
				break
			}
		}
	}

	// Resolve ID (handle both int64 and string UUID)
	var id int64
	switch v := record.ID.(type) {
	case float64:
		id = int64(v)
	case int64:
		id = v
	case string:
		// UUID string - use 0 as fallback (ID is mostly for logging)
		id = 0
	}

	return &ReceivedMessage[T]{
		ID:          id,
		ChannelName: channel,
		Metadata:    record.Metadata,
		CreatedAt:   createdAt,
	}
}

// replayBinaryMessage replays the record of a receive whose data was bytes.
func replayBinaryMessage[T any](record *receiveRecord) (*ReceivedMessage[T], error) {
	var encoded string
	if err := json.Unmarshal(record.Data, &encoded); err != nil {
		return nil, fmt.Errorf("failed to deserialize cached message data: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize cached message data: %w", err)
	}
	msg := messageFromRecord[T](record)
	if err := decodeBytes(data, &msg.Data); err != nil {
		return nil, fmt.Errorf("failed to deserialize cached message data: %w", err)
	}
	msg.binary, msg.binaryData = true, data
	return msg, nil
}
