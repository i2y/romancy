// Package main demonstrates a simple workflow without events.
//
// This example shows:
// - Basic workflow and activity definitions
// - Sequential activity execution
// - Type-safe inputs and outputs
//
// Usage:
//
//	go run ./examples/simple/
package main

import (
	"context"
	"fmt"
	"log"
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

// GreetingInput is the input for the greeting workflow.
type GreetingInput struct {
	Name     string `json:"name"`
	Language string `json:"language"` // "en", "ja", "es"
}

// GreetingResult is the output of the greeting workflow.
type GreetingResult struct {
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// ----- Activities -----

// GetGreeting returns a greeting message based on the language.
var GetGreeting = romancy.DefineActivity(
	"get_greeting",
	func(ctx context.Context, language string) (string, error) {
		log.Printf("[Activity] Getting greeting for language: %s", language)
		time.Sleep(50 * time.Millisecond) // Simulate work

		greetings := map[string]string{
			"en": "Hello",
			"ja": "こんにちは",
			"es": "Hola",
			"fr": "Bonjour",
			"de": "Hallo",
		}

		greeting, ok := greetings[language]
		if !ok {
			greeting = "Hello" // Default to English
		}

		return greeting, nil
	},
)

// FormatMessage formats the greeting with the name.
var FormatMessage = romancy.DefineActivity(
	"format_message",
	func(ctx context.Context, input struct {
		Greeting string
		Name     string
	}) (string, error) {
		log.Printf("[Activity] Formatting message for: %s", input.Name)
		time.Sleep(50 * time.Millisecond) // Simulate work

		message := fmt.Sprintf("%s, %s!", input.Greeting, input.Name)
		return message, nil
	},
)

// ----- Workflow -----

// GreetingWorkflow creates a personalized greeting.
type GreetingWorkflow struct{}

func (w *GreetingWorkflow) Name() string { return "greeting_workflow" }

func (w *GreetingWorkflow) Execute(ctx *romancy.WorkflowContext, input GreetingInput) (GreetingResult, error) {
	log.Printf("[Workflow] Starting greeting workflow for: %s", input.Name)

	// Step 1: Get the greeting based on language
	greeting, err := GetGreeting.Execute(ctx, input.Language)
	if err != nil {
		return GreetingResult{}, fmt.Errorf("failed to get greeting: %w", err)
	}

	// Step 2: Format the message with the name
	message, err := FormatMessage.Execute(ctx, struct {
		Greeting string
		Name     string
	}{
		Greeting: greeting,
		Name:     input.Name,
	})
	if err != nil {
		return GreetingResult{}, fmt.Errorf("failed to format message: %w", err)
	}

	result := GreetingResult{
		Message:   message,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	log.Printf("[Workflow] Completed: %s", result.Message)
	return result, nil
}

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil // No tracing configured
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-simple"
	}

	// Note: otlptracegrpc automatically reads OTEL_EXPORTER_OTLP_ENDPOINT env var
	// The endpoint should be host:port without scheme (e.g., "localhost:4317")
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(10*time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
	)
	return tp, nil
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
			// Give time for traces to be exported before shutdown
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tp.Shutdown(shutdownCtx); err != nil {
				log.Printf("Error shutting down tracer: %v", err)
			}
		}()
	}

	// Get database URL from environment or use default
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "simple_demo.db"
	}
	log.Printf("Using DATABASE_URL: %s", dbURL)

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbURL),
		romancy.WithWorkerID("simple-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register the workflow
	romancy.RegisterWorkflow[GreetingInput, GreetingResult](app, &GreetingWorkflow{})

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}
	defer func() {
		if err := app.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Println("==============================================")
	log.Println("Simple Workflow Example")
	log.Println("==============================================")

	// Run workflows with different languages
	languages := []struct {
		name string
		lang string
	}{
		{"Alice", "en"},
		{"Taro", "ja"},
		{"Carlos", "es"},
	}

	for _, l := range languages {
		input := GreetingInput{
			Name:     l.name,
			Language: l.lang,
		}

		log.Printf("\n--- Starting workflow for %s (%s) ---", l.name, l.lang)

		instanceID, err := romancy.StartWorkflow(ctx, app, &GreetingWorkflow{}, input)
		if err != nil {
			log.Printf("Failed to start workflow: %v", err)
			continue
		}

		log.Printf("Instance ID: %s", instanceID)

		// Wait a bit for the workflow to complete
		time.Sleep(500 * time.Millisecond)

		// Get the result
		result, err := romancy.GetWorkflowResult[GreetingResult](ctx, app, instanceID)
		if err != nil {
			log.Printf("Failed to get result: %v", err)
			continue
		}

		log.Printf("Result: %s", result.Output.Message)
	}

	log.Println("\n==============================================")
	log.Println("All workflows completed!")
	log.Println("==============================================")

	// Force flush traces before shutdown
	if tp != nil {
		log.Println("Flushing traces...")
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := tp.ForceFlush(flushCtx); err != nil {
			log.Printf("Error flushing traces: %v", err)
		}
		cancel()
	}

	// In serve mode (for Tilt), wait for shutdown signal instead of exiting
	if os.Getenv("ROMANCY_SERVE") == "true" {
		log.Println("\nRunning in serve mode. Press Ctrl+C to stop.")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("\nShutting down...")
	}
}
