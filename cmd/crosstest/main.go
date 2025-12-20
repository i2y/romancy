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
//nolint:gocritic // exitAfterDefer is acceptable for CLI tools using log.Fatalf
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
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
	mode := flag.String("mode", "", "Mode: 'subscriber', 'publisher', or 'send-to' (required)")
	channelMode := flag.String("channel-mode", "broadcast", "Channel mode: 'broadcast', 'competing', or 'direct'")
	messageType := flag.String("message-type", "strict", "Message type: 'strict' (time.Time) or 'compatible' (string timestamp)")
	timeout := flag.Int("timeout", 30, "Timeout in seconds for subscriber mode")
	message := flag.String("message", "Hello from Go!", "Message text for publisher/send-to mode")
	dataJSON := flag.String("data", "", "Additional JSON data for publisher/send-to mode")
	metadataJSON := flag.String("metadata", "", "JSON metadata for publisher/send-to mode")
	targetInstance := flag.String("target-instance", "", "Target instance ID for send-to mode (required for send-to)")
	workerID := flag.String("worker-id", "go-crosstest-worker", "Worker ID")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --db is required")
		flag.Usage()
		os.Exit(1)
	}

	if *mode != "subscriber" && *mode != "publisher" && *mode != "send-to" {
		fmt.Fprintln(os.Stderr, "Error: --mode must be 'subscriber', 'publisher', or 'send-to'")
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
	app := romancy.NewApp(
		romancy.WithDatabase(*dbPath),
		romancy.WithWorkerID(*workerID),
	)

	// Register workflows
	romancy.RegisterWorkflow[SubscriberInput, SubscriberResult](app, subscriberWorkflow)
	romancy.RegisterWorkflow[PublisherInput, PublisherResult](app, publisherWorkflow)
	romancy.RegisterWorkflow[SendToInput, SendToResult](app, sendToWorkflow)

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
	}

	if resultErr != nil {
		log.Fatalf("Failed to marshal result: %v", resultErr)
	}

	// Print result as JSON to stdout
	fmt.Println(string(resultJSON))
}
