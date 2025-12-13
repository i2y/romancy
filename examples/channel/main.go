// Package main demonstrates channel-based message passing between workflows.
//
// This example shows:
// - Subscribe/Receive for receiving messages
// - Publish for broadcasting messages
// - SendTo for direct messaging to specific workflow instances
// - ModeBroadcast vs ModeCompeting delivery modes
//
// Usage:
//
//	go run ./examples/channel/
//
// Then in separate terminals:
//
//  1. Start a consumer (broadcast mode):
//     curl -X POST http://localhost:8084/start-consumer \
//     -H "Content-Type: application/json" \
//     -d '{"consumer_id": "consumer-1", "mode": "broadcast"}'
//
//  2. Start another consumer:
//     curl -X POST http://localhost:8084/start-consumer \
//     -H "Content-Type: application/json" \
//     -d '{"consumer_id": "consumer-2", "mode": "broadcast"}'
//
//  3. Send a message to all consumers:
//     curl -X POST http://localhost:8084/publish \
//     -H "Content-Type: application/json" \
//     -d '{"channel": "notifications", "message": "Hello everyone!"}'
//
//  4. Send a direct message to a specific consumer:
//     curl -X POST http://localhost:8084/send-to \
//     -H "Content-Type: application/json" \
//     -d '{"instance_id": "<workflow-instance-id>", "channel": "notifications", "message": "Private message"}'
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
	Mode       string `json:"mode"` // "broadcast" or "competing"
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

// ----- Workflows -----

// consumerWorkflow subscribes to a channel and receives messages.
var consumerWorkflow = romancy.DefineWorkflow("channel_consumer",
	func(ctx *romancy.WorkflowContext, input ConsumerInput) (ConsumerResult, error) {
		log.Printf("[Consumer %s] Starting consumer workflow", input.ConsumerID)

		result := ConsumerResult{
			ConsumerID:       input.ConsumerID,
			MessagesReceived: []string{},
			Status:           "running",
		}

		// Determine channel mode
		mode := romancy.ModeBroadcast
		if input.Mode == "competing" {
			mode = romancy.ModeCompeting
		}

		// Subscribe to the notifications channel
		channelName := "notifications"
		log.Printf("[Consumer %s] Subscribing to channel '%s' in %s mode", input.ConsumerID, channelName, input.Mode)
		if err := romancy.Subscribe(ctx, channelName, mode); err != nil {
			result.Status = "subscription_failed"
			return result, fmt.Errorf("failed to subscribe: %w", err)
		}

		log.Printf("[Consumer %s] Subscribed! Draining all pending messages...", input.ConsumerID)
		log.Printf("[Consumer %s] Instance ID: %s", input.ConsumerID, ctx.InstanceID())

		// Drain all pending messages with a short timeout (2 seconds)
		// This will consume all existing messages until the channel is empty
		for {
			log.Printf("[Consumer %s] Waiting for message (count: %d)...", input.ConsumerID, len(result.MessagesReceived))

			// Wait for a message with 2-second timeout to detect empty channel
			msg, err := romancy.Receive[ChatMessage](ctx, channelName, romancy.WithReceiveTimeout(2*time.Second))
			if err != nil {
				// Check if this is a suspend signal (normal workflow pause)
				if romancy.IsSuspendSignal(err) {
					return result, err
				}
				// Check for timeout error - channel is empty
				var timeoutErr *romancy.ChannelMessageTimeoutError
				if errors.As(err, &timeoutErr) {
					log.Printf("[Consumer %s] Channel empty (timeout), finished draining", input.ConsumerID)
					result.Status = "drained"
					break
				}
				result.Status = "receive_failed"
				return result, fmt.Errorf("failed to receive message: %w", err)
			}

			log.Printf("[Consumer %s] Received message from %s: %s", input.ConsumerID, msg.Data.From, msg.Data.Content)
			result.MessagesReceived = append(result.MessagesReceived, fmt.Sprintf("%s: %s", msg.Data.From, msg.Data.Content))
		}

		log.Printf("[Consumer %s] Finished draining %d messages", input.ConsumerID, len(result.MessagesReceived))
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
	log.Println("This example demonstrates channel-based message passing.")
	log.Println("")
	log.Println("API Endpoints:")
	log.Println("")
	log.Println("1. Start a consumer (subscribes to channel):")
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer \`)
	fmt.Println(`     -H "Content-Type: application/json" \`)
	fmt.Println(`     -d '{"consumer_id": "alice", "mode": "broadcast"}'`)
	log.Println("")
	log.Println("2. Start another consumer:")
	fmt.Println(`curl -X POST http://localhost:8084/start-consumer \`)
	fmt.Println(`     -H "Content-Type: application/json" \`)
	fmt.Println(`     -d '{"consumer_id": "bob", "mode": "broadcast"}'`)
	log.Println("")
	log.Println("3. Publish a message (all consumers receive it):")
	fmt.Println(`curl -X POST http://localhost:8084/publish \`)
	fmt.Println(`     -H "Content-Type: application/json" \`)
	fmt.Println(`     -d '{"channel": "notifications", "message": "Hello everyone!", "from": "system"}'`)
	log.Println("")
	log.Println("4. Start a producer (sends multiple messages):")
	fmt.Println(`curl -X POST http://localhost:8084/start-producer \`)
	fmt.Println(`     -H "Content-Type: application/json" \`)
	fmt.Println(`     -d '{"producer_id": "bot", "messages": ["msg1", "msg2", "exit"], "channel": "notifications"}'`)
	log.Println("")
	log.Println("==============================================")

	// Setup custom HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/start-consumer", StartConsumerHandler)
	mux.HandleFunc("/start-producer", StartProducerHandler)
	mux.HandleFunc("/publish", PublishHandler)
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
