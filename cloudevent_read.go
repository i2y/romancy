package romancy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	case binding.EncodingStructured, binding.EncodingBinary:
		e, err = binding.ToEvent(r.Context(), message)
	case binding.EncodingBatch:
		err = errors.New("the batched content mode is not supported: send one event per request")
	default:
		e = &event.Event{}
		err = json.Unmarshal(body, e)
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

// cloudEventFromSDK converts an event that the CloudEvents SDK read.
func cloudEventFromSDK(e *event.Event) (*CloudEvent, error) {
	data, err := cloudEventDataJSON(e)
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

// cloudEventDataJSON is an event's data as the JSON its channel message
// carries. JSON data (a JSON datacontenttype, or none and data that is JSON)
// is kept as it is. Text (a text/* datacontenttype, in UTF-8) is a string.
// Any other data is its bytes, in base64, which a []byte decodes.
func cloudEventDataJSON(e *event.Event) (json.RawMessage, error) {
	data := e.Data()
	if len(data) == 0 {
		return nil, nil
	}
	mediaType := e.DataMediaType()
	switch {
	case mediaType == "" && !e.DataBase64 && json.Valid(data):
		return data, nil
	case mediaType != "" && isJSONMediaType(mediaType):
		if !json.Valid(data) {
			return nil, fmt.Errorf("the data is not JSON, but its datacontenttype is %s", mediaType)
		}
		return data, nil
	case strings.HasPrefix(mediaType, "text/") && utf8.Valid(data):
		return json.Marshal(string(data))
	default:
		return json.Marshal(data)
	}
}

// isJSONMediaType reports whether data of mediaType is JSON.
func isJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || mediaType == "text/json" || strings.HasSuffix(mediaType, "+json")
}
