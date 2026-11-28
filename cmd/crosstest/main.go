// Package main provides a CLI tool for cross-language channel testing between Romancy (Go) and Edda (Python).
//
// Usage:
//
//	# Subscriber mode (wait for message and print it)
//	go run ./cmd/crosstest --db=/tmp/cross_test.db --channel=test --mode=subscriber --timeout=30
//
//	# Publisher mode (send a test message)
//	go run ./cmd/crosstest --db=/tmp/cross_test.db --channel=test --mode=publisher --message='{"hello": "world"}'
//
//	# Supervisor mode (supervise one child that another runtime runs; print how it ended)
//	go run ./cmd/crosstest --db=/tmp/cross_test.db --mode=supervisor --child-framework=python --child-workflow=crosstest_py_echo --child-input='{"msg": "hi"}'
//
//	# Worker mode (run crosstest_go_echo and crosstest_go_string for other runtimes' supervisors)
//	go run ./cmd/crosstest --db=/tmp/cross_test.db --mode=worker --timeout=120
//
//	# Wait-event mode (wait for one event of a type with WaitEvent and print it)
//	go run ./cmd/crosstest --db=/tmp/cross_test.db --mode=wait-event --event-type=order.shipped --timeout=30
//
//	# Post-event mode (send a structured CloudEvent through the app's endpoint, in this process)
//	go run ./cmd/crosstest --db=/tmp/cross_test.db --mode=post-event --event='{"specversion": "1.0", ...}'
//
//nolint:gocritic // exitAfterDefer is acceptable for CLI tools using log.Fatalf
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/i2y/romancy"
)

// TestMessage is a message type with strict Go types.
// Uses time.Time for Timestamp - this causes deserialization issues
// when receiving messages published from Python (ISO string format).
type TestMessage struct {
	Text      string                 `json:"text"`
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// CompatibleMessage is a cross-language compatible message type.
// Uses string for Timestamp to match Python's ISO format.
type CompatibleMessage struct {
	Text      string         `json:"text"`
	Timestamp string         `json:"timestamp,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// SubscriberInput is the input for the subscriber workflow.
type SubscriberInput struct {
	Channel        string `json:"channel"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ChannelMode    string `json:"channel_mode"` // "broadcast", "competing", or "direct"
	MessageType    string `json:"message_type"` // "strict" (TestMessage) or "compatible" (CompatibleMessage)
}

// SubscriberResult is the output of the subscriber workflow.
type SubscriberResult struct {
	Received bool           `json:"received"`
	Message  any            `json:"message,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// PublisherInput is the input for the publisher workflow.
type PublisherInput struct {
	Channel  string                 `json:"channel"`
	Text     string                 `json:"text"`
	Data     map[string]interface{} `json:"data,omitempty"`
	Metadata map[string]any         `json:"metadata,omitempty"`
}

// PublisherResult is the output of the publisher workflow.
type PublisherResult struct {
	Published bool   `json:"published"`
	Error     string `json:"error,omitempty"`
}

// SendToInput is the input for the send-to workflow (direct messaging).
type SendToInput struct {
	TargetInstanceID string                 `json:"target_instance_id"`
	Channel          string                 `json:"channel"`
	Text             string                 `json:"text"`
	Data             map[string]interface{} `json:"data,omitempty"`
	Metadata         map[string]any         `json:"metadata,omitempty"`
}

// SendToResult is the output of the send-to workflow.
type SendToResult struct {
	Sent  bool   `json:"sent"`
	Error string `json:"error,omitempty"`
}

// subscriberWorkflow waits for a message on the channel.
var subscriberWorkflow = romancy.DefineWorkflow("crosstest_subscriber",
	func(ctx *romancy.WorkflowContext, input SubscriberInput) (SubscriberResult, error) {
		result := SubscriberResult{Received: false}

		// Determine subscription mode
		var mode romancy.ChannelMode
		switch input.ChannelMode {
		case "competing":
			mode = romancy.ModeCompeting
		case "direct":
			mode = romancy.ModeDirect
		default:
			mode = romancy.ModeBroadcast
		}

		log.Printf("[Subscriber] Subscribing to channel '%s' in %s mode (instance: %s)", input.Channel, input.ChannelMode, ctx.InstanceID())
		if err := romancy.Subscribe(ctx, input.Channel, mode); err != nil {
			result.Error = fmt.Sprintf("subscribe failed: %v", err)
			return result, nil
		}

		log.Printf("[Subscriber] Waiting for message (timeout: %ds, type: %s)...", input.TimeoutSeconds, input.MessageType)
		timeout := time.Duration(input.TimeoutSeconds) * time.Second

		// Use different message types based on MessageType setting
		if input.MessageType == "compatible" {
			// Use CompatibleMessage (string timestamp) for cross-language compatibility
			msg, err := romancy.Receive[CompatibleMessage](ctx, input.Channel, romancy.WithReceiveTimeout(timeout))
			if err != nil {
				var timeoutErr *romancy.ChannelMessageTimeoutError
				if errors.As(err, &timeoutErr) {
					result.Error = "timeout"
					return result, nil
				}
				if romancy.IsSuspendSignal(err) {
					return result, err
				}
				result.Error = fmt.Sprintf("receive failed: %v", err)
				return result, nil
			}
			log.Printf("[Subscriber] Received compatible message: %+v", msg.Data)
			result.Received = true
			result.Message = msg.Data
			result.Metadata = msg.Metadata
			return result, nil
		}

		// Default: Use TestMessage (time.Time timestamp) - may fail with Python messages
		msg, err := romancy.Receive[TestMessage](ctx, input.Channel, romancy.WithReceiveTimeout(timeout))
		if err != nil {
			var timeoutErr *romancy.ChannelMessageTimeoutError
			if errors.As(err, &timeoutErr) {
				result.Error = "timeout"
				return result, nil
			}
			if romancy.IsSuspendSignal(err) {
				return result, err
			}
			result.Error = fmt.Sprintf("receive failed: %v", err)
			return result, nil
		}

		log.Printf("[Subscriber] Received message: %+v", msg.Data)
		result.Received = true
		result.Message = msg.Data
		result.Metadata = msg.Metadata
		return result, nil
	},
)

// publisherWorkflow sends a message to the channel.
var publisherWorkflow = romancy.DefineWorkflow("crosstest_publisher",
	func(ctx *romancy.WorkflowContext, input PublisherInput) (PublisherResult, error) {
		result := PublisherResult{Published: false}

		msg := TestMessage{
			Text:      input.Text,
			Timestamp: time.Now().UTC(),
			Data:      input.Data,
		}

		log.Printf("[Publisher] Publishing message to channel '%s': %s", input.Channel, input.Text)

		var opts []romancy.PublishOption
		if input.Metadata != nil {
			opts = append(opts, romancy.WithMetadata(input.Metadata))
		}

		if err := romancy.Publish(ctx, input.Channel, msg, opts...); err != nil {
			result.Error = fmt.Sprintf("publish failed: %v", err)
			return result, nil
		}

		log.Printf("[Publisher] Message published successfully")
		result.Published = true
		return result, nil
	},
)

// sendToWorkflow sends a message directly to a specific workflow instance.
var sendToWorkflow = romancy.DefineWorkflow("crosstest_send_to",
	func(ctx *romancy.WorkflowContext, input SendToInput) (SendToResult, error) {
		result := SendToResult{Sent: false}

		msg := TestMessage{
			Text:      input.Text,
			Timestamp: time.Now().UTC(),
			Data:      input.Data,
		}

		log.Printf("[SendTo] Sending direct message to instance '%s' on channel '%s': %s", input.TargetInstanceID, input.Channel, input.Text)

		var opts []romancy.PublishOption
		if input.Metadata != nil {
			opts = append(opts, romancy.WithMetadata(input.Metadata))
		}

		if err := romancy.SendTo(ctx, input.TargetInstanceID, input.Channel, msg, opts...); err != nil {
			result.Error = fmt.Sprintf("send_to failed: %v", err)
			return result, nil
		}

		log.Printf("[SendTo] Message sent successfully")
		result.Sent = true
		return result, nil
	},
)

// EchoInput is the input of crosstest_go_echo, which a supervisor of another
// runtime starts as its child.
type EchoInput struct {
	Msg string `json:"msg"`
}

// EchoResult is the output of crosstest_go_echo.
type EchoResult struct {
	Echo string `json:"echo"`
	By   string `json:"by"`
}

var goEchoWorkflow = romancy.DefineWorkflow("crosstest_go_echo",
	func(ctx *romancy.WorkflowContext, input EchoInput) (EchoResult, error) {
		return EchoResult{Echo: input.Msg, By: "romancy"}, nil
	},
)

// goStringWorkflow takes a string: the JSON null that a child started without
// an input gets decodes to "", while an object would not decode at all.
var goStringWorkflow = romancy.DefineWorkflow("crosstest_go_string",
	func(ctx *romancy.WorkflowContext, input string) (string, error) {
		return fmt.Sprintf("romancy got %q", input), nil
	},
)

// WaitEventInput is the input for the wait-event workflow.
type WaitEventInput struct {
	EventType      string `json:"event_type"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// WaitEventResult is what wait-event mode prints: the event that WaitEvent
// returned, its data as JSON.
type WaitEventResult struct {
	Received bool                                    `json:"received"`
	Event    *romancy.ReceivedEvent[json.RawMessage] `json:"event,omitempty"`
	Error    string                                  `json:"error,omitempty"`
}

// waitEventWorkflow waits for one event of a type. The CloudEvents endpoint
// of any runtime may have stored it, and any runtime may have delivered it.
var waitEventWorkflow = romancy.DefineWorkflow("crosstest_wait_event",
	func(ctx *romancy.WorkflowContext, input WaitEventInput) (WaitEventResult, error) {
		log.Printf("[WaitEvent] Waiting for an event of type '%s' (instance: %s)", input.EventType, ctx.InstanceID())
		event, err := romancy.WaitEvent[json.RawMessage](ctx, input.EventType,
			romancy.WithEventTimeout(time.Duration(input.TimeoutSeconds)*time.Second))
		if err != nil {
			if romancy.IsSuspendSignal(err) {
				return WaitEventResult{}, err
			}
			return WaitEventResult{Error: err.Error()}, nil
		}
		return WaitEventResult{Received: true, Event: event}, nil
	},
)

// PostEventResult is what post-event mode prints: the endpoint's answer.
type PostEventResult struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

// SupervisionResult is what supervisor mode prints: how the supervisor and
// its child ended.
type SupervisionResult struct {
	SupervisorStatus string          `json:"supervisor_status"`
	ChildInstanceID  string          `json:"child_instance_id"`
	ChildFramework   string          `json:"child_framework"`
	ChildStatus      string          `json:"child_status"`
	ChildInput       json.RawMessage `json:"child_input"`
	ChildOutput      json.RawMessage `json:"child_output,omitempty"`
}

// waitForSupervision waits until the supervisor ends and reports its child.
func waitForSupervision(ctx context.Context, app *romancy.App, instanceID string, timeout time.Duration) (*SupervisionResult, error) {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		sup, err := app.GetInstance(ctx, instanceID)
		if err != nil {
			return nil, err
		}
		switch sup.Status {
		case romancy.StatusCompleted, romancy.StatusFailed, romancy.StatusCancelled:
		default:
			continue
		}
		childID := instanceID + ":child:g1"
		if result, err := romancy.GetWorkflowResult[romancy.SupervisorResult](ctx, app, instanceID); err == nil &&
			len(result.Output.Children) == 1 {
			childID = result.Output.Children[0].InstanceID // the last generation
		}
		child, err := app.GetInstance(ctx, childID)
		if err != nil {
			return nil, err
		}
		return &SupervisionResult{
			SupervisorStatus: string(sup.Status),
			ChildInstanceID:  child.InstanceID,
			ChildFramework:   child.Framework,
			ChildStatus:      string(child.Status),
			ChildInput:       child.InputData,
			ChildOutput:      child.OutputData,
		}, nil
	}
	return nil, fmt.Errorf("timeout waiting for supervisor %s", instanceID)
}

// waitForResult polls for workflow completion and returns the result.
func waitForResult[O any](ctx context.Context, app *romancy.App, instanceID string, timeout time.Duration) (*O, error) {
	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		result, err := romancy.GetWorkflowResult[O](ctx, app, instanceID)
		if err != nil {
			return nil, err
		}

		// Check if workflow completed (success or failure)
		if result.Status == "completed" {
			return &result.Output, nil
		}
		if result.Status == "failed" {
			if result.Error != nil {
				return nil, result.Error
			}
			return nil, fmt.Errorf("workflow failed")
		}

		time.Sleep(pollInterval)
	}

	return nil, fmt.Errorf("timeout waiting for workflow completion")
}

func main() {
	// Parse command-line flags
	dbPath := flag.String("db", "", "SQLite database path (required)")
	channel := flag.String("channel", "cross-test", "Channel name")
	mode := flag.String("mode", "", "Mode: 'subscriber', 'publisher', 'send-to', 'supervisor', 'worker', 'wait-event' or 'post-event' (required)")
	channelMode := flag.String("channel-mode", "broadcast", "Channel mode: 'broadcast', 'competing', or 'direct'")
	messageType := flag.String("message-type", "strict", "Message type: 'strict' (time.Time) or 'compatible' (string timestamp)")
	timeout := flag.Int("timeout", 30, "Timeout in seconds for subscriber, supervisor and wait-event mode; how long worker mode runs")
	message := flag.String("message", "Hello from Go!", "Message text for publisher/send-to mode")
	dataJSON := flag.String("data", "", "Additional JSON data for publisher/send-to mode")
	metadataJSON := flag.String("metadata", "", "JSON metadata for publisher/send-to mode")
	targetInstance := flag.String("target-instance", "", "Target instance ID for send-to mode (required for send-to)")
	workerID := flag.String("worker-id", "go-crosstest-worker", "Worker ID")
	childFramework := flag.String("child-framework", "", "Supervisor mode: runtime that runs the child: 'python', 'ruby' or 'go' (default)")
	childWorkflow := flag.String("child-workflow", "", "Supervisor mode: the child's workflow name (required)")
	childInputJSON := flag.String("child-input", "", "Supervisor mode: the child's input JSON (none: the runtime's empty input)")
	eventType := flag.String("event-type", "", "Wait-event mode: the type of the event to wait for (required)")
	eventJSON := flag.String("event", "", "Post-event mode: the structured CloudEvent JSON to send (required)")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --db is required")
		flag.Usage()
		os.Exit(1)
	}

	switch *mode {
	case "subscriber", "publisher", "send-to", "supervisor", "worker", "wait-event", "post-event":
	default:
		fmt.Fprintln(os.Stderr, "Error: --mode must be 'subscriber', 'publisher', 'send-to', 'supervisor', 'worker', 'wait-event' or 'post-event'")
		flag.Usage()
		os.Exit(1)
	}

	if *mode == "supervisor" && *childWorkflow == "" {
		fmt.Fprintln(os.Stderr, "Error: --child-workflow is required for supervisor mode")
		flag.Usage()
		os.Exit(1)
	}

	if *mode == "wait-event" && *eventType == "" {
		fmt.Fprintln(os.Stderr, "Error: --event-type is required for wait-event mode")
		flag.Usage()
		os.Exit(1)
	}

	if *mode == "post-event" && !json.Valid([]byte(*eventJSON)) {
		fmt.Fprintln(os.Stderr, "Error: --event must be a CloudEvent in JSON for post-event mode")
		flag.Usage()
		os.Exit(1)
	}

	if *mode == "send-to" && *targetInstance == "" {
		fmt.Fprintln(os.Stderr, "Error: --target-instance is required for send-to mode")
		flag.Usage()
		os.Exit(1)
	}

	ctx := context.Background()

	// Create Romancy app with SQLite
	opts := []romancy.Option{
		romancy.WithDatabase(*dbPath),
		romancy.WithWorkerID(*workerID),
	}
	if *mode == "supervisor" || *mode == "worker" || *mode == "wait-event" {
		// A supervisor and its child poll each other, and a waiter that
		// another runtime delivers to is resumed by the poll: keep the
		// loops short
		opts = append(opts,
			romancy.WithWorkflowResumptionInterval(200*time.Millisecond),
			romancy.WithEventTimeoutInterval(200*time.Millisecond),
		)
	}
	app := romancy.NewApp(opts...)

	// Register workflows
	romancy.RegisterWorkflow[SubscriberInput, SubscriberResult](app, subscriberWorkflow)
	romancy.RegisterWorkflow[PublisherInput, PublisherResult](app, publisherWorkflow)
	romancy.RegisterWorkflow[SendToInput, SendToResult](app, sendToWorkflow)
	romancy.RegisterWorkflow[EchoInput, EchoResult](app, goEchoWorkflow)
	romancy.RegisterWorkflow[string, string](app, goStringWorkflow)
	romancy.RegisterWorkflow[WaitEventInput, WaitEventResult](app, waitEventWorkflow)

	var supervisor *romancy.WorkflowFunc[romancy.SupervisorState, romancy.SupervisorResult]
	if *mode == "supervisor" {
		var childInput json.RawMessage
		if *childInputJSON != "" {
			if !json.Valid([]byte(*childInputJSON)) {
				log.Fatalf("Invalid --child-input JSON: %s", *childInputJSON)
			}
			childInput = json.RawMessage(*childInputJSON)
		}
		supervisor = romancy.DefineSupervisor("crosstest_supervisor",
			romancy.SuperviseConfig{Strategy: romancy.OneForOne, MaxRestarts: 3, WithinSeconds: 60, PollSeconds: 1},
			[]romancy.ChildSpec{{
				Name:      "child",
				Workflow:  *childWorkflow,
				Framework: *childFramework,
				Input:     childInput,
				Restart:   romancy.RestartTransient,
			}})
		romancy.RegisterWorkflow(app, supervisor)
	}

	// Start the app
	if err := app.Start(ctx); err != nil {
		log.Fatalf("Failed to start app: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Shutdown(shutdownCtx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	var resultJSON []byte
	var resultErr error

	switch *mode {
	case "subscriber":
		input := SubscriberInput{
			Channel:        *channel,
			TimeoutSeconds: *timeout,
			ChannelMode:    *channelMode,
			MessageType:    *messageType,
		}

		log.Printf("Starting subscriber workflow...")
		instanceID, err := romancy.StartWorkflow(ctx, app, subscriberWorkflow, input)
		if err != nil {
			log.Fatalf("Failed to start subscriber workflow: %v", err)
		}
		log.Printf("Subscriber instance started: %s", instanceID)

		// Wait for completion by polling
		result, err := waitForResult[SubscriberResult](ctx, app, instanceID, time.Duration(*timeout+10)*time.Second)
		if err != nil {
			log.Fatalf("Workflow failed: %v", err)
		}

		resultJSON, resultErr = json.MarshalIndent(result, "", "  ")

	case "publisher":
		// Parse optional data JSON
		var data map[string]interface{}
		if *dataJSON != "" {
			if err := json.Unmarshal([]byte(*dataJSON), &data); err != nil {
				log.Fatalf("Invalid --data JSON: %v", err)
			}
		}

		// Parse optional metadata JSON
		var metadata map[string]any
		if *metadataJSON != "" {
			if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
				log.Fatalf("Invalid --metadata JSON: %v", err)
			}
		}

		input := PublisherInput{
			Channel:  *channel,
			Text:     *message,
			Data:     data,
			Metadata: metadata,
		}

		log.Printf("Starting publisher workflow...")
		instanceID, err := romancy.StartWorkflow(ctx, app, publisherWorkflow, input)
		if err != nil {
			log.Fatalf("Failed to start publisher workflow: %v", err)
		}
		log.Printf("Publisher instance started: %s", instanceID)

		// Wait for completion by polling
		result, err := waitForResult[PublisherResult](ctx, app, instanceID, 30*time.Second)
		if err != nil {
			log.Fatalf("Workflow failed: %v", err)
		}

		resultJSON, resultErr = json.MarshalIndent(result, "", "  ")

	case "send-to":
		// Parse optional data JSON
		var data map[string]interface{}
		if *dataJSON != "" {
			if err := json.Unmarshal([]byte(*dataJSON), &data); err != nil {
				log.Fatalf("Invalid --data JSON: %v", err)
			}
		}

		// Parse optional metadata JSON
		var metadata map[string]any
		if *metadataJSON != "" {
			if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
				log.Fatalf("Invalid --metadata JSON: %v", err)
			}
		}

		input := SendToInput{
			TargetInstanceID: *targetInstance,
			Channel:          *channel,
			Text:             *message,
			Data:             data,
			Metadata:         metadata,
		}

		log.Printf("Starting send-to workflow...")
		instanceID, err := romancy.StartWorkflow(ctx, app, sendToWorkflow, input)
		if err != nil {
			log.Fatalf("Failed to start send-to workflow: %v", err)
		}
		log.Printf("Send-to instance started: %s", instanceID)

		// Wait for completion by polling
		result, err := waitForResult[SendToResult](ctx, app, instanceID, 30*time.Second)
		if err != nil {
			log.Fatalf("Workflow failed: %v", err)
		}

		resultJSON, resultErr = json.MarshalIndent(result, "", "  ")

	case "supervisor":
		log.Printf("Starting supervisor with a %q child %s...", *childFramework, *childWorkflow)
		instanceID, err := romancy.StartWorkflow(ctx, app, supervisor, romancy.SupervisorState{})
		if err != nil {
			log.Fatalf("Failed to start supervisor: %v", err)
		}
		log.Printf("Supervisor instance started: %s", instanceID)

		result, err := waitForSupervision(ctx, app, instanceID, time.Duration(*timeout)*time.Second)
		if err != nil {
			log.Fatalf("Supervisor did not end: %v", err)
		}

		resultJSON, resultErr = json.MarshalIndent(result, "", "  ")

	case "wait-event":
		log.Printf("Starting wait-event workflow...")
		input := WaitEventInput{EventType: *eventType, TimeoutSeconds: *timeout}
		instanceID, err := romancy.StartWorkflow(ctx, app, waitEventWorkflow, input)
		if err != nil {
			log.Fatalf("Failed to start wait-event workflow: %v", err)
		}
		log.Printf("Wait-event instance started: %s", instanceID)

		result, err := waitForResult[WaitEventResult](ctx, app, instanceID, time.Duration(*timeout+10)*time.Second)
		if err != nil {
			log.Fatalf("Workflow failed: %v", err)
		}

		resultJSON, resultErr = json.MarshalIndent(result, "", "  ")

	case "post-event":
		// The endpoint stores the event and wakes the workflows waiting for
		// it, whatever runtime runs them. Shutdown waits for that work.
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(*eventJSON))
		req.Header.Set("Content-Type", "application/cloudevents+json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		log.Printf("The endpoint answered %d", rec.Code)

		result := PostEventResult{Status: rec.Code}
		if rec.Code != http.StatusAccepted {
			result.Body = rec.Body.String()
		}
		resultJSON, resultErr = json.MarshalIndent(result, "", "  ")

	case "worker":
		// Print one line once the app is running, then serve
		// until the timeout or a signal.
		fmt.Println(`{"ready": true}`)
		sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		select {
		case <-sigCtx.Done():
		case <-time.After(time.Duration(*timeout) * time.Second):
		}
		return
	}

	if resultErr != nil {
		log.Fatalf("Failed to marshal result: %v", resultErr)
	}

	// Print result as JSON to stdout
	fmt.Println(string(resultJSON))
}
