---
title: "CloudEvents HTTP Binding"
weight: 2
---

Romancy fully supports the [CloudEvents HTTP Protocol Binding specification](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/bindings/http-protocol-binding), ensuring reliable event delivery and proper error handling.

## Server Setup

Romancy's `App` implements `http.Handler`, allowing easy integration with any HTTP server or router.

### Standalone Server

The simplest way to start receiving CloudEvents:

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy"
)

func main() {
	ctx := context.Background()

	app := romancy.NewApp(romancy.WithDatabase("app.db"))
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start CloudEvents server on port 8080
	log.Println("Listening for CloudEvents on :8080")
	log.Fatal(app.ListenAndServe(":8080"))
}
```

### Integration with net/http

Mount the handler on an existing HTTP server:

```go
app := romancy.NewApp(romancy.WithDatabase("app.db"))
app.Start(ctx)

// Mount as http.Handler
http.Handle("/events/", http.StripPrefix("/events", app.Handler()))
http.ListenAndServe(":8080", nil)
```

### Integration with Popular Routers

The handler routes on the request path (`/`, `/health/live`, `/cancel/{instanceID}`, ...), so strip the prefix you mount it under. Without `http.StripPrefix`, a request for `/events/health/live` reaches the CloudEvents route and gets `405`.

**Gin:**
```go
r := gin.Default()
r.Any("/events/*path", gin.WrapH(http.StripPrefix("/events", app.Handler())))
```

**Chi:**
```go
r := chi.NewRouter()
r.Mount("/events", http.StripPrefix("/events", app.Handler()))
```

**Echo:**
```go
e := echo.New()
e.Any("/events/*", echo.WrapHandler(http.StripPrefix("/events", app.Handler())))
```

### Handler Endpoints

The `app.Handler()` returns an `http.Handler` that provides:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | POST | Receive CloudEvents |
| `/health/live` | GET | Liveness probe |
| `/health/ready` | GET | Readiness probe |
| `/cancel/{instanceID}` | POST | Cancel a workflow |

## CloudEvents Content Modes

Romancy supports both CloudEvents content modes:

**Structured Mode (Recommended)**:
- All CloudEvents attributes in JSON body
- `Content-Type: application/cloudevents+json`
- No CE-* headers required
- Examples in this document use Structured Mode

**Binary Mode (Alternative)**:

```bash
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/json" \
  -H "CE-SpecVersion: 1.0" \
  -H "CE-Type: payment.completed" \
  -H "CE-Source: payment-service" \
  -H "CE-ID: event-123" \
  -d '{"amount": 99.99}'
```

Both modes are fully supported by Romancy's CloudEvents implementation.

## HTTP Response Status Codes

Romancy returns appropriate HTTP status codes according to the CloudEvents specification:

### Success (202 Accepted)

Romancy answers 202 once the event is stored: it is published to its channel, and the instance of the workflow that handles its type (registered with `WithEventHandler(true)`) is created. From then on the event is kept even if the process stops: a running worker resumes the workflows that have not handled it yet.

```bash
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123",
    "data": {"amount": 99.99}
  }'
```

**Response:**
```http
HTTP/1.1 202 Accepted
Content-Type: application/json

{
  "status": "accepted"
}
```

**When returned:**

- ✅ The event was parsed and stored
- ✅ The workflow that handles the event runs, and the workflows waiting for it (`WaitEvent`) are woken, after the response
- ✅ Final processing outcome is not yet known

### Client Error (400 Bad Request)

When the CloudEvent is malformed or fails validation (**non-retryable**):

```bash
# Missing required field: specversion
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123"
  }'
```

**Response:**
```http
HTTP/1.1 400 Bad Request
Content-Type: application/json

{
  "error": "Failed to find specversion in HTTP request",
  "error_type": "ValidationError",
  "retryable": "false"
}
```

**When returned:**

- ❌ Missing required CloudEvents fields (`specversion`, `type`, `source`, `id`)
- ❌ Invalid JSON format
- ❌ CloudEvents validation errors

**Client action:**

- 🚫 **DO NOT retry** - Fix the event structure and resend

### Forbidden (403 Forbidden)

When the event's channel is sealed (**non-retryable**). An event is published to the channel named by its `type`, or `type:<instance ID>` when it targets one instance. Once a `channel_pub` capability has been issued for that channel, the event must carry a valid token in the `capabilitytoken` extension attribute (in binary mode, the `ce-capabilitytoken` header):

```bash
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123",
    "capabilitytoken": "cap:channel_pub:payment.completed:..."
  }'
```

Without a token, or with an invalid or expired one:

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json

{
  "error": "...",
  "error_type": "Forbidden",
  "retryable": "false"
}
```

Nothing is published and no workflow starts. The `capabilitytoken` attribute is never stored with the message.

### Server Error (500 Internal Server Error)

When an internal error occurs (**retryable**):

**Response:**
```http
HTTP/1.1 500 Internal Server Error
Content-Type: application/json

{
  "error": "Database connection failed",
  "error_type": "DatabaseError",
  "retryable": "true"
}
```

**When returned:**

- ⚠️ The event could not be stored (for example, the database cannot be reached)
- ⚠️ Internal server errors
- ⚠️ Unexpected errors

**Client action:**

- 🔄 **Retry with exponential backoff**

### Service Unavailable (503 Service Unavailable)

When the app is not running: before `Start`, or once `Shutdown` has begun (**retryable**). Nothing is published and no workflow starts.

**Response:**
```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{
  "error": "app is not running",
  "error_type": "ServiceUnavailable",
  "retryable": "true"
}
```

**Client action:**

- 🔄 **Retry with exponential backoff**: another worker, or this one once it is started again, accepts the event

`Shutdown` waits for the events it accepted before: they finish storing and waking the workflows that wait for them, within the shutdown timeout (`WithShutdownTimeout`). Then it cancels the runs in progress, including the first runs of the workflows that events started, and waits for them before storage closes. A cancelled run leaves its instance `running`, and a later run continues it.

### Delivery Guarantee

Events are delivered at least once: a sender that retries after a 500, a 503 or a lost response can store the same event twice. A workflow that must not handle an event twice deduplicates on an id it receives. A workflow that waits for the event gets its `id` (`ReceivedEvent.ID`); a workflow that the event starts (`WithEventHandler(true)`) gets only its `data`, so the sender puts an id there.

## Error Response Structure

All error responses include structured information to help clients decide whether to retry:

```json
{
  "error": "Human-readable error message",
  "error_type": "ErrorTypeName",
  "retryable": "true" | "false"
}
```

### Fields

- **`error`** (string): Human-readable error message
- **`error_type`** (string): Error type name for debugging
- **`retryable`** (string, `"true"` or `"false"`): Whether the client should retry
  - `"false"`: Client error (400, 403) - Fix the request before retrying
  - `"true"`: Server error (500, 503) - Retry with exponential backoff

## Client Retry Logic

Example retry implementation in Go:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrorResponse represents Romancy error response
type ErrorResponse struct {
	Error     string `json:"error"`
	ErrorType string `json:"error_type"`
	Retryable string `json:"retryable"` // "true" or "false"
}

// SendCloudEventWithRetry sends a CloudEvent with automatic retry on server errors
func SendCloudEventWithRetry(eventData map[string]any, maxRetries int) error {
	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("POST", "http://localhost:8001/", bytes.NewBuffer(eventBytes))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/cloudevents+json")

		resp, err := client.Do(req)
		if err != nil {
			// Connection error - Retry
			if attempt < maxRetries-1 {
				waitTime := time.Duration(1<<attempt) * time.Second // Exponential backoff
				fmt.Printf("⚠️ Connection error, retrying in %v...\n", waitTime)
				time.Sleep(waitTime)
				continue
			}
			return fmt.Errorf("connection error after %d retries: %w", maxRetries, err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		switch resp.StatusCode {
		case http.StatusAccepted: // 202
			fmt.Println("✅ Event accepted")
			return nil

		case http.StatusBadRequest, http.StatusForbidden: // 400, 403
			// Client error - DO NOT retry
			var errResp ErrorResponse
			json.Unmarshal(body, &errResp)
			return fmt.Errorf("non-retryable error: %s", errResp.Error)

		case http.StatusInternalServerError, http.StatusServiceUnavailable: // 500, 503
			// Server error - Retry with exponential backoff
			var errResp ErrorResponse
			json.Unmarshal(body, &errResp)

			if attempt < maxRetries-1 {
				waitTime := time.Duration(1<<attempt) * time.Second
				fmt.Printf("⚠️ Server error, retrying in %v: %s\n", waitTime, errResp.Error)
				time.Sleep(waitTime)
				continue
			}
			return fmt.Errorf("max retries exceeded: %s", errResp.Error)
		}
	}

	return fmt.Errorf("unexpected error after %d attempts", maxRetries)
}

func main() {
	event := map[string]any{
		"specversion": "1.0",
		"type":        "payment.completed",
		"source":      "payment-service",
		"id":          "event-123",
		"data": map[string]any{
			"order_id": "ORD-123",
			"amount":   99.99,
		},
	}

	if err := SendCloudEventWithRetry(event, 3); err != nil {
		fmt.Printf("Failed to send event: %v\n", err)
	}
}
```

## CloudEvents Specification Compliance

Romancy complies with the following CloudEvents specifications:

### HTTP Protocol Binding v1.0.2

✅ **Success Responses:**

- `202 Accepted` - Event accepted for async processing (recommended for async systems)
- `200 OK` - Event processed synchronously (not used by Romancy)

✅ **Client Error Responses (Non-Retryable):**

- `400 Bad Request` - Malformed CloudEvent
- `403 Forbidden` - The event's channel is sealed and the event carries no valid `capabilitytoken`
- `415 Unsupported Media Type` - (Reserved for future use)

✅ **Server Error Responses (Retryable):**

- `500 Internal Server Error` - Internal error
- `503 Service Unavailable` - The app is not running (before `Start`, or once `Shutdown` has begun)

❌ **Prohibited:**

- 3xx redirect codes - Not allowed by CloudEvents spec

### Error Response Extensions

Romancy extends the CloudEvents specification with additional error metadata:

```json
{
  "error": "Error message",
  "error_type": "ErrorTypeName",
  "retryable": "true" | "false"
}
```

This extension helps clients make intelligent retry decisions without parsing error messages.

## Integration Examples

### With CloudEvents SDK (Go)

Using the official CloudEvents Go SDK:

```go
package main

import (
	"context"
	"fmt"
	"log"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

func main() {
	// Create CloudEvents client
	c, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget("http://localhost:8001/"),
	)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// Create CloudEvent
	event := cloudevents.NewEvent()
	event.SetType("payment.completed")
	event.SetSource("payment-service")
	event.SetID("event-123")
	event.SetData(cloudevents.ApplicationJSON, map[string]any{
		"order_id": "ORD-123",
		"amount":   99.99,
	})

	// Send to Romancy
	ctx := context.Background()
	result := c.Send(ctx, event)

	if cloudevents.IsACK(result) {
		fmt.Println("✅ Event accepted")
	} else if cloudevents.IsUndelivered(result) {
		fmt.Printf("❌ Failed to send: %v\n", result)
	} else {
		fmt.Printf("⚠️ Unexpected result: %v\n", result)
	}
}
```

### Using net/http Directly

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func sendCloudEvent() error {
	event := map[string]any{
		"specversion": "1.0",
		"type":        "payment.completed",
		"source":      "payment-service",
		"id":          "event-123",
		"data": map[string]any{
			"order_id": "ORD-123",
			"amount":   99.99,
		},
	}

	eventBytes, err := json.Marshal(event)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "http://localhost:8001/", bytes.NewBuffer(eventBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusAccepted:
		fmt.Println("✅ Event accepted")
		return nil
	case http.StatusBadRequest:
		var errResp map[string]any
		json.Unmarshal(body, &errResp)
		return fmt.Errorf("client error: %v", errResp["error"])
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		var errResp map[string]any
		json.Unmarshal(body, &errResp)
		return fmt.Errorf("server error (retryable): %v", errResp["error"])
	default:
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
}

func main() {
	if err := sendCloudEvent(); err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
```

### Using curl

```bash
# Send CloudEvent via curl
curl -X POST http://localhost:8001/ \
  -H "Content-Type: application/cloudevents+json" \
  -d '{
    "specversion": "1.0",
    "type": "payment.completed",
    "source": "payment-service",
    "id": "event-123",
    "data": {
      "order_id": "ORD-123",
      "amount": 99.99
    }
  }'
```

## Best Practices

### 1. Always Check Response Status

```go
// ❌ Bad: Ignoring response status
resp, _ := client.Do(req)
// No error handling

// ✅ Good: Checking response status
resp, err := client.Do(req)
if err != nil {
	return err
}
defer resp.Body.Close()

if resp.StatusCode != http.StatusAccepted {
	return handleError(resp)
}
```

### 2. Implement Retry Logic

```go
// ✅ Retry on 500 and 503, don't retry on 400
switch resp.StatusCode {
case http.StatusBadRequest:
	// Don't retry - fix the request
	return fmt.Errorf("invalid event: %w", parseError(resp))
case http.StatusInternalServerError, http.StatusServiceUnavailable:
	var errResp ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)
	if errResp.Retryable == "true" {
		return retryWithBackoff()
	}
}
```

### 3. Use Structured Logging

```go
package main

import (
	"log/slog"
	"os"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Send event...

	logger.Info("cloudevent_sent",
		"status_code", resp.StatusCode,
		"event_type", eventData["type"],
		"retryable", errResp.Retryable,
	)
}
```

### 4. Set Appropriate Timeouts

```go
// ✅ Configure client with timeout
client := &http.Client{
	Timeout: 30 * time.Second,
}

// Or use context for more control
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

req = req.WithContext(ctx)
resp, err := client.Do(req)
if err != nil {
	return err
}
defer resp.Body.Close()
```

## Related Documentation

- **[Event Waiting Example](/docs/examples/events)**: Complete event-driven workflow examples
- **[CloudEvents Specification](https://github.com/cloudevents/spec)**: Official CloudEvents spec
- **[HTTP Protocol Binding](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/bindings/http-protocol-binding)**: HTTP binding specification
- **[CloudEvents Go SDK](https://github.com/cloudevents/sdk-go)**: Official Go SDK
