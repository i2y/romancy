package romancy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/cloudevents/sdk-go/v2/event"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/cloudevents/sdk-go/v2/types"
)

// cloudEventReadError is a request that the endpoint refuses with 400:
// errType is ValidationError for an event without a required attribute (or
// with an invalid one), ParseError for any other request it cannot read.
type cloudEventReadError struct {
	errType string
	err     error
}

func (e *cloudEventReadError) Error() string { return e.err.Error() }
func (e *cloudEventReadError) Unwrap() error { return e.err }

// readCloudEvent reads the event of an incoming request in either content
// mode of the CloudEvents HTTP binding (SEMANTICS §3c). A request whose
// Content-Type is application/cloudevents+json is a structured event: the
// body is the event. A request with a ce-specversion header is a binary
// event: the attributes are its ce-* headers, the body is the data and the
// Content-Type its datacontenttype. The body of any other request is read as
// a structured event. The event must have the required attributes
// specversion, id, source and type.
func readCloudEvent(r *http.Request) (*CloudEvent, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, &cloudEventReadError{"ParseError", fmt.Errorf("cannot read the request body: %w", err)}
	}
	message := cehttp.NewMessage(r.Header, io.NopCloser(bytes.NewReader(body)))
	var e *event.Event
	switch message.ReadEncoding() {
	case binding.EncodingBinary:
		e, err = binding.ToEvent(r.Context(), message)
	case binding.EncodingBatch:
		err = errors.New("the batched content mode is not supported: send one event per request")
	default:
		e, err = readStructuredEvent(body)
	}
	if err == nil {
		err = e.Validate()
	}
	if err != nil {
		var invalid event.ValidationError
		if errors.As(err, &invalid) {
			return nil, &cloudEventReadError{"ValidationError", errors.New(validationMessage(invalid))}
		}
		return nil, &cloudEventReadError{"ParseError", fmt.Errorf("cannot read a CloudEvent: %w", err)}
	}
	return cloudEventFromSDK(e)
}

// validationMessage lists the invalid attributes of an event, by name.
func validationMessage(invalid event.ValidationError) string {
	names := slices.Sorted(maps.Keys(invalid))
	problems := make([]string, len(names))
	for i, name := range names {
		problems[i] = name + ": " + invalid[name].Error()
	}
	return "invalid CloudEvent: " + strings.Join(problems, "; ")
}

// readStructuredEvent reads a structured event: the body is the event. The
// SDK reads the data member as a JSON value only for application/json and
// text/json, and refuses the JSON value of any other JSON type (*/json,
// */*+json), which the JSON format reads as JSON too: it reads such an event
// as application/json, and the event keeps its own type.
func readStructuredEvent(body []byte) (*event.Event, error) {
	e := &event.Event{}
	var members map[string]json.RawMessage
	var contentType string
	if json.Unmarshal(body, &members) == nil &&
		json.Unmarshal(members["datacontenttype"], &contentType) == nil &&
		isJSONMediaType(mediaTypeOf(contentType)) && !sdkReadsAsJSON(contentType) {
		members["datacontenttype"], _ = json.Marshal(event.ApplicationJSON)
		asJSON, err := json.Marshal(members)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(asJSON, e); err != nil {
			return nil, err
		}
		e.SetDataContentType(contentType)
		return e, nil
	}
	return e, json.Unmarshal(body, e)
}

// sdkReadsAsJSON reports whether the SDK reads the data member of a
// structured event of this datacontenttype as a JSON value.
func sdkReadsAsJSON(contentType string) bool {
	switch mediaTypeOf(contentType) {
	case event.ApplicationJSON, event.TextJSON, event.ApplicationCloudEventsJSON, event.ApplicationCloudEventsBatchJSON:
		return true
	}
	return false
}

// cloudEventFromSDK converts an event that the CloudEvents SDK read.
func cloudEventFromSDK(e *event.Event) (*CloudEvent, error) {
	data, binary, err := cloudEventData(e)
	if err != nil {
		return nil, &cloudEventReadError{"ParseError", err}
	}
	ce := &CloudEvent{
		ID:              e.ID(),
		Type:            e.Type(),
		Source:          e.Source(),
		SpecVersion:     e.SpecVersion(),
		DataSchema:      e.DataSchema(),
		Subject:         e.Subject(),
		DataContentType: e.DataContentType(),
		Data:            data,
		Extensions:      make(map[string]any, len(e.Extensions())),
	}
	if binary {
		// Data keeps the bytes as JSON: the input of the workflow that
		// handles the event, which a []byte decodes.
		ce.binary, ce.binaryData = true, data
		if ce.Data, err = json.Marshal(data); err != nil {
			return nil, &cloudEventReadError{"ParseError", err}
		}
	}
	if t := e.Time(); !t.IsZero() {
		ce.Time = &t
	}
	for name, value := range e.Extensions() {
		ce.Extensions[name] = cloudEventExtensionValue(value)
	}
	return ce, nil
}

// cloudEventExtensionValue is an extension attribute's value in JSON:
// strings, booleans and integers as they are, the other CloudEvents types
// (URI, URI-reference, timestamp, binary) in their canonical string form.
func cloudEventExtensionValue(value any) any {
	switch value.(type) {
	case string, bool, int32:
		return value
	}
	if s, err := types.Format(value); err == nil {
		return s
	}
	return value
}

// cloudEventData is an event's data as its channel message stores it
// (SEMANTICS §3c), by its datacontenttype, whichever content mode the event
// came in. JSON data (a JSON type: */json, */*+json) is kept as it is; data
// of a JSON type that is not JSON is refused. Text (text/*, in UTF-8) is a
// JSON string. Any other data is bytes (binary is true). Without a
// datacontenttype, data that is JSON is JSON (a structured event's data
// member always is), and data_base64 and other data are bytes.
func cloudEventData(e *event.Event) (data []byte, binary bool, err error) {
	data = e.Data()
	if len(data) == 0 {
		return nil, false, nil
	}
	mediaType := e.DataMediaType()
	switch {
	case mediaType == "":
		if !e.DataBase64 && json.Valid(data) {
			return data, false, nil
		}
		return data, true, nil
	case isJSONMediaType(mediaType):
		if !json.Valid(data) {
			return nil, false, fmt.Errorf("the data is not JSON, but its datacontenttype is %s", mediaType)
		}
		return data, false, nil
	case strings.HasPrefix(mediaType, "text/") && utf8.Valid(data):
		text, err := json.Marshal(string(data))
		return text, false, err
	default:
		return data, true, nil
	}
}

// isJSONMediaType reports whether data of mediaType is JSON: */json or
// */*+json (the JSON format of CloudEvents, §3.1).
func isJSONMediaType(mediaType string) bool {
	return strings.HasSuffix(mediaType, "/json") || strings.HasSuffix(mediaType, "+json")
}

// mediaTypeOf is the media type of a content type, lowercased and without
// its parameters.
func mediaTypeOf(contentType string) string {
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		return mediaType
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mediaType))
}
