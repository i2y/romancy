// Package main demonstrates channel-based message passing between workflows.
//
// This example shows three delivery modes:
// - Broadcast mode: All consumers receive all messages (fan-out)
// - Competing mode: Each message is delivered to exactly one consumer (load balancing)
// - Direct mode: SendTo delivers messages to a specific workflow instance
//
// Usage:
//
//	go run ./examples/channel/
//
// API Endpoints:
//
//  1. Start a broadcast consumer (all receive all messages):
//     curl -X POST http://localhost:8084/start-consumer \
//     -H "Content-Type: application/json" \
//     -d '{"consumer_id": "alice", "mode": "broadcast"}'
//
//  2. Start a competing consumer (each message goes to one):
//     curl -X POST http://localhost:8084/start-consumer \
//     -H "Content-Type: application/json" \
//     -d '{"consumer_id": "worker-1", "mode": "competing"}'
//
//  3. Start a direct message receiver (for SendTo):
//     curl -X POST http://localhost:8084/start-consumer \
//     -H "Content-Type: application/json" \
//     -d '{"consumer_id": "bob", "mode": "direct"}'
//     # Note the instance_id in the response for SendTo
//
//  4. Publish a message (broadcast/competing consumers receive it):
//     curl -X POST http://localhost:8084/publish \
//     -H "Content-Type: application/json" \
//     -d '{"channel": "notifications", "message": "Hello everyone!", "from": "system"}'
//
//  5. Send a direct message to a specific instance:
//     curl -X POST http://localhost:8084/send-to \
//     -H "Content-Type: application/json" \
//     -d '{"instance_id": "<instance-id-from-step-3>", "channel": "notifications", "message": "Private message"}'
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/hooks/otel"
)

// ----- Types -----

// ConsumerInput is the input for the consumer workflow.
type ConsumerInput struct {
	ConsumerID string `json:"consumer_id"`
	Mode       string `json:"mode"` // "broadcast", "competing", or "direct"
}

// ConsumerResult is the output of the consumer workflow.
type ConsumerResult struct {
	ConsumerID       string   `json:"consumer_id"`
	MessagesReceived []string `json:"messages_received"`
	Status           string   `json:"status"`
}

// ProducerInput is the input for the producer workflow.
type ProducerInput struct {
	ProducerID string   `json:"producer_id"`
	Messages   []string `json:"messages"`
	Channel    string   `json:"channel"`
}

// ProducerResult is the output of the producer workflow.
type ProducerResult struct {
	ProducerID   string `json:"producer_id"`
	MessagesSent int    `json:"messages_sent"`
	Status       string `json:"status"`
}

// ChatMessage represents a message in the channel.
type ChatMessage struct {
	From      string    `json:"from"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// SendToInput is the input for the send-to workflow.
type SendToInput struct {
	TargetInstanceID string `json:"target_instance_id"`
	Channel          string `json:"channel"`
	Message          string `json:"message"`
	From             string `json:"from"`
}

// SendToResult is the output of the send-to workflow.
type SendToResult struct {
	Status string `json:"status"`
}

// ----- Workflows -----

// consumerWorkflow subscribes to a channel and receives messages.
// Supports three modes:
// - "broadcast": All consumers receive all messages (fan-out)
// - "competing": Each message goes to exactly one consumer (load balancing)
// - "direct": Receives SendTo messages sent to this specific instance
var consumerWorkflow = romancy.DefineWorkflow("channel_consumer",
	func(ctx *romancy.WorkflowContext, input ConsumerInput) (ConsumerResult, error) {
		log.Printf("[Consumer %s] Starting consumer workflow (mode: %s)", input.ConsumerID, input.Mode)

		result := ConsumerResult{
			ConsumerID:       input.ConsumerID,
			MessagesReceived: []string{},
			Status:           "running",
		}

		// Determine channel name and subscription mode based on input.Mode
		var channelName string
		var subscriptionMode romancy.ChannelMode

		switch input.Mode {
		case "competing":
			// Competing mode: subscribe to shared channel, each message goes to one consumer
			channelName = "notifications"
			subscriptionMode = romancy.ModeCompeting
			log.Printf("[Consumer %s] Subscribing to channel '%s' in COMPETING mode", input.ConsumerID, channelName)

		case "direct":
			// Direct mode: use ModeDirect to receive SendTo messages
			// ModeDirect automatically subscribes to "notifications:instanceID"
			channelName = "notifications"
			subscriptionMode = romancy.ModeDirect
			log.Printf("[Consumer %s] Subscribing to channel '%s' in DIRECT mode", input.ConsumerID, channelName)
			log.Printf("[Consumer %s] Instance ID for SendTo: %s", input.ConsumerID, ctx.InstanceID())

		default: // "broadcast" or empty
			// Broadcast mode: all consumers receive all messages
			channelName = "notifications"
			subscriptionMode = romancy.ModeBroadcast
			log.Printf("[Consumer %s] Subscribing to channel '%s' in BROADCAST mode", input.ConsumerID, channelName)
		}

		// Subscribe to the channel
		if err := romancy.Subscribe(ctx, channelName, subscriptionMode); err != nil {
			result.Status = "subscription_failed"
			return result, fmt.Errorf("failed to subscribe: %w", err)
		}

		log.Printf("[Consumer %s] Subscribed! Waiting for messages...", input.ConsumerID)

		// Receive messages with timeout
		// Uses short timeout to drain pending messages, then exits
		emptyCount := 0
		maxEmptyCount := 1 // Exit after 1 timeout (no pending messages)

		for emptyCount < maxEmptyCount {
			log.Printf("[Consumer %s] Waiting for message (received: %d)...", input.ConsumerID, len(result.MessagesReceived))

			msg, err := romancy.Receive[ChatMessage](ctx, channelName, romancy.WithReceiveTimeout(2*time.Second))
			if err == nil {
				prefix := ""
				if input.Mode == "direct" {
					prefix = "[DIRECT] "
				}
				log.Printf("[Consumer %s] %sReceived message from %s: %s", input.ConsumerID, prefix, msg.Data.From, msg.Data.Content)
				result.MessagesReceived = append(result.MessagesReceived, fmt.Sprintf("%s%s: %s", prefix, msg.Data.From, msg.Data.Content))
				emptyCount = 0
				continue
			}

			// Check for suspend signal (workflow needs to pause)
			if romancy.IsSuspendSignal(err) {
				return result, err
			}

			// Check for timeout
			var timeoutErr *romancy.ChannelMessageTimeoutError
			if errors.As(err, &timeoutErr) {
				emptyCount++
				continue
			}

			// Other error
			result.Status = "receive_failed"
			return result, fmt.Errorf("failed to receive message: %w", err)
		}

		log.Printf("[Consumer %s] No more pending messages, finished with %d messages", input.ConsumerID, len(result.MessagesReceived))
		result.Status = "drained"
		return result, nil
	},
)

// producerWorkflow sends multiple messages to a channel.
var producerWorkflow = romancy.DefineWorkflow("channel_producer",
	func(ctx *romancy.WorkflowContext, input ProducerInput) (ProducerResult, error) {
		log.Printf("[Producer %s] Starting producer workflow", input.ProducerID)

		result := ProducerResult{
			ProducerID: input.ProducerID,
			Status:     "running",
		}

		channelName := input.Channel
		if channelName == "" {
			channelName = "notifications"
		}

		// Subscribe to receive replies (optional, for bi-directional communication)
		if err := romancy.Subscribe(ctx, "replies", romancy.ModeBroadcast); err != nil {
			log.Printf("[Producer %s] Warning: failed to subscribe to replies: %v", input.ProducerID, err)
		}

		// Send all messages
		for i, content := range input.Messages {
			msg := ChatMessage{
				From:      input.ProducerID,
				Content:   content,
				Timestamp: time.Now(),
			}

			log.Printf("[Producer %s] Publishing message %d/%d: %s", input.ProducerID, i+1, len(input.Messages), content)
			if err := romancy.Publish(ctx, channelName, msg); err != nil {
				result.Status = "publish_failed"
				return result, fmt.Errorf("failed to publish message: %w", err)
			}
			result.MessagesSent++

			// Small delay between messages
			time.Sleep(100 * time.Millisecond)
		}

		result.Status = "completed"
		log.Printf("[Producer %s] Finished sending %d messages", input.ProducerID, result.MessagesSent)
		return result, nil
	},
)

// sendToWorkflow sends a direct message to a specific workflow instance.
var sendToWorkflow = romancy.DefineWorkflow("channel_send_to",
	func(ctx *romancy.WorkflowContext, input SendToInput) (SendToResult, error) {
		log.Printf("[SendTo] Sending direct message to %s on channel '%s'", input.TargetInstanceID, input.Channel)

		msg := ChatMessage{
			From:      input.From,
			Content:   input.Message,
			Timestamp: time.Now(),
		}

		if err := romancy.SendTo(ctx, input.TargetInstanceID, input.Channel, msg); err != nil {
			return SendToResult{Status: "failed"}, fmt.Errorf("failed to send: %w", err)
		}

		log.Printf("[SendTo] Message sent successfully to %s", input.TargetInstanceID)
		return SendToResult{Status: "sent"}, nil
	},
)

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-channel"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(10*time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)), sdktrace.WithResource(res))
	return tp, nil
}

// ----- HTTP Handlers -----

var app *romancy.App

// StartConsumerHandler starts a new consumer workflow.
func StartConsumerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input ConsumerInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if input.ConsumerID == "" {
		input.ConsumerID = fmt.Sprintf("consumer-%d", time.Now().UnixNano())
	}
	if input.Mode == "" {
		input.Mode = "broadcast"
	}

	instanceID, err := romancy.StartWorkflow(r.Context(), app, consumerWorkflow, input)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to start consumer: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP] Started consumer workflow: %s (instance: %s)", input.ConsumerID, instanceID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":      "started",
		"consumer_id": input.ConsumerID,
		"instance_id": instanceID,
		"mode":        input.Mode,
	})
}

// StartProducerHandler starts a new producer workflow.
func StartProducerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input ProducerInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if input.ProducerID == "" {
		input.ProducerID = fmt.Sprintf("producer-%d", time.Now().UnixNano())
	}
	if input.Channel == "" {
		input.Channel = "notifications"
	}

	instanceID, err := romancy.StartWorkflow(r.Context(), app, producerWorkflow, input)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to start producer: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP] Started producer workflow: %s (instance: %s)", input.ProducerID, instanceID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":      "started",
		"producer_id": input.ProducerID,
		"instance_id": instanceID,
	})
}

// PublishHandler publishes a message to a channel (outside of workflow context).
func PublishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Channel string `json:"channel"`
		Message string `json:"message"`
		From    string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if req.Channel == "" {
		req.Channel = "notifications"
	}
	if req.From == "" {
		req.From = "http-client"
	}

	// Start a simple producer workflow to send the message
	input := ProducerInput{
		ProducerID: req.From,
		Messages:   []string{req.Message},
		Channel:    req.Channel,
	}

	instanceID, err := romancy.StartWorkflow(r.Context(), app, producerWorkflow, input)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to publish: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP] Published message to channel '%s': %s", req.Channel, req.Message)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":      "published",
		"channel":     req.Channel,
		"message":     req.Message,
		"instance_id": instanceID,
	})
}

// SendToHandler sends a direct message to a specific workflow instance.
func SendToHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		InstanceID string `json:"instance_id"`
		Channel    string `json:"channel"`
		Message    string `json:"message"`
		From       string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if req.InstanceID == "" {
		http.Error(w, "instance_id is required", http.StatusBadRequest)
		return
	}
	if req.Channel == "" {
		req.Channel = "notifications"
	}
	if req.From == "" {
		req.From = "http-client"
	}

	input := SendToInput{
		TargetInstanceID: req.InstanceID,
		Channel:          req.Channel,
		Message:          req.Message,
		From:             req.From,
	}

	_, err := romancy.StartWorkflow(r.Context(), app, sendToWorkflow, input)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to send: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP] Sent direct message to instance '%s' on channel '%s': %s", req.InstanceID, req.Channel, req.Message)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":      "sent",
		"instance_id": req.InstanceID,
		"channel":     req.Channel,
		"message":     req.Message,
	})
}

// ----- Main -----

func main() {
	ctx := context.Background()

	// Setup OpenTelemetry tracing
	tp, err := setupTracing(ctx)
	if err != nil {
		log.Printf("Warning: Failed to setup tracing: %v", err)
	}
	if tp != nil {
		defer func() {
			if err := tp.Shutdown(ctx); err != nil {
				log.Printf("Error shutting down tracer: %v", err)
			}
		}()
	}

	// Get database URL from environment or use default
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "channel_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbURL),
		romancy.WithWorkerID("channel-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app = romancy.NewApp(opts...)

	// Register workflows
	romancy.RegisterWorkflow[ConsumerInput, ConsumerResult](app, consumerWorkflow)
	romancy.RegisterWorkflow[ProducerInput, ProducerResult](app, producerWorkflow)
	romancy.RegisterWorkflow[SendToInput, SendToResult](app, sendToWorkflow)

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}

	log.Println("==============================================")
	log.Println("Channel Message Passing Example")
	log.Printf("Database: %s", dbURL)
	log.Println("==============================================")
	log.Println("")
	log.Println("This example demonstrates three channel delivery modes:")
	log.Println("- BROADCAST: All consumers receive all messages")
	log.Println("- COMPETING: Each message goes to exactly one consumer")
	log.Println("- DIRECT: SendTo delivers to a specific instance")
	log.Println("")
	log.Println("=== BROADCAST MODE ===")
	log.Println("Start two broadcast consumers:")
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer -H "Content-Type: application/json" -d '{"consumer_id": "alice", "mode": "broadcast"}'`)
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer -H "Content-Type: application/json" -d '{"consumer_id": "bob", "mode": "broadcast"}'`)
	log.Println("Publish a message (both alice and bob receive it):")
	fmt.Println(`curl -X POST http://localhost:8084/publish -H "Content-Type: application/json" -d '{"channel": "notifications", "message": "Hello everyone!", "from": "system"}'`)
	log.Println("")
	log.Println("=== COMPETING MODE ===")
	log.Println("Start three competing consumers:")
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer -H "Content-Type: application/json" -d '{"consumer_id": "w1", "mode": "competing"}'`)
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer -H "Content-Type: application/json" -d '{"consumer_id": "w2", "mode": "competing"}'`)
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer -H "Content-Type: application/json" -d '{"consumer_id": "w3", "mode": "competing"}'`)
	log.Println("Publish 3 messages (each worker receives exactly one):")
	fmt.Println(`curl -X POST http://localhost:8084/start-producer -H "Content-Type: application/json" -d '{"producer_id": "bot", "messages": ["Task-1", "Task-2", "Task-3"], "channel": "notifications"}'`)
	log.Println("")
	log.Println("=== DIRECT MODE (SendTo) ===")
	log.Println("Start a direct message receiver (note the instance_id in response):")
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer -H "Content-Type: application/json" -d '{"consumer_id": "bob", "mode": "direct"}'`)
	log.Println("Send a direct message to that instance:")
	fmt.Println(`curl -X POST http://localhost:8084/send-to -H "Content-Type: application/json" -d '{"instance_id": "<INSTANCE_ID>", "channel": "notifications", "message": "Secret message for Bob", "from": "alice"}'`)
	log.Println("")
	log.Println("==============================================")

	// Setup custom HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/start-consumer", StartConsumerHandler)
	mux.HandleFunc("/start-producer", StartProducerHandler)
	mux.HandleFunc("/publish", PublishHandler)
	mux.HandleFunc("/send-to", SendToHandler)
	// Also mount the romancy handler for CloudEvents
	mux.Handle("/", app.Handler())

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in goroutine
	server := &http.Server{Addr: ":8084", Handler: mux}
	go func() {
		log.Println("HTTP server running on :8084")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Println("Press Ctrl+C to stop")

	// Wait for signal
	<-sigCh
	log.Println("\nShutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	if err := app.Shutdown(shutdownCtx); err != nil {
		log.Printf("App shutdown error: %v", err)
	}

	log.Println("Goodbye!")
}
